package ocimiddleware_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/ociclient"
	"github.com/ohseeeye/oci/ocilayout"
	"github.com/ohseeeye/oci/ocimem"
	"github.com/ohseeeye/oci/ocimiddleware"
	"github.com/ohseeeye/oci/ociserver"
	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/stretchr/testify/require"
)

type counted struct {
	oci.Registry
	blobs, manifests, tags, ranges atomic.Int64
}

func (r *counted) GetBlob(ctx context.Context, repo string, digest oci.Digest) (oci.BlobReader, error) {
	r.blobs.Add(1)
	return r.Registry.GetBlob(ctx, repo, digest)
}
func (r *counted) GetManifest(ctx context.Context, repo string, digest oci.Digest) (oci.BlobReader, error) {
	r.manifests.Add(1)
	return r.Registry.GetManifest(ctx, repo, digest)
}
func (r *counted) ResolveTag(ctx context.Context, repo, tag string) (oci.Descriptor, error) {
	r.tags.Add(1)
	return r.Registry.ResolveTag(ctx, repo, tag)
}
func (r *counted) GetBlobRange(ctx context.Context, repo string, digest oci.Digest, start, end int64) (oci.BlobReader, error) {
	r.ranges.Add(1)
	return r.Registry.GetBlobRange(ctx, repo, digest, start, end)
}

func sparse() *ocimem.Registry {
	return ocimem.NewWithConfig(&ocimem.Config{AllowSparseManifests: true})
}
func wrap(t *testing.T, upstream, cache oci.Registry, opts *ocimiddleware.CacheOptions) oci.Registry {
	t.Helper()
	r, err := ocimiddleware.Cache(upstream, cache, opts)
	require.NoError(t, err)
	return r
}
func content(t *testing.T, reader oci.BlobReader, err error) []byte {
	t.Helper()
	require.NoError(t, err)
	defer reader.Close()
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	return data
}
func getBlob(t *testing.T, r oci.Registry, digest oci.Digest) []byte {
	t.Helper()
	reader, err := r.GetBlob(t.Context(), "app", digest)
	return content(t, reader, err)
}
func getManifest(t *testing.T, r oci.Registry, digest oci.Digest) []byte {
	t.Helper()
	reader, err := r.GetManifest(t.Context(), "app", digest)
	return content(t, reader, err)
}
func pushBlob(t *testing.T, r oci.Registry, data string) oci.Descriptor {
	t.Helper()
	desc := oci.Descriptor{Digest: ocidigest.FromBytes([]byte(data)), Size: int64(len(data)), MediaType: "application/octet-stream"}
	got, err := r.PushBlob(t.Context(), "app", desc, strings.NewReader(data))
	require.NoError(t, err)
	return got
}
func pushIndex(t *testing.T, r oci.Registry, tag, marker string, children ...oci.Descriptor) (oci.Descriptor, []byte) {
	t.Helper()
	data, err := json.Marshal(oci.IndexOrManifest{SchemaVersion: 2, MediaType: oci.MediaTypeImageIndex, Manifests: children, Annotations: map[string]string{"marker": marker}})
	require.NoError(t, err)
	params := &oci.PushManifestParameters{}
	if tag != "" {
		params.Tags = []string{tag}
	}
	desc, err := r.PushManifest(t.Context(), "app", data, oci.MediaTypeImageIndex, params)
	require.NoError(t, err)
	return desc, data
}

func TestReadThrough(t *testing.T) {
	for _, backend := range []string{"memory", "layout", "per repository"} {
		t.Run(backend, func(t *testing.T) {
			var cache oci.Registry = sparse()
			var err error
			switch backend {
			case "layout":
				cache, err = ocilayout.New(t.TempDir(), &ocilayout.Options{AllowSparseManifests: true})
			case "per repository":
				cache, err = ocilayout.NewPerRepository(t.TempDir(), &ocilayout.PerRepoOptions{AllowSparseManifests: true})
			}
			require.NoError(t, err)
			upstream := &counted{Registry: ocimem.New()}
			blob := pushBlob(t, upstream, "configuration")
			data, err := json.Marshal(oci.IndexOrManifest{SchemaVersion: 2, MediaType: oci.MediaTypeImageManifest, Config: &blob, Layers: []oci.Descriptor{}})
			require.NoError(t, err)
			child, err := upstream.PushManifest(t.Context(), "app", data, oci.MediaTypeImageManifest, nil)
			require.NoError(t, err)
			parent, index := pushIndex(t, upstream, "latest", "parent", child)
			r := wrap(t, upstream, cache, nil)
			require.Equal(t, index, getManifest(t, r, parent.Digest))
			_, err = cache.ResolveManifest(t.Context(), "app", child.Digest)
			require.ErrorIs(t, err, oci.ErrManifestUnknown)
			require.Equal(t, data, getManifest(t, r, child.Digest))
			_, err = cache.ResolveBlob(t.Context(), "app", blob.Digest)
			require.ErrorIs(t, err, oci.ErrBlobUnknown)
			require.Equal(t, []byte("configuration"), getBlob(t, r, blob.Digest))
			require.Equal(t, index, getManifest(t, r, parent.Digest))
			require.Equal(t, data, getManifest(t, r, child.Digest))
			require.Equal(t, []byte("configuration"), getBlob(t, r, blob.Digest))
			require.EqualValues(t, 2, upstream.manifests.Load())
			require.EqualValues(t, 1, upstream.blobs.Load())
			got, err := r.ResolveManifest(t.Context(), "app", parent.Digest)
			require.NoError(t, err)
			require.Equal(t, parent.Digest, got.Digest)
			_, err = r.GetManifest(t.Context(), "other", parent.Digest)
			require.Error(t, err, "manifests do not cross repositories")
		})
	}
}

func TestTagPolicy(t *testing.T) {
	for _, cacheTags := range []bool{false, true} {
		t.Run(strconv.FormatBool(cacheTags), func(t *testing.T) {
			upstream := &counted{Registry: ocimem.New()}
			cache := sparse()
			first, _ := pushIndex(t, upstream, "latest", "first")
			r := wrap(t, upstream, cache, &ocimiddleware.CacheOptions{CacheTags: cacheTags})
			got, err := r.ResolveTag(t.Context(), "app", "latest")
			require.NoError(t, err)
			require.Equal(t, first.Digest, got.Digest)
			second, secondData := pushIndex(t, upstream, "latest", "second")
			got, err = r.ResolveTag(t.Context(), "app", "latest")
			require.NoError(t, err)
			if cacheTags {
				require.Equal(t, first.Digest, got.Digest)
				require.EqualValues(t, 1, upstream.tags.Load())
				// A TTL backend expires this assignment; simulate that here.
				require.NoError(t, cache.DeleteTag(t.Context(), "app", "latest"))
			} else {
				require.Equal(t, second.Digest, got.Digest)
				require.EqualValues(t, 2, upstream.tags.Load())
				_, err = cache.ResolveTag(t.Context(), "app", "latest")
				require.Error(t, err, "default must not populate tags")
			}
			reader, err := r.GetTag(t.Context(), "app", "latest")
			require.Equal(t, secondData, content(t, reader, err))
			// An incomplete cache must never determine listings or referrers.
			pushIndex(t, upstream, "uncached", "third")
			tags, err := oci.All(r.Tags(t.Context(), "app", nil))
			require.NoError(t, err)
			require.ElementsMatch(t, []string{"latest", "uncached"}, tags)
			_, err = upstream.PushManifest(t.Context(), "other", secondData, oci.MediaTypeImageIndex, nil)
			require.NoError(t, err)
			repos, err := oci.All(r.Repositories(t.Context(), ""))
			require.NoError(t, err)
			require.ElementsMatch(t, []string{"app", "other"}, repos)
			refData, err := json.Marshal(oci.IndexOrManifest{SchemaVersion: 2, MediaType: oci.MediaTypeImageIndex, Subject: &second, Manifests: []oci.Descriptor{}})
			require.NoError(t, err)
			ref, err := upstream.PushManifest(t.Context(), "app", refData, oci.MediaTypeImageIndex, nil)
			require.NoError(t, err)
			refs, err := oci.All(r.Referrers(t.Context(), "app", second.Digest, nil))
			require.NoError(t, err)
			require.Len(t, refs, 1)
			require.Equal(t, ref.Digest, refs[0].Digest)
			// counted deliberately exposes only Registry, not optional TagHistory.
			_, err = oci.All(r.(oci.TagHistory).TagHistory(t.Context(), "app", "latest", nil))
			require.ErrorIs(t, err, oci.ErrUnsupported)
		})
	}
}

func TestBlobRangesAndInterruptedReads(t *testing.T) {
	upstream := &counted{Registry: ocimem.New()}
	cache := sparse()
	desc := pushBlob(t, upstream, "0123456789")
	dir := t.TempDir()
	r := wrap(t, upstream, cache, &ocimiddleware.CacheOptions{TempDir: dir})
	reader, err := r.GetBlobRange(t.Context(), "app", desc.Digest, 2, 5)
	require.Equal(t, []byte("234"), content(t, reader, err))
	_, err = cache.ResolveBlob(t.Context(), "app", desc.Digest)
	require.Error(t, err)
	reader, err = r.GetBlob(t.Context(), "app", desc.Digest)
	require.NoError(t, err)
	_, err = reader.Read(make([]byte, 3))
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	_, err = cache.ResolveBlob(t.Context(), "app", desc.Digest)
	require.Error(t, err, "incomplete downloads must not populate")
	ctx, cancel := context.WithCancel(t.Context())
	reader, err = r.GetBlob(ctx, "app", desc.Digest)
	require.NoError(t, err)
	cancel()
	_, err = io.ReadAll(reader)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, reader.Close())
	_, err = cache.ResolveBlob(t.Context(), "app", desc.Digest)
	require.Error(t, err, "canceled downloads must not populate")
	require.Equal(t, []byte("0123456789"), getBlob(t, r, desc.Digest))
	reader, err = r.GetBlobRange(t.Context(), "app", desc.Digest, 2, 5)
	require.Equal(t, []byte("234"), content(t, reader, err))
	require.EqualValues(t, 1, upstream.ranges.Load())
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "temporary downloads must be removed")
}

func TestConcurrentMisses(t *testing.T) {
	upstream := &counted{Registry: ocimem.New()}
	cache := sparse()
	desc := pushBlob(t, upstream, "shared download")
	r := wrap(t, upstream, cache, nil)
	owner, err := r.GetBlob(t.Context(), "app", desc.Digest)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = r.GetBlob(ctx, "app", desc.Digest)
	require.ErrorIs(t, err, context.Canceled)
	const clients = 20
	var wg sync.WaitGroup
	results := make(chan error, clients)
	for range clients {
		wg.Go(func() {
			reader, err := r.GetBlob(t.Context(), "app", desc.Digest)
			if err == nil {
				var data []byte
				data, err = io.ReadAll(reader)
				_ = reader.Close()
				if err == nil && string(data) != "shared download" {
					err = errors.New("incorrect shared content")
				}
			}
			results <- err
		})
	}
	require.Equal(t, []byte("shared download"), content(t, owner, nil))
	wg.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, upstream.blobs.Load(), "concurrent misses share one download")
}

type badReader struct {
	io.ReadCloser
	desc oci.Descriptor
}

func (r *badReader) Descriptor() oci.Descriptor { return r.desc }

func TestUnverifiedContent(t *testing.T) {
	for _, test := range []struct {
		name, body string
		size       int64
		want       error
	}{
		{"digest", "wrong", 5, oci.ErrDigestInvalid},
		{"short", "ok", 5, oci.ErrSizeInvalid},
		{"long", "too long", 5, oci.ErrSizeInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			desc := oci.Descriptor{Digest: ocidigest.FromBytes([]byte("right")), Size: test.size, MediaType: "application/octet-stream"}
			upstream := &oci.Funcs{GetBlob_: func(context.Context, string, oci.Digest) (oci.BlobReader, error) {
				return &badReader{ReadCloser: io.NopCloser(strings.NewReader(test.body)), desc: desc}, nil
			}}
			cache := sparse()
			r := wrap(t, upstream, cache, nil)
			reader, err := r.GetBlob(t.Context(), "app", desc.Digest)
			require.NoError(t, err)
			_, err = io.ReadAll(reader)
			require.ErrorIs(t, err, test.want)
			require.NoError(t, reader.Close())
			_, err = cache.ResolveBlob(t.Context(), "app", desc.Digest)
			require.Error(t, err)
		})
	}
}

type failing struct {
	oci.Registry
	readErr, pushErr, deleteErr error
}

func (r *failing) GetBlob(ctx context.Context, repo string, digest oci.Digest) (oci.BlobReader, error) {
	if r.readErr != nil {
		return nil, r.readErr
	}
	return r.Registry.GetBlob(ctx, repo, digest)
}
func (r *failing) PushBlob(ctx context.Context, repo string, desc oci.Descriptor, reader io.Reader) (oci.Descriptor, error) {
	if r.pushErr != nil {
		return oci.Descriptor{}, r.pushErr
	}
	return r.Registry.PushBlob(ctx, repo, desc, reader)
}
func (r *failing) DeleteTag(ctx context.Context, repo, tag string) error {
	if r.deleteErr != nil {
		return r.deleteErr
	}
	return r.Registry.DeleteTag(ctx, repo, tag)
}

func TestCacheFailures(t *testing.T) {
	upstream := &counted{Registry: ocimem.New()}
	desc := pushBlob(t, upstream, "content")
	cache := &failing{Registry: sparse(), readErr: oci.ErrDenied}
	var reported []error
	r := wrap(t, upstream, cache, &ocimiddleware.CacheOptions{OnError: func(err error) { reported = append(reported, err) }})
	_, err := r.GetBlob(t.Context(), "app", desc.Digest)
	require.ErrorIs(t, err, oci.ErrDenied)
	require.Zero(t, upstream.blobs.Load(), "access errors must not cause fallback")
	cache.readErr = nil
	cache.pushErr = errors.New("cache full")
	require.Equal(t, []byte("content"), getBlob(t, r, desc.Digest))
	require.Len(t, reported, 1)
	require.ErrorIs(t, reported[0], cache.pushErr)
	_, err = cache.ResolveBlob(t.Context(), "app", desc.Digest)
	require.Error(t, err)
	r = wrap(t, upstream, cache, &ocimiddleware.CacheOptions{TempDir: filepath.Join(t.TempDir(), "missing"), OnError: func(err error) { reported = append(reported, err) }})
	require.Equal(t, []byte("content"), getBlob(t, r, desc.Digest))
	require.Len(t, reported, 2, "temporary-file failures still serve upstream bytes")
}

func TestWritesAndInvalidation(t *testing.T) {
	upstream := ocimem.New()
	cache := &failing{Registry: sparse()}
	first, _ := pushIndex(t, upstream, "latest", "first")
	r := wrap(t, upstream, cache, &ocimiddleware.CacheOptions{CacheTags: true})
	_, err := r.ResolveTag(t.Context(), "app", "latest")
	require.NoError(t, err)
	_, second := pushIndex(t, upstream, "", "second")
	params := &oci.PushManifestParameters{Tags: []string{"latest"}, IfMatch: strconv.Quote(ocidigest.FromBytes([]byte("wrong")).String())}
	_, err = r.PushManifest(t.Context(), "app", second, oci.MediaTypeImageIndex, params)
	require.ErrorIs(t, err, oci.ErrManifestInvalid)
	got, err := r.ResolveTag(t.Context(), "app", "latest")
	require.NoError(t, err)
	require.Equal(t, first.Digest, got.Digest)
	params.IfMatch = strconv.Quote(first.Digest.String())
	cache.deleteErr = errors.New("cannot delete cached tag")
	next, err := r.PushManifest(t.Context(), "app", second, oci.MediaTypeImageIndex, params)
	require.NoError(t, err)
	got, err = r.ResolveTag(t.Context(), "app", "latest")
	require.NoError(t, err)
	require.Equal(t, next.Digest, got.Digest, "failed invalidation must bypass stale cache")
	require.NoError(t, r.DeleteManifest(t.Context(), "app", next.Digest))
	_, err = r.ResolveTag(t.Context(), "app", "latest")
	require.Error(t, err, "deleted manifest cannot survive through cached aliases")
}

func TestDeleteDuringDownload(t *testing.T) {
	upstream := ocimem.New()
	cache := sparse()
	desc := pushBlob(t, upstream, "deleted while downloading")
	r := wrap(t, upstream, cache, nil)
	reader, err := r.GetBlob(t.Context(), "app", desc.Digest)
	require.NoError(t, err)
	require.NoError(t, r.DeleteBlob(t.Context(), "app", desc.Digest))
	require.Equal(t, []byte("deleted while downloading"), content(t, reader, nil))
	_, err = cache.ResolveBlob(t.Context(), "app", desc.Digest)
	require.Error(t, err, "older fill cannot resurrect content after deletion")
	_, err = r.GetBlob(t.Context(), "app", desc.Digest)
	require.Error(t, err)
}

func TestConditionalPushDiscoversAuthoritativeETag(t *testing.T) {
	upstream := ocimem.New()
	cache := sparse()
	first, firstData := pushIndex(t, upstream, "latest", "first")
	_, err := cache.PushManifest(t.Context(), "app", firstData, oci.MediaTypeImageIndex, &oci.PushManifestParameters{Tags: []string{"latest"}})
	require.NoError(t, err)
	second, secondData := pushIndex(t, upstream, "latest", "second")
	handler, err := ociserver.New(upstream, nil)
	require.NoError(t, err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := ociclient.New(strings.TrimPrefix(server.URL, "http://"), &ociclient.Options{Insecure: true})
	require.NoError(t, err)
	r := wrap(t, client, cache, &ocimiddleware.CacheOptions{CacheTags: true})
	got, err := r.ResolveTag(t.Context(), "app", "latest")
	require.NoError(t, err)
	require.Equal(t, first.Digest, got.Digest)
	require.False(t, client.SupportsIfMatch())
	_, err = r.PushManifest(t.Context(), "app", firstData, oci.MediaTypeImageIndex, &oci.PushManifestParameters{Tags: []string{"latest"}, IfMatch: strconv.Quote(first.Digest.String())})
	require.ErrorIs(t, err, oci.ErrManifestInvalid)
	require.True(t, client.SupportsIfMatch())
	got, err = upstream.ResolveTag(t.Context(), "app", "latest")
	require.NoError(t, err)
	require.Equal(t, second.Digest, got.Digest)
	require.Equal(t, secondData, getManifest(t, r, second.Digest))
}

func TestConstructorAndTagHistory(t *testing.T) {
	_, err := ocimiddleware.Cache(nil, sparse(), nil)
	require.Error(t, err)
	_, err = ocimiddleware.Cache(sparse(), nil, nil)
	require.Error(t, err)
	upstream := ocimem.New()
	pushIndex(t, upstream, "latest", "first")
	r := wrap(t, upstream, sparse(), nil)
	history, err := oci.All(r.(oci.TagHistory).TagHistory(t.Context(), "app", "latest", nil))
	require.NoError(t, err)
	require.Len(t, history, 1)
}
