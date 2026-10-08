package ocimiddleware

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"sync"

	"github.com/ohseeeye/oci"
)

// CacheOptions configures the cache wrapper. Expiration belongs to the cache backend.
type CacheOptions struct {
	// CacheTags enables cached tag lookups. Default false: every tag is resolved
	// upstream. Enabling it with a backend without expiration caches tags indefinitely.
	CacheTags bool
	// TempDir holds temporary blob downloads. Empty uses the system temporary directory.
	TempDir string
	// OnError receives cache population and invalidation errors. Such errors do not
	// fail successful upstream operations. It may be called concurrently.
	OnError func(error)
}

type cacheRegistry struct {
	oci.Registry
	cache   oci.Registry
	opts    CacheOptions
	mu      sync.Mutex
	flights map[cacheKey]chan struct{}
	repos   map[string]*cacheRepoState
}

type cacheKey struct{ repo, kind, value string }
type cacheRepoState struct {
	mu         sync.Mutex
	generation uint64
	invalid    map[cacheKey]bool
}

var _ oci.Registry = (*cacheRegistry)(nil)
var _ oci.TagHistory = (*cacheRegistry)(nil)

// Cache wraps upstream with a read-through cache. The cache must accept
// manifests before their children arrive, for example using AllowSparseManifests
// on our backends. Writes and listings go to the authoritative upstream.
//
// Dedicate cache to this upstream and authorization domain. Access checks must
// wrap the returned registry so they also apply to cache hits. The caller owns
// both backends.
func Cache(upstream, cache oci.Registry, opts *CacheOptions) (oci.Registry, error) {
	if upstream == nil || cache == nil {
		return nil, fmt.Errorf("upstream and cache must be non-nil")
	}
	r := &cacheRegistry{Registry: upstream, cache: cache, flights: make(map[cacheKey]chan struct{}), repos: make(map[string]*cacheRepoState)}
	if opts != nil {
		r.opts = *opts
	}
	return r, nil
}

func cacheMissing(err error) bool {
	return errors.Is(err, oci.ErrNameUnknown) || errors.Is(err, oci.ErrManifestUnknown) || errors.Is(err, oci.ErrBlobUnknown)
}

func (r *cacheRegistry) state(repo string) *cacheRepoState {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.repos[repo]
	if s == nil {
		s = &cacheRepoState{invalid: make(map[cacheKey]bool)}
		r.repos[repo] = s
	}
	return s
}

func (r *cacheRegistry) usable(k cacheKey) bool {
	s := r.state(k.repo)
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.invalid[k]
}

func (r *cacheRegistry) report(err error) {
	if err != nil && r.opts.OnError != nil {
		r.opts.OnError(err)
	}
}

// A miss owns its flight until the download is consumed or closed. Waiters
// recheck the cache after release and can cancel without canceling its owner.
func (r *cacheRegistry) acquire(ctx context.Context, k cacheKey) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r.mu.Lock()
		ch := r.flights[k]
		if ch == nil {
			ch = make(chan struct{})
			r.flights[k] = ch
			r.mu.Unlock()
			var once sync.Once
			return func() { once.Do(func() { r.mu.Lock(); delete(r.flights, k); close(ch); r.mu.Unlock() }) }, nil
		}
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ch:
		}
	}
}

// ResolveBlob checks cached content before resolving upstream; a miss does not fetch the body.
func (r *cacheRegistry) ResolveBlob(ctx context.Context, repo string, digest oci.Digest) (oci.Descriptor, error) {
	if err := ctx.Err(); err != nil {
		return oci.Descriptor{}, err
	}
	if r.usable(cacheKey{repo, "blob", digest.String()}) {
		desc, err := r.cache.ResolveBlob(ctx, repo, digest)
		if !cacheMissing(err) {
			return desc, err
		}
	}
	return r.Registry.ResolveBlob(ctx, repo, digest)
}

// ResolveManifest checks cached content before resolving upstream; a miss does not fetch the body.
func (r *cacheRegistry) ResolveManifest(ctx context.Context, repo string, digest oci.Digest) (oci.Descriptor, error) {
	if err := ctx.Err(); err != nil {
		return oci.Descriptor{}, err
	}
	if r.usable(cacheKey{repo, "manifest", digest.String()}) {
		desc, err := r.cache.ResolveManifest(ctx, repo, digest)
		if !cacheMissing(err) {
			return desc, err
		}
	}
	return r.Registry.ResolveManifest(ctx, repo, digest)
}

// ResolveTag resolves upstream unless CacheTags permits a cached assignment.
func (r *cacheRegistry) ResolveTag(ctx context.Context, repo, tag string) (oci.Descriptor, error) {
	if err := ctx.Err(); err != nil {
		return oci.Descriptor{}, err
	}
	if !r.opts.CacheTags {
		return r.Registry.ResolveTag(ctx, repo, tag)
	}
	release, err := r.acquire(ctx, cacheKey{repo, "tag", tag})
	if err != nil {
		return oci.Descriptor{}, err
	}
	defer release()
	if r.usable(cacheKey{repo, "tag", tag}) {
		desc, err := r.cache.ResolveTag(ctx, repo, tag)
		if err != nil && !cacheMissing(err) {
			return oci.Descriptor{}, err
		}
		if err == nil && r.usable(cacheKey{repo, "manifest", desc.Digest.String()}) {
			return desc, nil
		}
	}
	s := r.state(repo)
	s.mu.Lock()
	generation := s.generation
	s.mu.Unlock()
	desc, err := r.Registry.ResolveTag(ctx, repo, tag)
	if err != nil {
		return oci.Descriptor{}, err
	}
	// Registry has no tag-only write. Populate its manifest to assign this tag.
	reader, err := r.manifest(ctx, repo, desc.Digest, tag, generation)
	if err != nil {
		r.report(err)
	} else {
		_ = reader.Close()
	}
	return desc, nil
}

// GetTag resolves the tag according to CacheTags, then reads its manifest by digest.
func (r *cacheRegistry) GetTag(ctx context.Context, repo, tag string) (oci.BlobReader, error) {
	desc, err := r.ResolveTag(ctx, repo, tag)
	if err != nil {
		return nil, err
	}
	return r.GetManifest(ctx, repo, desc.Digest)
}

// GetManifest reads by digest and caches verified upstream content on a miss.
func (r *cacheRegistry) GetManifest(ctx context.Context, repo string, digest oci.Digest) (oci.BlobReader, error) {
	s := r.state(repo)
	s.mu.Lock()
	generation := s.generation
	s.mu.Unlock()
	return r.manifest(ctx, repo, digest, "", generation)
}

func (r *cacheRegistry) manifest(ctx context.Context, repo string, digest oci.Digest, tag string, generation uint64) (oci.BlobReader, error) {
	k := cacheKey{repo, "manifest", digest.String()}
	release, err := r.acquire(ctx, k)
	if err != nil {
		return nil, err
	}
	defer release()
	var reader oci.BlobReader
	if r.usable(k) {
		reader, err = r.cache.GetManifest(ctx, repo, digest)
		if err == nil && tag == "" {
			return reader, nil
		}
		if err != nil && !cacheMissing(err) {
			return nil, err
		}
	}
	if reader == nil {
		reader, err = r.Registry.GetManifest(ctx, repo, digest)
		if err != nil {
			return nil, err
		}
	}
	defer reader.Close()
	desc := reader.Descriptor()
	if desc.Size < 0 {
		return nil, oci.ErrSizeInvalid
	}
	data, err := io.ReadAll(io.LimitReader(reader, desc.Size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != desc.Size {
		return nil, oci.ErrSizeInvalid
	}
	if err := digest.Validate(); err != nil {
		return nil, oci.ErrDigestInvalid
	}
	if desc.Digest != digest || digest.Algorithm().FromBytes(data) != digest {
		return nil, oci.ErrDigestInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	params := &oci.PushManifestParameters{Digest: digest}
	if tag != "" {
		params.Tags = []string{tag}
	}
	s := r.state(repo)
	s.mu.Lock()
	if generation == s.generation {
		_, err = r.cache.PushManifest(ctx, repo, data, desc.MediaType, params)
		if err == nil {
			delete(s.invalid, k)
			if tag != "" {
				delete(s.invalid, cacheKey{repo, "tag", tag})
			}
		}
	}
	s.mu.Unlock()
	r.report(err)
	return &cacheContentReader{ReadCloser: io.NopCloser(bytes.NewReader(data)), desc: desc}, nil
}

// GetBlob streams a cache miss upstream and populates the cache after verified EOF.
func (r *cacheRegistry) GetBlob(ctx context.Context, repo string, digest oci.Digest) (oci.BlobReader, error) {
	k := cacheKey{repo, "blob", digest.String()}
	release, err := r.acquire(ctx, k)
	if err != nil {
		return nil, err
	}
	if r.usable(k) {
		reader, err := r.cache.GetBlob(ctx, repo, digest)
		if !cacheMissing(err) {
			release()
			return reader, err
		}
	}
	s := r.state(repo)
	s.mu.Lock()
	generation := s.generation
	s.mu.Unlock()
	reader, err := r.Registry.GetBlob(ctx, repo, digest)
	if err != nil {
		release()
		return nil, err
	}
	return r.stream(ctx, k, generation, reader, release)
}

// GetBlobRange serves cached ranges or forwards partial misses without populating.
func (r *cacheRegistry) GetBlobRange(ctx context.Context, repo string, digest oci.Digest, start, end int64) (oci.BlobReader, error) {
	if start == 0 && end < 0 {
		return r.GetBlob(ctx, repo, digest)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.usable(cacheKey{repo, "blob", digest.String()}) {
		reader, err := r.cache.GetBlobRange(ctx, repo, digest, start, end)
		if !cacheMissing(err) {
			return reader, err
		}
	}
	// Partial content is never published as a complete cached blob.
	return r.Registry.GetBlobRange(ctx, repo, digest, start, end)
}

// invalidate fences in-progress fills, and bypasses stale cache entries even if
// the cache backend fails to remove them. Writes are authoritative upstream.
func (r *cacheRegistry) invalidate(k cacheKey, remove func() error) {
	s := r.state(k.repo)
	s.mu.Lock()
	s.generation++
	s.invalid[k] = true
	err := remove()
	s.mu.Unlock()
	if !cacheMissing(err) {
		r.report(err)
	}
}

// PushManifest writes upstream and invalidates affected cached tags and manifests.
func (r *cacheRegistry) PushManifest(ctx context.Context, repo string, data []byte, mediaType string, params *oci.PushManifestParameters) (oci.Descriptor, error) {
	if params != nil && params.IfMatch != "" && len(params.Tags) == 1 {
		// Discovery must reach the authoritative HTTP client even if the tag was
		// read from cache. Only the upstream backend evaluates the condition.
		_, err := r.Registry.ResolveTag(ctx, repo, params.Tags[0])
		if err != nil && !cacheMissing(err) {
			return oci.Descriptor{}, err
		}
	}
	desc, err := r.Registry.PushManifest(ctx, repo, data, mediaType, params)
	if err != nil {
		return desc, err
	}
	if params != nil {
		for _, tag := range params.Tags {
			name := tag
			r.invalidate(cacheKey{repo, "tag", name}, func() error { return r.cache.DeleteTag(ctx, repo, name) })
		}
	}
	r.invalidate(cacheKey{repo, "manifest", desc.Digest.String()}, func() error { return r.cache.DeleteManifest(ctx, repo, desc.Digest) })
	return desc, nil
}

// DeleteTag deletes upstream and invalidates the cached assignment.
func (r *cacheRegistry) DeleteTag(ctx context.Context, repo, tag string) error {
	if err := r.Registry.DeleteTag(ctx, repo, tag); err != nil {
		return err
	}
	r.invalidate(cacheKey{repo, "tag", tag}, func() error { return r.cache.DeleteTag(ctx, repo, tag) })
	return nil
}

// DeleteManifest deletes upstream and invalidates the cached manifest.
func (r *cacheRegistry) DeleteManifest(ctx context.Context, repo string, digest oci.Digest) error {
	if err := r.Registry.DeleteManifest(ctx, repo, digest); err != nil {
		return err
	}
	r.invalidate(cacheKey{repo, "manifest", digest.String()}, func() error { return r.cache.DeleteManifest(ctx, repo, digest) })
	return nil
}

// DeleteBlob deletes upstream and invalidates the cached blob.
func (r *cacheRegistry) DeleteBlob(ctx context.Context, repo string, digest oci.Digest) error {
	if err := r.Registry.DeleteBlob(ctx, repo, digest); err != nil {
		return err
	}
	r.invalidate(cacheKey{repo, "blob", digest.String()}, func() error { return r.cache.DeleteBlob(ctx, repo, digest) })
	return nil
}

// TagHistory returns history from upstream, like all listings.
func (r *cacheRegistry) TagHistory(ctx context.Context, repo, tag string, params *oci.TagHistoryParameters) iter.Seq2[oci.Descriptor, error] {
	if history, ok := r.Registry.(oci.TagHistory); ok {
		return history.TagHistory(ctx, repo, tag, params)
	}
	return oci.ErrorSeq[oci.Descriptor](oci.ErrUnsupported)
}

type cacheContentReader struct {
	io.ReadCloser
	desc oci.Descriptor
}

func (r *cacheContentReader) Descriptor() oci.Descriptor { return r.desc }
