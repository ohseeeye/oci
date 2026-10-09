package ocitransfer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/ociclient"
	"github.com/ohseeeye/oci/ocilayout"
	"github.com/ohseeeye/oci/ocimem"
	"github.com/ohseeeye/oci/ociserver"
	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/stretchr/testify/require"
)

type copyFixture struct {
	registry    *ocimem.Registry
	root, layer oci.Descriptor
	children    []oci.Descriptor
	data        []byte
}

func makeCopyFixture(t *testing.T) copyFixture {
	t.Helper()
	r := ocimem.New()
	content := []byte("shared content")
	layer := oci.Descriptor{MediaType: "application/vnd.oci.image.layer.v1.tar", Digest: ocidigest.SHA512.FromBytes(content), Size: int64(len(content))}
	_, err := r.PushBlob(t.Context(), "source", layer, bytes.NewReader(content))
	require.NoError(t, err)
	var children []oci.Descriptor
	for _, arch := range []string{"amd64", "arm64"} {
		data, err := json.Marshal(oci.IndexOrManifest{SchemaVersion: 2, MediaType: oci.MediaTypeImageManifest, Config: &layer, Layers: []oci.Descriptor{layer}, Annotations: map[string]string{"arch": arch}})
		require.NoError(t, err)
		desc, err := r.PushManifest(t.Context(), "source", data, oci.MediaTypeImageManifest, nil)
		require.NoError(t, err)
		desc.Platform = &oci.Platform{OS: "linux", Architecture: arch}
		children = append(children, desc)
	}
	data, err := json.Marshal(oci.IndexOrManifest{SchemaVersion: 2, MediaType: oci.MediaTypeImageIndex, Manifests: children})
	require.NoError(t, err)
	// Unknown fields and original formatting must survive a copy unchanged.
	data = append([]byte("{\n  \"customExtension\": {\"example\": true}, "), data[1:]...)
	digest := ocidigest.SHA512.FromBytes(data)
	root, err := r.PushManifest(t.Context(), "source", data, oci.MediaTypeImageIndex, &oci.PushManifestParameters{Digest: digest, Tags: []string{"latest", "v1"}})
	require.NoError(t, err)
	return copyFixture{r, root, layer, children, data}
}

type observedCopySource struct {
	oci.Registry
	blobReads, manifestReads, tagReads atomic.Int64
}

func (s *observedCopySource) GetBlob(ctx context.Context, repo string, digest oci.Digest) (oci.BlobReader, error) {
	s.blobReads.Add(1)
	return s.Registry.GetBlob(ctx, repo, digest)
}
func (s *observedCopySource) GetManifest(ctx context.Context, repo string, digest oci.Digest) (oci.BlobReader, error) {
	s.manifestReads.Add(1)
	return s.Registry.GetManifest(ctx, repo, digest)
}
func (s *observedCopySource) ResolveTag(ctx context.Context, repo, tag string) (oci.Descriptor, error) {
	s.tagReads.Add(1)
	return s.Registry.ResolveTag(ctx, repo, tag)
}

func readCopyContent(t *testing.T, reader oci.BlobReader, err error) []byte {
	t.Helper()
	require.NoError(t, err)
	defer reader.Close()
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	return data
}

func checkCopyFixture(t *testing.T, f copyFixture, dst oci.Reader, tag string) {
	t.Helper()
	root, err := dst.ResolveTag(t.Context(), "destination", tag)
	require.NoError(t, err)
	require.Equal(t, f.root.Digest, root.Digest)
	reader, err := dst.GetManifest(t.Context(), "destination", root.Digest)
	require.Equal(t, f.data, readCopyContent(t, reader, err))
	for _, child := range f.children {
		reader, err := dst.GetManifest(t.Context(), "destination", child.Digest)
		require.NoError(t, err)
		require.NoError(t, reader.Close())
	}
	reader, err = dst.GetBlob(t.Context(), "destination", f.layer.Digest)
	require.Equal(t, []byte("shared content"), readCopyContent(t, reader, err))
}

func TestCopy(t *testing.T) {
	for _, backend := range []string{"memory", "layout", "http", "http destination"} {
		t.Run(backend, func(t *testing.T) {
			f := makeCopyFixture(t)
			source := &observedCopySource{Registry: f.registry}
			var src oci.Reader = source
			var dst oci.ReadWriter = ocimem.New()
			if backend == "layout" {
				var err error
				dst, err = ocilayout.New(t.TempDir(), nil)
				require.NoError(t, err)
			}
			if backend == "http destination" {
				handler, err := ociserver.New(ocimem.New(), nil)
				require.NoError(t, err)
				server := httptest.NewServer(handler)
				t.Cleanup(server.Close)
				dst, err = ociclient.New(strings.TrimPrefix(server.URL, "http://"), &ociclient.Options{Insecure: true})
				require.NoError(t, err)
			}
			if backend == "http" {
				handler, err := ociserver.New(source, nil)
				require.NoError(t, err)
				server := httptest.NewServer(handler)
				t.Cleanup(server.Close)
				src, err = ociclient.New(strings.TrimPrefix(server.URL, "http://"), &ociclient.Options{Insecure: true})
				require.NoError(t, err)
			}
			got, err := Copy(t.Context(), src, "source", "latest", dst, "destination", nil)
			require.NoError(t, err)
			require.Equal(t, f.root.Digest, got.Digest)
			checkCopyFixture(t, f, dst, "latest")
			require.EqualValues(t, 1, source.blobReads.Load(), "shared blobs transfer only once")
			_, err = Copy(t.Context(), src, "source", f.root.Digest.String(), dst, "destination", &CopyOptions{Tags: []string{"release", "stable"}})
			require.NoError(t, err)
			checkCopyFixture(t, f, dst, "release")
			require.EqualValues(t, 1, source.blobReads.Load(), "existing destination blobs are skipped")
		})
	}
}

func TestCopyTagPolicy(t *testing.T) {
	for _, tags := range [][]string{nil, {}} {
		f := makeCopyFixture(t)
		dst := ocimem.New()
		_, err := Copy(t.Context(), f.registry, "source", f.root.Digest.String(), dst, "destination", &CopyOptions{Tags: tags})
		require.NoError(t, err)
		got, err := oci.All(dst.Tags(t.Context(), "destination", nil))
		require.NoError(t, err)
		require.Empty(t, got, "copy by digest assigns no source tags")
	}
	f := makeCopyFixture(t)
	dst := ocimem.New()
	_, err := Copy(t.Context(), f.registry, "source", "latest", dst, "destination", &CopyOptions{Tags: []string{}})
	require.NoError(t, err)
	tags, err := oci.All(dst.Tags(t.Context(), "destination", nil))
	require.NoError(t, err)
	require.Empty(t, tags, "explicitly empty Tags disables tag assignment")
}

func TestCopyRepository(t *testing.T) {
	f := makeCopyFixture(t)
	source := &observedCopySource{Registry: f.registry}
	dst := ocimem.New()
	// An untagged root is intentionally outside the repository helper's scope.
	untagged, err := f.registry.PushManifest(t.Context(), "source", []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`), oci.MediaTypeImageIndex, nil)
	require.NoError(t, err)
	require.NoError(t, CopyRepository(t.Context(), source, "source", dst, "destination", nil))
	checkCopyFixture(t, f, dst, "latest")
	checkCopyFixture(t, f, dst, "v1")
	require.EqualValues(t, 2, source.tagReads.Load())
	require.EqualValues(t, 1, source.blobReads.Load())
	require.EqualValues(t, 4, source.manifestReads.Load(), "three graph nodes plus root read for the second tag")
	_, err = dst.ResolveManifest(t.Context(), "destination", untagged.Digest)
	require.ErrorIs(t, err, oci.ErrManifestUnknown)
	err = CopyRepository(t.Context(), source, "source", dst, "destination", &CopyOptions{Tags: []string{}})
	require.Error(t, err)
}

func TestCopyRepairsSparseDestination(t *testing.T) {
	f := makeCopyFixture(t)
	dst := ocimem.NewWithConfig(&ocimem.Config{AllowSparseManifests: true})
	_, err := dst.PushManifest(t.Context(), "destination", f.data, f.root.MediaType, &oci.PushManifestParameters{Digest: f.root.Digest})
	require.NoError(t, err)
	_, err = Copy(t.Context(), f.registry, "source", "latest", dst, "destination", nil)
	require.NoError(t, err)
	checkCopyFixture(t, f, dst, "latest")
}

func TestCopyMounts(t *testing.T) {
	f := makeCopyFixture(t)
	dst := ocimem.New()
	_, err := dst.PushBlob(t.Context(), "mounted", f.layer, strings.NewReader("shared content"))
	require.NoError(t, err)
	source := &observedCopySource{Registry: f.registry}
	_, err = Copy(t.Context(), source, "source", "latest", dst, "destination", &CopyOptions{MountFrom: "mounted"})
	require.NoError(t, err)
	require.Zero(t, source.blobReads.Load())
	checkCopyFixture(t, f, dst, "latest")
	dst = ocimem.New()
	_, err = Copy(t.Context(), source, "source", "latest", dst, "destination", &CopyOptions{MountFrom: "missing"})
	require.NoError(t, err)
	require.EqualValues(t, 1, source.blobReads.Load(), "missing mount source falls back to streaming")
}

func TestCopyReferrers(t *testing.T) {
	for _, include := range []bool{false, true} {
		f := makeCopyFixture(t)
		var refs []oci.Descriptor
		subject := f.root
		for range 2 {
			data, err := json.Marshal(oci.IndexOrManifest{SchemaVersion: 2, MediaType: oci.MediaTypeImageManifest, Config: &f.layer, Layers: []oci.Descriptor{}, Subject: &subject, ArtifactType: "application/example.signature"})
			require.NoError(t, err)
			ref, err := f.registry.PushManifest(t.Context(), "source", data, oci.MediaTypeImageManifest, nil)
			require.NoError(t, err)
			refs = append(refs, ref)
			subject = ref
		}
		dst := ocimem.New()
		_, err := Copy(t.Context(), f.registry, "source", "latest", dst, "destination", &CopyOptions{IncludeReferrers: include})
		require.NoError(t, err)
		for _, ref := range refs {
			_, err = dst.ResolveManifest(t.Context(), "destination", ref.Digest)
			if include {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, oci.ErrManifestUnknown)
			}
		}
	}
}

type copySourceOverride struct {
	oci.Reader
	getBlob func(context.Context, string, oci.Digest) (oci.BlobReader, error)
}

func (s copySourceOverride) GetBlob(ctx context.Context, repo string, digest oci.Digest) (oci.BlobReader, error) {
	return s.getBlob(ctx, repo, digest)
}

type copyTestReader struct {
	io.ReadCloser
	desc oci.Descriptor
}

func (r copyTestReader) Descriptor() oci.Descriptor { return r.desc }

func TestCopyVerifiesContentBeforeTagging(t *testing.T) {
	for _, test := range []struct {
		name, content string
		want          error
	}{
		{"digest", "broken content", oci.ErrDigestInvalid},
		{"short", "short", oci.ErrSizeInvalid},
		{"long", "longer than shared content", oci.ErrSizeInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := makeCopyFixture(t)
			source := copySourceOverride{Reader: f.registry, getBlob: func(context.Context, string, oci.Digest) (oci.BlobReader, error) {
				return copyTestReader{io.NopCloser(strings.NewReader(test.content)), f.layer}, nil
			}}
			dst := ocimem.New()
			_, err := Copy(t.Context(), source, "source", "latest", dst, "destination", nil)
			require.ErrorIs(t, err, test.want)
			_, err = dst.ResolveTag(t.Context(), "destination", "latest")
			require.Error(t, err, "a failed copy must not assign destination tags")
		})
	}
}

func TestCopyInlineAndExternalContent(t *testing.T) {
	r := ocimem.NewWithConfig(&ocimem.Config{AllowSparseManifests: true})
	inline := []byte("embedded")
	config := oci.Descriptor{MediaType: oci.MediaTypeImageConfig, Digest: ocidigest.FromBytes(inline), Size: int64(len(inline)), Data: inline}
	layer := oci.Descriptor{MediaType: "application/vnd.oci.image.layer.nondistributable.v1.tar", Digest: ocidigest.FromBytes([]byte("external")), Size: 8, URLs: []string{"https://example.invalid/layer"}}
	data, err := json.Marshal(oci.IndexOrManifest{SchemaVersion: 2, MediaType: oci.MediaTypeImageManifest, Config: &config, Layers: []oci.Descriptor{layer}})
	require.NoError(t, err)
	_, err = r.PushManifest(t.Context(), "source", data, oci.MediaTypeImageManifest, &oci.PushManifestParameters{Tags: []string{"latest"}})
	require.NoError(t, err)
	dst := ocimem.New()
	_, err = Copy(t.Context(), r, "source", "latest", dst, "destination", nil)
	require.NoError(t, err)
	reader, err := dst.GetBlob(t.Context(), "destination", config.Digest)
	require.Equal(t, inline, readCopyContent(t, reader, err))
	_, err = dst.ResolveBlob(t.Context(), "destination", layer.Digest)
	require.ErrorIs(t, err, oci.ErrBlobUnknown)
}

func TestCopyRejectsInvalidInputs(t *testing.T) {
	f := makeCopyFixture(t)
	for _, test := range []struct {
		name, ref string
		options   *CopyOptions
	}{
		{"empty reference", "", nil},
		{"bad digest", "sha256:bad", nil},
		{"bad tag", "bad/tag", nil},
		{"negative concurrency", "latest", &CopyOptions{Concurrency: -1}},
		{"negative size", "latest", &CopyOptions{MaxManifestSize: -1}},
		{"invalid tags", "latest", &CopyOptions{Tags: []string{"bad/tag"}}},
		{"invalid mount", "latest", &CopyOptions{MountFrom: "../bad"}},
		{"size limit", "latest", &CopyOptions{MaxManifestSize: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Copy(t.Context(), f.registry, "source", test.ref, ocimem.New(), "destination", test.options)
			require.Error(t, err)
		})
	}
	_, err := Copy(t.Context(), nil, "source", "latest", ocimem.New(), "destination", nil)
	require.Error(t, err)
	_, err = Copy(t.Context(), f.registry, "source", "latest", nil, "destination", nil)
	require.Error(t, err)
	_, err = Copy(t.Context(), f.registry, "../source", "latest", ocimem.New(), "destination", nil)
	require.ErrorIs(t, err, oci.ErrNameInvalid)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = Copy(ctx, f.registry, "source", "latest", ocimem.New(), "destination", nil)
	require.ErrorIs(t, err, context.Canceled)
	// Embedding only Reader deliberately omits the optional referrer capability.
	_, err = Copy(t.Context(), struct{ oci.Reader }{f.registry}, "source", "latest", ocimem.New(), "destination", &CopyOptions{IncludeReferrers: true})
	require.ErrorIs(t, err, oci.ErrUnsupported)
}

func TestCopyPropagatesDestinationErrors(t *testing.T) {
	f := makeCopyFixture(t)
	failure := errors.New("destination unavailable")
	dst := &oci.Funcs{ResolveBlob_: func(context.Context, string, oci.Digest) (oci.Descriptor, error) { return oci.Descriptor{}, failure }}
	_, err := Copy(t.Context(), f.registry, "source", "latest", dst, "destination", nil)
	require.ErrorIs(t, err, failure)
}

func TestCopyRejectsUntraversableManifests(t *testing.T) {
	r := ocimem.New()
	_, err := r.PushManifest(t.Context(), "source", []byte("opaque"), "application/example.unknown", &oci.PushManifestParameters{Tags: []string{"latest"}})
	require.NoError(t, err)
	_, err = Copy(t.Context(), r, "source", "latest", ocimem.New(), "destination", nil)
	require.ErrorIs(t, err, oci.ErrUnsupported)
}

type copyManifestOverride struct {
	oci.Reader
	getManifest func(context.Context, string, oci.Digest) (oci.BlobReader, error)
}

func (s copyManifestOverride) GetManifest(ctx context.Context, repo string, digest oci.Digest) (oci.BlobReader, error) {
	return s.getManifest(ctx, repo, digest)
}

func TestCopyRejectsCorruptManifest(t *testing.T) {
	f := makeCopyFixture(t)
	corrupt := bytes.Clone(f.data)
	corrupt[len(corrupt)-1] = ' '
	source := copyManifestOverride{Reader: f.registry, getManifest: func(context.Context, string, oci.Digest) (oci.BlobReader, error) {
		return copyTestReader{io.NopCloser(bytes.NewReader(corrupt)), f.root}, nil
	}}
	dst := ocimem.New()
	_, err := Copy(t.Context(), source, "source", "latest", dst, "destination", nil)
	require.ErrorIs(t, err, oci.ErrDigestInvalid)
	_, err = dst.ResolveTag(t.Context(), "destination", "latest")
	require.Error(t, err)
}

func TestCopyPinsSourceTag(t *testing.T) {
	f := makeCopyFixture(t)
	source := &oci.Funcs{
		ResolveTag_: func(ctx context.Context, repo, tag string) (oci.Descriptor, error) {
			root, err := f.registry.ResolveTag(ctx, repo, tag)
			require.NoError(t, err)
			_, err = f.registry.PushManifest(ctx, repo, []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`), oci.MediaTypeImageIndex, &oci.PushManifestParameters{Tags: []string{tag}})
			require.NoError(t, err)
			return root, nil
		},
		GetManifest_: f.registry.GetManifest,
		GetBlob_:     f.registry.GetBlob,
	}
	dst := ocimem.New()
	_, err := Copy(t.Context(), source, "source", "latest", dst, "destination", nil)
	require.NoError(t, err)
	checkCopyFixture(t, f, dst, "latest")
	current, err := f.registry.ResolveTag(t.Context(), "source", "latest")
	require.NoError(t, err)
	require.NotEqual(t, f.root.Digest, current.Digest)
}

func TestCopyConcurrencyAndCancellation(t *testing.T) {
	r := ocimem.New()
	var layers []oci.Descriptor
	for _, data := range []string{"one", "two", "three"} {
		desc := oci.Descriptor{Digest: ocidigest.FromBytes([]byte(data)), Size: int64(len(data)), MediaType: "application/octet-stream"}
		_, err := r.PushBlob(t.Context(), "source", desc, strings.NewReader(data))
		require.NoError(t, err)
		layers = append(layers, desc)
	}
	data, err := json.Marshal(oci.IndexOrManifest{SchemaVersion: 2, MediaType: oci.MediaTypeImageManifest, Config: &layers[0], Layers: layers})
	require.NoError(t, err)
	_, err = r.PushManifest(t.Context(), "source", data, oci.MediaTypeImageManifest, &oci.PushManifestParameters{Tags: []string{"latest"}})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	started := make(chan struct{}, 3)
	var active atomic.Int64
	source := copySourceOverride{Reader: r, getBlob: func(ctx context.Context, _ string, _ oci.Digest) (oci.BlobReader, error) {
		active.Add(1)
		defer active.Add(-1)
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	finished := make(chan error, 1)
	go func() {
		_, err := Copy(ctx, source, "source", "latest", ocimem.New(), "destination", &CopyOptions{Concurrency: 2})
		finished <- err
	}()
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("workers failed to start")
		}
	}
	require.EqualValues(t, 2, active.Load())
	cancel()
	require.ErrorIs(t, <-finished, context.Canceled)
	require.Zero(t, active.Load(), "Copy waits for canceled workers")
}
