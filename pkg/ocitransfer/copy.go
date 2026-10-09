package ocitransfer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"slices"
	"strings"
	"sync"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/ohseeeye/oci/pkg/ociref"
)

// CopyOptions controls image, index, and artifact copies.
type CopyOptions struct {
	// Tags assigns destination tags to the root manifest. Nil preserves a source
	// tag (or leaves a source digest untagged); an empty slice copies by digest only.
	// CopyRepository preserves each source tag and requires Tags to be nil.
	Tags []string
	// Concurrency limits simultaneous blob transfers. Zero selects four.
	Concurrency int
	// MountFrom optionally names a repository in the DESTINATION registry from
	// which to mount blobs. Missing content or unsupported mounts fall back to
	// streaming from source. Other mount errors are returned.
	MountFrom string
	// IncludeReferrers recursively includes referrers of copied manifests. Source
	// must implement Referrers; unsupported discovery is an error, not an empty list.
	IncludeReferrers bool
	// MaxManifestSize limits each buffered manifest. Zero selects 16 MiB.
	MaxManifestSize int64
}

type referrerSource interface {
	Referrers(context.Context, string, oci.Digest, *oci.ReferrersParameters) iter.Seq2[oci.Descriptor, error]
}

type copier struct {
	source          oci.Reader
	destination     oci.ReadWriter
	sourceRepo      string
	destinationRepo string
	opts            CopyOptions
	manifests       map[oci.Digest]oci.Descriptor
	blobs           map[oci.Digest]oci.Descriptor
}

type copyManifest struct {
	desc oci.Descriptor
	data []byte
}

type copyGraph struct {
	owner     *copier
	manifests []copyManifest // Children precede parents.
	blobs     map[oci.Digest]oci.Descriptor
	seen      map[oci.Digest]oci.Descriptor
	visiting  map[oci.Digest]bool
}

// Copy copies the complete content reachable from a source tag or digest into
// destinationRepo, returning the root descriptor. Reference must be non-empty;
// use CopyRepository to copy all tags. Source tags are resolved once, then read
// by digest. Destination tags are assigned after all content has been copied.
//
// Copy preserves manifest bytes and all platforms. It validates sizes and digests,
// traverses OCI/Docker manifests and indexes, and checks children even when their
// parent already exists at the destination. Unknown manifest media types return
// ErrUnsupported because their dependencies cannot be determined safely.
//
// Inline descriptor data is copied directly. Blobs with URLs and no inline data
// remain external references; Copy does not fetch those URLs. Subjects are not
// traversed as dependencies. IncludeReferrers follows reverse subject links.
// Copy does not delete destination content and is not atomic: a failure can leave
// completed blobs/manifests or some assigned tags. Rerunning skips existing content.
func Copy(ctx context.Context, source oci.Reader, sourceRepo, reference string, destination oci.ReadWriter, destinationRepo string, options *CopyOptions) (oci.Descriptor, error) {
	c, err := newCopier(source, sourceRepo, destination, destinationRepo, options)
	if err != nil {
		return oci.Descriptor{}, err
	}
	root, tag, err := c.resolve(ctx, reference)
	if err != nil {
		return oci.Descriptor{}, err
	}
	tags := c.opts.Tags
	if tags == nil && tag != "" {
		tags = []string{tag}
	}
	return c.copy(ctx, root, tags)
}

// CopyRepository copies every listed source tag and its reachable content,
// preserving tag names and sharing deduplication across tags. It does not discover
// untagged roots, delete destination-only tags, or take a transactional snapshot
// of the source. Each tag is resolved once when processed. IncludeReferrers also
// copies untagged artifacts referring to reachable manifests.
func CopyRepository(ctx context.Context, source oci.Registry, sourceRepo string, destination oci.ReadWriter, destinationRepo string, options *CopyOptions) error {
	c, err := newCopier(source, sourceRepo, destination, destinationRepo, options)
	if err != nil {
		return err
	}
	if c.opts.Tags != nil {
		return fmt.Errorf("CopyRepository preserves source tags; Tags must be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tags, err := oci.All(source.Tags(ctx, sourceRepo, nil))
	if err != nil {
		return fmt.Errorf("listing source tags: %w", err)
	}
	for _, tag := range tags {
		root, _, err := c.resolve(ctx, tag)
		if err != nil {
			return fmt.Errorf("copying tag %q: %w", tag, err)
		}
		if _, err := c.copy(ctx, root, []string{tag}); err != nil {
			return fmt.Errorf("copying tag %q: %w", tag, err)
		}
	}
	return nil
}

func newCopier(source oci.Reader, sourceRepo string, destination oci.ReadWriter, destinationRepo string, options *CopyOptions) (*copier, error) {
	if source == nil || destination == nil {
		return nil, fmt.Errorf("source and destination must be non-nil")
	}
	if !ociref.IsValidRepository(sourceRepo) || !ociref.IsValidRepository(destinationRepo) {
		return nil, oci.ErrNameInvalid
	}
	var opts CopyOptions
	if options != nil {
		opts = *options
		opts.Tags = slices.Clone(options.Tags)
	}
	if opts.Concurrency < 0 || opts.MaxManifestSize < 0 {
		return nil, fmt.Errorf("copy options must not be negative")
	}
	if opts.Concurrency == 0 {
		opts.Concurrency = 4
	}
	if opts.MaxManifestSize == 0 {
		opts.MaxManifestSize = 16 << 20
	}
	for _, tag := range opts.Tags {
		if !ociref.IsValidTag(tag) {
			return nil, oci.ErrNameInvalid
		}
	}
	if opts.MountFrom != "" && !ociref.IsValidRepository(opts.MountFrom) {
		return nil, oci.ErrNameInvalid
	}
	if opts.IncludeReferrers {
		if _, ok := source.(referrerSource); !ok {
			return nil, fmt.Errorf("source does not support referrers: %w", oci.ErrUnsupported)
		}
	}
	return &copier{source: source, destination: destination, sourceRepo: sourceRepo, destinationRepo: destinationRepo, opts: opts,
		manifests: make(map[oci.Digest]oci.Descriptor), blobs: make(map[oci.Digest]oci.Descriptor)}, nil
}

func (c *copier) resolve(ctx context.Context, reference string) (oci.Descriptor, string, error) {
	if err := ctx.Err(); err != nil {
		return oci.Descriptor{}, "", err
	}
	if strings.Contains(reference, ":") {
		digest, err := ocidigest.Parse(reference)
		if err != nil {
			return oci.Descriptor{}, "", fmt.Errorf("invalid source digest: %w", oci.ErrDigestInvalid)
		}
		desc, err := c.source.ResolveManifest(ctx, c.sourceRepo, digest)
		if err == nil && desc.Digest != digest {
			err = oci.ErrDigestInvalid
		}
		return desc, "", err
	}
	if !ociref.IsValidTag(reference) {
		return oci.Descriptor{}, "", fmt.Errorf("source reference must be a tag or digest: %w", oci.ErrNameInvalid)
	}
	desc, err := c.source.ResolveTag(ctx, c.sourceRepo, reference)
	return desc, reference, err
}

func (c *copier) copy(ctx context.Context, root oci.Descriptor, tags []string) (oci.Descriptor, error) {
	g := &copyGraph{owner: c, blobs: make(map[oci.Digest]oci.Descriptor), seen: make(map[oci.Digest]oci.Descriptor), visiting: make(map[oci.Digest]bool)}
	if err := g.visit(ctx, root, 0); err != nil {
		return oci.Descriptor{}, err
	}
	if c.opts.IncludeReferrers {
		source, ok := c.source.(referrerSource)
		if !ok {
			return oci.Descriptor{}, oci.ErrUnsupported
		}
		//nolint:intrange // Discovering referrers appends manifests that must also be scanned.
		for i := 0; i < len(g.manifests); i++ {
			for desc, err := range source.Referrers(ctx, c.sourceRepo, g.manifests[i].desc.Digest, nil) {
				if err != nil {
					return oci.Descriptor{}, fmt.Errorf("listing referrers: %w", err)
				}
				if err := g.visit(ctx, desc, 0); err != nil {
					return oci.Descriptor{}, err
				}
			}
		}
	}
	if err := g.copyBlobs(ctx); err != nil {
		return oci.Descriptor{}, err
	}
	for _, manifest := range g.manifests {
		if err := ctx.Err(); err != nil {
			return oci.Descriptor{}, err
		}
		got, err := c.destination.ResolveManifest(ctx, c.destinationRepo, manifest.desc.Digest)
		if err == nil {
			if err := matchingDescriptor(manifest.desc, got); err != nil {
				return oci.Descriptor{}, err
			}
		} else {
			if !copyMissing(err) && !errors.Is(err, oci.ErrUnsupported) {
				return oci.Descriptor{}, err
			}
			got, err = c.destination.PushManifest(ctx, c.destinationRepo, manifest.data, manifest.desc.MediaType, &oci.PushManifestParameters{Digest: manifest.desc.Digest})
			if err != nil {
				return oci.Descriptor{}, fmt.Errorf("pushing manifest %s: %w", manifest.desc.Digest, err)
			}
			if err := matchingDescriptor(manifest.desc, got); err != nil {
				return oci.Descriptor{}, err
			}
		}
		c.manifests[manifest.desc.Digest] = manifest.desc
	}
	if len(tags) > 0 {
		// A previously copied root still needs its new tag assignment.
		var data []byte
		for _, manifest := range g.manifests {
			if manifest.desc.Digest == root.Digest {
				data = manifest.data
				break
			}
		}
		if data == nil {
			var err error
			data, err = c.manifestBytes(ctx, root)
			if err != nil {
				return oci.Descriptor{}, err
			}
		}
		got, err := c.destination.PushManifest(ctx, c.destinationRepo, data, root.MediaType, &oci.PushManifestParameters{Digest: root.Digest, Tags: tags})
		if err != nil {
			return oci.Descriptor{}, fmt.Errorf("assigning destination tags: %w", err)
		}
		if err := matchingDescriptor(root, got); err != nil {
			return oci.Descriptor{}, err
		}
	}
	return root, nil
}

func (g *copyGraph) visit(ctx context.Context, desc oci.Descriptor, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := copyDescriptor(desc); err != nil {
		return err
	}
	if desc.Data != nil {
		if err := verifyCopyBytes(desc, desc.Data); err != nil {
			return err
		}
	}
	if depth > 1000 || g.visiting[desc.Digest] {
		return fmt.Errorf("manifest graph is cyclic or too deep: %w", oci.ErrManifestInvalid)
	}
	if known, ok := g.owner.manifests[desc.Digest]; ok {
		return matchingDescriptor(desc, known)
	}
	if known, ok := g.seen[desc.Digest]; ok {
		return matchingDescriptor(desc, known)
	}
	data, err := g.owner.manifestBytes(ctx, desc)
	if err != nil {
		return err
	}
	switch desc.MediaType {
	case oci.MediaTypeImageManifest, oci.MediaTypeDockerManifest, oci.MediaTypeImageIndex, oci.MediaTypeDockerManifestList:
	default:
		return fmt.Errorf("cannot traverse manifest media type %q: %w", desc.MediaType, oci.ErrUnsupported)
	}
	var manifest oci.IndexOrManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("decoding manifest: %w: %v", oci.ErrManifestInvalid, err)
	}
	if manifest.MediaType == "" {
		manifest.MediaType = desc.MediaType
	}
	if manifest.MediaType != desc.MediaType || manifest.SchemaVersion != 2 {
		return oci.ErrManifestInvalid
	}
	if err := manifest.Validate(); err != nil {
		return fmt.Errorf("%w: %v", oci.ErrManifestInvalid, err)
	}
	g.visiting[desc.Digest] = true
	defer delete(g.visiting, desc.Digest)
	for _, child := range manifest.Manifests {
		if err := g.visit(ctx, child, depth+1); err != nil {
			return err
		}
	}
	blobs := slices.Clone(manifest.Layers)
	if manifest.Config != nil {
		blobs = append(blobs, *manifest.Config)
	}
	for _, blob := range blobs {
		if err := copyDescriptor(blob); err != nil {
			return err
		}
		if len(blob.URLs) > 0 && blob.Data == nil {
			continue
		}
		if blob.Data != nil {
			if err := verifyCopyBytes(blob, blob.Data); err != nil {
				return err
			}
		}
		if known, ok := g.blobs[blob.Digest]; ok {
			if known.Size != blob.Size {
				return oci.ErrSizeInvalid
			}
			if known.Data == nil && blob.Data != nil {
				g.blobs[blob.Digest] = blob
			}
		} else {
			g.blobs[blob.Digest] = blob
		}
	}
	g.seen[desc.Digest] = desc
	g.manifests = append(g.manifests, copyManifest{desc: desc, data: data})
	return nil
}

func (c *copier) manifestBytes(ctx context.Context, desc oci.Descriptor) ([]byte, error) {
	if desc.Size > c.opts.MaxManifestSize {
		return nil, fmt.Errorf("manifest exceeds MaxManifestSize: %w", oci.ErrSizeInvalid)
	}
	if desc.Data != nil {
		return desc.Data, verifyCopyBytes(desc, desc.Data)
	}
	reader, err := c.source.GetManifest(ctx, c.sourceRepo, desc.Digest)
	if err != nil {
		return nil, fmt.Errorf("reading manifest %s: %w", desc.Digest, err)
	}
	defer reader.Close()
	if err := matchingDescriptor(desc, reader.Descriptor()); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, source: reader}, desc.Size))
	if err != nil {
		return nil, err
	}
	// Probe separately to avoid overflowing a size+1 limit.
	var extra [1]byte
	if n, err := io.ReadFull(contextReader{ctx: ctx, source: reader}, extra[:]); n > 0 {
		return nil, oci.ErrSizeInvalid
	} else if err != io.EOF {
		return nil, err
	}
	return data, verifyCopyBytes(desc, data)
}

func copyDescriptor(desc oci.Descriptor) error {
	if desc.Digest.Validate() != nil {
		return oci.ErrDigestInvalid
	}
	if desc.Size < 0 {
		return oci.ErrSizeInvalid
	}
	if desc.MediaType == "" {
		return oci.ErrManifestInvalid
	}
	return nil
}

func matchingDescriptor(expected, got oci.Descriptor) error {
	if got.Digest != expected.Digest {
		return oci.ErrDigestInvalid
	}
	if got.Size != expected.Size {
		return oci.ErrSizeInvalid
	}
	if got.MediaType != expected.MediaType {
		return oci.ErrManifestInvalid
	}
	return nil
}

func verifyCopyBytes(desc oci.Descriptor, data []byte) error {
	if int64(len(data)) != desc.Size {
		return oci.ErrSizeInvalid
	}
	if desc.Digest.Algorithm().FromBytes(data) != desc.Digest {
		return oci.ErrDigestInvalid
	}
	return nil
}

func copyMissing(err error) bool {
	return errors.Is(err, oci.ErrNameUnknown) || errors.Is(err, oci.ErrManifestUnknown) || errors.Is(err, oci.ErrBlobUnknown)
}

func (g *copyGraph) copyBlobs(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan oci.Descriptor)
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error
	var completed []oci.Descriptor
	var completedMu sync.Mutex
	for range min(g.owner.opts.Concurrency, len(g.blobs)) {
		wg.Go(func() {
			for desc := range jobs {
				if err := g.owner.copyBlob(ctx, desc); err != nil {
					once.Do(func() { firstErr = err; cancel() })
					return
				}
				completedMu.Lock()
				completed = append(completed, desc)
				completedMu.Unlock()
			}
		})
	}
	for _, desc := range g.blobs {
		select {
		case <-ctx.Done():
		case jobs <- desc:
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, desc := range completed {
		g.owner.blobs[desc.Digest] = desc
	}
	return nil
}

func (c *copier) copyBlob(ctx context.Context, desc oci.Descriptor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if known, ok := c.blobs[desc.Digest]; ok {
		if known.Size != desc.Size {
			return oci.ErrSizeInvalid
		}
		return nil
	}
	got, err := c.destination.ResolveBlob(ctx, c.destinationRepo, desc.Digest)
	if err == nil {
		if got.Digest != desc.Digest {
			return oci.ErrDigestInvalid
		}
		if got.Size != desc.Size {
			return oci.ErrSizeInvalid
		}
		return nil
	}
	if !copyMissing(err) && !errors.Is(err, oci.ErrUnsupported) {
		return err
	}
	if c.opts.MountFrom != "" {
		got, err = c.destination.MountBlob(ctx, c.opts.MountFrom, c.destinationRepo, desc.Digest)
		if err == nil {
			if got.Digest != desc.Digest {
				return oci.ErrDigestInvalid
			}
			return nil
		}
		if !copyMissing(err) && !errors.Is(err, oci.ErrUnsupported) {
			return err
		}
	}
	var source io.Reader
	if desc.Data != nil {
		source = bytes.NewReader(desc.Data)
	} else {
		reader, err := c.source.GetBlob(ctx, c.sourceRepo, desc.Digest)
		if err != nil {
			return fmt.Errorf("reading blob %s: %w", desc.Digest, err)
		}
		defer reader.Close()
		if got := reader.Descriptor(); got.Digest != desc.Digest {
			return oci.ErrDigestInvalid
		} else if got.Size != desc.Size {
			return oci.ErrSizeInvalid
		}
		source = reader
	}
	reader, err := ocidigest.NewReader(contextReader{ctx: ctx, source: source}, desc.Digest.Algorithm())
	if err != nil {
		return err
	}
	verified := &copyBlobReader{Reader: reader, expected: desc}
	got, err = c.destination.PushBlob(ctx, c.destinationRepo, desc, verified)
	if verifyErr := verified.failure(); verifyErr != nil {
		return fmt.Errorf("reading blob %s: %w", desc.Digest, verifyErr)
	}
	if err != nil {
		return fmt.Errorf("pushing blob %s: %w", desc.Digest, err)
	}
	// Some backends stop reading if another writer already published this blob.
	if _, err := io.Copy(io.Discard, verified); err != nil {
		return err
	}
	if got.Digest != desc.Digest {
		return oci.ErrDigestInvalid
	}
	if got.Size != desc.Size {
		return oci.ErrSizeInvalid
	}
	return nil
}

type copyBlobReader struct {
	*ocidigest.Reader
	expected oci.Descriptor
	err      error
	mu       sync.Mutex
}

func (r *copyBlobReader) failure() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err == io.EOF {
		return nil
	}
	return r.err
}

func (r *copyBlobReader) Read(p []byte) (n int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return 0, r.err
	}
	defer func() {
		if err != nil {
			r.err = err
		}
	}()
	n, err = r.Reader.Read(p)
	if r.Size() > r.expected.Size {
		return n, oci.ErrSizeInvalid
	}
	if err == io.EOF {
		if r.Size() != r.expected.Size {
			return n, oci.ErrSizeInvalid
		}
		digest, digestErr := r.Digest()
		if digestErr != nil {
			return n, digestErr
		}
		if digest != r.expected.Digest {
			return n, oci.ErrDigestInvalid
		}
	}
	return n, err
}
