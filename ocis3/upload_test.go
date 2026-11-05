package ocis3

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/stretchr/testify/require"
)

func exerciseUploads(t *testing.T, r *Registry) {
	t.Helper()
	for _, alg := range []ocidigest.Algorithm{ocidigest.SHA256, ocidigest.SHA512} {
		t.Run(alg.String(), func(t *testing.T) { exerciseUpload(t, r, alg) })
	}
}

func digestBytes(t *testing.T, data []byte, alg ocidigest.Algorithm) oci.Digest {
	t.Helper()
	dw, err := ocidigest.NewWriter(nil, alg)
	require.NoError(t, err)
	_, err = dw.Write(data)
	require.NoError(t, err)
	digest, err := dw.Digest()
	require.NoError(t, err)
	return digest
}

func exerciseUpload(t *testing.T, r *Registry, alg ocidigest.Algorithm) {
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
	desc, err := w.Commit(digestBytes(t, []byte("hello world"), alg))
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

func TestUploadCreationFailure(t *testing.T) {
	r := newRegistry(t)
	r.client.(*memoryObjects).failSuffix = "/session"
	w, err := r.PushBlobChunked(t.Context(), "example", 0)
	require.ErrorContains(t, err, "injected PUT failure")
	require.Nil(t, w, "an upload writer requires a successfully persisted session")
}

func storedSession(t *testing.T, r *Registry, repo, id string) uploadSession {
	t.Helper()
	var session uploadSession
	_, err := r.getJSON(t.Context(), r.repoKey(repo, "_uploads/"+id+"/session"), &session)
	require.NoError(t, err)
	return session
}

func assertSessionDigest(t *testing.T, session uploadSession, content []byte) {
	t.Helper()
	dw, err := ocidigest.NewWriterFromState(nil, session.DigestState)
	require.NoError(t, err)
	require.EqualValues(t, len(content), dw.Size())
	require.Equal(t, session.Size, dw.Size())
	for _, alg := range []ocidigest.Algorithm{ocidigest.SHA256, ocidigest.SHA512} {
		got, err := dw.DigestFor(alg)
		require.NoError(t, err)
		require.Equal(t, digestBytes(t, content, alg), got)
	}
}

func TestUploadDigestStateAtomicity(t *testing.T) {
	r := newRegistry(t)
	w, err := r.PushBlobChunked(t.Context(), "example", 127)
	require.NoError(t, err)
	assertSessionDigest(t, storedSession(t, r, "example", w.ID()), nil)
	// Cross both algorithms' block boundaries and resume partial hash blocks.
	prefix := []byte(strings.Repeat("p", 259))
	_, err = w.Write(prefix)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	before := storedSession(t, r, "example", w.ID())
	assertSessionDigest(t, before, prefix)
	m := r.client.(*memoryObjects)
	require.Zero(t, m.chunkGETs, "digest restoration must not read chunk bytes")
	w, err = r.PushBlobChunkedResume(t.Context(), "example", w.ID(), -1, 131)
	require.NoError(t, err)
	tail := []byte(strings.Repeat("t", 263))
	m.failKey = r.repoKey("example", "_uploads/"+w.ID()+"/session")
	n, err := w.Write(tail)
	require.ErrorContains(t, err, "injected PUT failure")
	require.Zero(t, n)
	require.EqualValues(t, len(prefix), w.Size())
	require.Equal(t, before, storedSession(t, r, "example", w.ID()), "failed publication must preserve hash state and offset together")
	n, err = w.Write(tail)
	require.NoError(t, err)
	require.Equal(t, len(tail), n)
	content := append(prefix, tail...)
	committed := storedSession(t, r, "example", w.ID())
	assertSessionDigest(t, committed, content)
	_, err = w.Commit(ocidigest.FromBytes([]byte("wrong")))
	require.ErrorIs(t, err, oci.ErrDigestInvalid)
	require.Equal(t, committed, storedSession(t, r, "example", w.ID()))
	require.Zero(t, m.chunkGETs, "bad digests must be rejected from persisted state before reading chunks")
	require.Zero(t, m.multipartStarts)
	_, err = w.Commit(digestBytes(t, content, ocidigest.SHA512))
	require.NoError(t, err)
	require.Equal(t, len(committed.Chunks), m.chunkGETs, "assembly reads committed chunks once and skips orphan chunks")
}

func TestUploadRejectsInvalidDigestState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*uploadSession)
	}{
		{"missing", func(s *uploadSession) { s.DigestState = ocidigest.State{} }},
		{"invalid payload", func(s *uploadSession) { s.DigestState.States[0].Payload = []byte("invalid") }},
		{"unsupported encoding", func(s *uploadSession) { s.DigestState.States[0].Encoding = "unknown" }},
		{"missing algorithm", func(s *uploadSession) { s.DigestState.States = s.DigestState.States[:1] }},
		{"unequal digest offsets", func(s *uploadSession) { s.DigestState.States[0].Offset++ }},
		{"session offset mismatch", func(s *uploadSession) { s.Size++ }},
		{"chunk offset mismatch", func(s *uploadSession) { s.Chunks[0].Size++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRegistry(t)
			w, err := r.PushBlobChunked(t.Context(), "example", 0)
			require.NoError(t, err)
			_, err = w.Write([]byte("data"))
			require.NoError(t, err)
			session := storedSession(t, r, "example", w.ID())
			tc.mutate(&session)
			_, err = r.putJSON(t.Context(), r.repoKey("example", "_uploads/"+w.ID()+"/session"), session, "", false)
			require.NoError(t, err)
			_, err = r.PushBlobChunkedResume(t.Context(), "example", w.ID(), -1, 0)
			require.ErrorIs(t, err, oci.ErrBlobUploadInvalid)
			require.Zero(t, r.client.(*memoryObjects).chunkGETs, "invalid states are rejected without a legacy rehash fallback")
		})
	}
}

func TestUploadRejectsChangedChunk(t *testing.T) {
	r := newRegistry(t)
	w, err := r.PushBlobChunked(t.Context(), "example", 0)
	require.NoError(t, err)
	_, err = w.Write([]byte("data"))
	require.NoError(t, err)
	chunk := storedSession(t, r, "example", w.ID()).Chunks[0]
	// Same size, different bytes: length checks alone would accept this.
	_, err = r.put(t.Context(), chunk.Key, []byte("evil"), "application/octet-stream", "", false)
	require.NoError(t, err)
	digest := ocidigest.FromBytes([]byte("data"))
	_, err = w.Commit(digest)
	require.True(t, conflict(err), "chunk GET must enforce its saved ETag: %v", err)
	_, err = r.ResolveBlob(t.Context(), "example", digest)
	require.ErrorIs(t, err, oci.ErrBlobUnknown)
}

func TestUploadDigestStateCommitRetry(t *testing.T) {
	r := newRegistry(t)
	w, err := r.PushBlobChunked(t.Context(), "example", 3)
	require.NoError(t, err)
	data := []byte("resumable commit")
	_, err = w.Write(data)
	require.NoError(t, err)
	digest := digestBytes(t, data, ocidigest.SHA512)
	key, err := r.membershipKey("example", digest)
	require.NoError(t, err)
	r.client.(*memoryObjects).failKey = key
	_, err = w.Commit(digest)
	require.ErrorContains(t, err, "injected PUT failure")
	session := storedSession(t, r, "example", w.ID())
	require.Equal(t, "committing", session.Status)
	require.Equal(t, digest, session.Digest)
	assertSessionDigest(t, session, data)
	other := &Registry{client: r.client, bucket: r.bucket, prefix: r.prefix, partSize: r.partSize}
	w, err = other.PushBlobChunkedResume(t.Context(), "example", w.ID(), -1, 3)
	require.NoError(t, err)
	_, err = w.Write([]byte("extra"))
	require.Error(t, err, "committing session is frozen")
	_, err = w.Commit(digest)
	require.NoError(t, err)
	br, err := r.GetBlob(t.Context(), "example", digest)
	require.Equal(t, data, readContent(t, br, err))
}

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
	assertSessionDigest(t, storedSession(t, r, "example", w.ID()), []byte("data"))
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
