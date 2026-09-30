package ocisqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/stretchr/testify/require"
	"zombiezen.com/go/sqlite"
)

func TestUploadResumeAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	r, err := New(dir, nil)
	require.NoError(t, err)
	w, err := r.PushBlobChunked(t.Context(), "example", 4096)
	require.NoError(t, err)
	require.Equal(t, 4096, w.ChunkSize())
	_, err = w.Write([]byte("first"))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	require.NoError(t, w.Close())
	require.NoError(t, r.Close())
	r = newRegistry(t, dir)
	_, err = r.PushBlobChunkedResume(t.Context(), "example", w.ID(), 1, 0)
	require.ErrorIs(t, err, oci.ErrRangeInvalid)
	pushBlob(t, r, "other", "other")
	_, err = r.PushBlobChunkedResume(t.Context(), "other", w.ID(), -1, 0)
	require.ErrorIs(t, err, oci.ErrBlobUploadUnknown)
	_, err = r.PushBlobChunkedResume(t.Context(), "example", "../../escape", -1, 0)
	require.ErrorIs(t, err, oci.ErrBlobUploadUnknown)
	_, err = r.PushBlobChunkedResume(t.Context(), "example", newUploadID(), -1, 0)
	require.ErrorIs(t, err, oci.ErrBlobUploadUnknown)
	// Simulate bytes written and synced before an interrupted SQLite commit.
	path := filepath.Join(dir, "uploads", w.ID())
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.Write([]byte("uncommitted"))
	require.NoError(t, err)
	require.NoError(t, f.Close())
	resumed, err := r.PushBlobChunkedResume(t.Context(), "example", w.ID(), -1, 0)
	require.NoError(t, err)
	require.EqualValues(t, 5, resumed.Size())
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "first", string(data))
	_, err = resumed.Write([]byte("second"))
	require.NoError(t, err)
	_, err = resumed.Commit(ocidigest.FromBytes([]byte("wrong")))
	require.ErrorIs(t, err, oci.ErrDigestInvalid)
	desc, err := resumed.Commit(ocidigest.FromBytes([]byte("firstsecond")))
	require.NoError(t, err)
	require.NoError(t, resumed.Cancel())
	require.NoFileExists(t, path)
	_, err = r.PushBlobChunkedResume(t.Context(), "example", w.ID(), -1, 0)
	require.ErrorIs(t, err, oci.ErrBlobUploadUnknown)
	br, err := r.GetBlob(t.Context(), "example", desc.Digest)
	require.Equal(t, []byte("firstsecond"), readContent(t, br, err))
}

func TestStaleUploadWriterAndCancel(t *testing.T) {
	dir := t.TempDir()
	r := newRegistry(t, dir)
	other := newRegistry(t, dir)
	w, err := r.PushBlobChunked(t.Context(), "example", 0)
	require.NoError(t, err)
	stale, err := other.PushBlobChunkedResume(t.Context(), "example", w.ID(), 0, 0)
	require.NoError(t, err)
	_, err = w.Write([]byte("content"))
	require.NoError(t, err)
	_, err = stale.Write([]byte("bad"))
	require.ErrorIs(t, err, oci.ErrRangeInvalid)
	_, err = stale.Commit(ocidigest.FromBytes([]byte("")))
	require.ErrorIs(t, err, oci.ErrRangeInvalid)
	require.NoError(t, w.Cancel())
	require.NoError(t, w.Cancel())
	require.NoError(t, stale.Cancel())
	require.NoFileExists(t, filepath.Join(dir, "uploads", w.ID()))
	ctx, cancel := context.WithCancel(t.Context())
	w, err = r.PushBlobChunked(ctx, "example", 0)
	require.NoError(t, err)
	cancel()
	_, err = w.Write([]byte("canceled"))
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, w.Cancel())
}

func TestUploadCommitRollbackPreservesPublishedContent(t *testing.T) {
	r := newRegistry(t, t.TempDir())
	w, err := r.PushBlobChunked(t.Context(), "example", 0)
	require.NoError(t, err)
	_, err = w.Write([]byte("first"))
	require.NoError(t, err)
	digest := ocidigest.FromBytes([]byte("first"))
	require.NoError(t, r.withConn(t.Context(), true, func(conn *sqlite.Conn) error {
		return execute(conn, `CREATE TRIGGER reject_membership BEFORE INSERT ON repository_blob BEGIN SELECT RAISE(ABORT, 'injected membership failure'); END`)
	}))
	_, err = w.Commit(digest)
	require.ErrorContains(t, err, "injected membership failure")
	_, err = r.ResolveBlob(t.Context(), "example", digest)
	require.ErrorIs(t, err, oci.ErrBlobUnknown)
	require.NoError(t, r.withConn(t.Context(), true, func(conn *sqlite.Conn) error { return execute(conn, "DROP TRIGGER reject_membership") }))
	resumed, err := r.PushBlobChunkedResume(t.Context(), "example", w.ID(), -1, 0)
	require.NoError(t, err)
	_, err = resumed.Write([]byte("second"))
	require.NoError(t, err)
	// A failed metadata commit may have published an orphan file. Extending
	// the resumable upload must never modify that immutable content's inode.
	path, err := blobPath(r.dir, digest)
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "first", string(data))
	_, err = resumed.Commit(ocidigest.FromBytes([]byte("firstsecond")))
	require.NoError(t, err)
	require.NoError(t, resumed.Cancel())
}
