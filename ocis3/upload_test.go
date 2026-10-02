package ocis3

import (
	"fmt"
	"sync"
	"testing"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/stretchr/testify/require"
)

func exerciseUploads(t *testing.T, r *Registry) {
	t.Helper()
	w, err := r.PushBlobChunked(t.Context(), "uploads", 3)
	require.NoError(t, err)
	n, err := w.Write([]byte("hello"))
	require.NoError(t, err)
	require.Equal(t, 5, n)
	require.NoError(t, w.Close())
	second := &Registry{client: r.client, bucket: r.bucket, prefix: r.prefix, partSize: r.partSize}
	_, err = second.PushBlobChunkedResume(t.Context(), "wrong", w.ID(), -1, 3)
	require.ErrorIs(t, err, oci.ErrBlobUploadUnknown)
	_, err = second.PushBlobChunkedResume(t.Context(), "uploads", w.ID(), 4, 3)
	require.ErrorIs(t, err, oci.ErrRangeInvalid)
	w, err = second.PushBlobChunkedResume(t.Context(), "uploads", w.ID(), -1, 3)
	require.NoError(t, err)
	require.EqualValues(t, 5, w.Size())
	stale, err := r.PushBlobChunkedResume(t.Context(), "uploads", w.ID(), 5, 3)
	require.NoError(t, err)
	n, err = w.Write([]byte(" world"))
	require.NoError(t, err)
	require.Equal(t, 6, n)
	n, err = stale.Write([]byte(" stale"))
	require.ErrorIs(t, err, oci.ErrRangeInvalid)
	require.Zero(t, n)
	_, err = w.Commit(ocidigest.FromBytes([]byte("wrong")))
	require.ErrorIs(t, err, oci.ErrDigestInvalid)
	require.EqualValues(t, 11, w.Size())
	require.NoError(t, w.Close())
	desc, err := w.Commit(ocidigest.FromBytes([]byte("hello world")))
	require.NoError(t, err)
	require.NoError(t, w.Cancel())
	br, err := r.GetBlob(t.Context(), "uploads", desc.Digest)
	require.Equal(t, "hello world", string(readContent(t, br, err)))
	_, err = r.PushBlobChunkedResume(t.Context(), "uploads", w.ID(), -1, 3)
	require.ErrorIs(t, err, oci.ErrBlobUploadUnknown)
	canceled, err := r.PushBlobChunked(t.Context(), "uploads", 0)
	require.NoError(t, err)
	_, err = canceled.Write([]byte("discard"))
	require.NoError(t, err)
	require.NoError(t, canceled.Cancel())
	require.NoError(t, canceled.Cancel())
	_, err = r.PushBlobChunkedResume(t.Context(), "uploads", canceled.ID(), -1, 0)
	require.ErrorIs(t, err, oci.ErrBlobUploadUnknown)
}
func TestUploadResume(t *testing.T) { exerciseUploads(t, newRegistry(t)) }

func collectHistory(t *testing.T, r *Registry, repo, tag string) ([]oci.Descriptor, error) {
	t.Helper()
	return oci.All(r.TagHistory(t.Context(), repo, tag, nil))
}
func exerciseConcurrentTags(t *testing.T, r *Registry) {
	t.Helper()
	pushTaggedIndex(t, r, "concurrent", "latest", "initial")
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := range 12 {
		wg.Go(func() {
			other := &Registry{client: r.client, bucket: r.bucket, prefix: r.prefix, partSize: r.partSize}
			data := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[],"annotations":{"version":%q}}`, oci.MediaTypeImageIndex, fmt.Sprint(i)))
			_, err := other.PushManifest(t.Context(), "concurrent", data, oci.MediaTypeImageIndex, &oci.PushManifestParameters{Tags: []string{"latest"}})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	history, err := collectHistory(t, r, "concurrent", "latest")
	require.NoError(t, err)
	require.Len(t, history, 13)
	for i := 1; i < len(history); i++ {
		require.True(t, historyTime(t, history[i-1]).After(historyTime(t, history[i])))
	}
	current, err := r.ResolveTag(t.Context(), "concurrent", "latest")
	require.NoError(t, err)
	require.Equal(t, history[0].Digest, current.Digest)
}
func TestConcurrentTags(t *testing.T) { exerciseConcurrentTags(t, newRegistry(t)) }

func TestHistoryFailureAndLostResponse(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprintf("after=%t", after), func(t *testing.T) {
			r := newRegistry(t)
			first := pushTaggedIndex(t, r, "example", "latest", "first")
			m := r.client.(*memoryObjects)
			m.failKey = r.repoKey("example", "_tags/latest")
			m.failAfter = after
			data := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[],"annotations":{"version":"second"}}`, oci.MediaTypeImageIndex))
			second, err := r.PushManifest(t.Context(), "example", data, oci.MediaTypeImageIndex, &oci.PushManifestParameters{Tags: []string{"latest"}})
			if after {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "injected PUT failure")
			}
			current, err := r.ResolveTag(t.Context(), "example", "latest")
			require.NoError(t, err)
			history, err := collectHistory(t, r, "example", "latest")
			require.NoError(t, err)
			if after {
				require.Equal(t, second, current)
				require.Len(t, history, 2)
			} else {
				require.Equal(t, first, current)
				require.Len(t, history, 1)
			}
		})
	}
}

func TestUploadLostResponse(t *testing.T) {
	r := newRegistry(t)
	w, err := r.PushBlobChunked(t.Context(), "example", 4)
	require.NoError(t, err)
	m := r.client.(*memoryObjects)
	m.failKey = r.repoKey("example", "_uploads/"+w.ID()+"/session")
	m.failAfter = true
	m.failPrecondition = true
	n, err := w.Write([]byte("data"))
	require.NoError(t, err)
	require.Equal(t, 4, n)
	require.EqualValues(t, 4, w.Size())
	desc, err := w.Commit(ocidigest.FromBytes([]byte("data")))
	require.NoError(t, err)
	br, err := r.GetBlob(t.Context(), "example", desc.Digest)
	require.Equal(t, "data", string(readContent(t, br, err)))
}

func TestTagLostResponseAfterSDKRetry(t *testing.T) {
	r := newRegistry(t)
	pushTaggedIndex(t, r, "example", "latest", "first")
	m := r.client.(*memoryObjects)
	m.failKey = r.repoKey("example", "_tags/latest")
	m.failAfter, m.failPrecondition = true, true
	second := pushTaggedIndex(t, r, "example", "latest", "second")
	history, err := collectHistory(t, r, "example", "latest")
	require.NoError(t, err)
	require.Len(t, history, 2, "a lost response followed by an SDK retry must not duplicate history")
	require.Equal(t, second.Digest, history[0].Digest)
}
