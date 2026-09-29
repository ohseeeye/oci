package ocitransfer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"testing"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/ocimem"
	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/stretchr/testify/require"
)

// Embedding only Writer verifies that uploads do not require a full Registry.
type uploadWriter struct {
	oci.Writer
	start func(context.Context, string, int) (oci.BlobWriter, error)
}

func (w uploadWriter) PushBlobChunked(ctx context.Context, repo string, chunkSize int) (oci.BlobWriter, error) {
	return w.start(ctx, repo, chunkSize)
}

type ownedSource struct {
	io.Reader
	closed bool
}

func (r *ownedSource) Close() error {
	r.closed = true
	return nil
}

func TestUploadBlobRoundTripAndSourceOwnership(t *testing.T) {
	for _, size := range []int{0, 4, 5, 17} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			data := bytes.Repeat([]byte("x"), size)
			source := &ownedSource{Reader: bytes.NewReader(data)}
			reg := ocimem.New()
			var writer oci.Writer = reg
			desc, err := UploadBlob(t.Context(), writer, "repo", source, &UploadOptions{ChunkSize: 4, Algorithm: ocidigest.SHA512})
			require.NoError(t, err)
			require.False(t, source.closed)
			require.Equal(t, ocidigest.SHA512.FromBytes(data), desc.Digest)
			require.EqualValues(t, len(data), desc.Size)
			blob, err := reg.GetBlob(t.Context(), "repo", desc.Digest)
			require.NoError(t, err)
			defer blob.Close()
			got, err := io.ReadAll(blob)
			require.NoError(t, err)
			require.Equal(t, data, got)
		})
	}
}

type failingBlobWriter struct {
	oci.BlobWriter
	writes    int
	canceled  bool
	committed bool
	n         int
	err       error
	commitErr error
}

func (w *failingBlobWriter) Write([]byte) (int, error) {
	w.writes++
	return w.n, w.err
}

func (w *failingBlobWriter) Cancel() error {
	w.canceled = true
	return nil
}

func (w *failingBlobWriter) Commit(oci.Digest) (oci.Descriptor, error) {
	w.committed = true
	return oci.Descriptor{}, w.commitErr
}

func TestUploadBlobDoesNotReplayFailedOrShortWrites(t *testing.T) {
	failure := errors.New("write interrupted after accepting bytes")
	for _, test := range []struct {
		name string
		n    int
		err  error
		want error
	}{
		{"partial failure", 2, failure, failure},
		{"short write", 2, nil, io.ErrShortWrite},
	} {
		t.Run(test.name, func(t *testing.T) {
			bw := &failingBlobWriter{n: test.n, err: test.err}
			writer := uploadWriter{start: func(context.Context, string, int) (oci.BlobWriter, error) { return bw, nil }}
			source := &ownedSource{Reader: bytes.NewBufferString("blob")}
			_, err := UploadBlob(t.Context(), writer, "repo", source, &UploadOptions{ChunkSize: 4})
			require.ErrorIs(t, err, test.want)
			require.Equal(t, 1, bw.writes)
			require.True(t, bw.canceled)
			require.False(t, bw.committed)
			require.False(t, source.closed)
		})
	}
}

type errorSource struct{ err error }

func (r errorSource) Read([]byte) (int, error) { return 0, r.err }

func TestUploadBlobSourceFailureCancelsUpload(t *testing.T) {
	failure := errors.New("source unavailable")
	bw := &failingBlobWriter{}
	writer := uploadWriter{start: func(context.Context, string, int) (oci.BlobWriter, error) { return bw, nil }}
	source := &ownedSource{Reader: errorSource{err: failure}}
	_, err := UploadBlob(t.Context(), writer, "repo", source, &UploadOptions{ChunkSize: 4})
	require.ErrorIs(t, err, failure)
	require.True(t, bw.canceled)
	require.False(t, bw.committed)
	require.False(t, source.closed)
}

func TestUploadBlobCanceledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// No writer is needed because cancellation is checked before starting a session.
	_, err := UploadBlob(ctx, nil, "repo", bytes.NewReader(nil), nil)
	require.ErrorIs(t, err, context.Canceled)
}

func TestUploadBlobRejectsNegativeChunkSize(t *testing.T) {
	_, err := UploadBlob(t.Context(), nil, "repo", bytes.NewReader(nil), &UploadOptions{ChunkSize: -1})
	require.Error(t, err)
}

func TestUploadBlobCommitFailureCancelsUpload(t *testing.T) {
	failure := errors.New("commit rejected")
	bw := &failingBlobWriter{commitErr: failure}
	writer := uploadWriter{start: func(context.Context, string, int) (oci.BlobWriter, error) { return bw, nil }}
	_, err := UploadBlob(t.Context(), writer, "repo", bytes.NewReader(nil), &UploadOptions{ChunkSize: 4})
	require.ErrorIs(t, err, failure)
	require.True(t, bw.committed)
	require.True(t, bw.canceled)
}

type cancelSource struct {
	cancel context.CancelFunc
}

func (r cancelSource) Read(buf []byte) (int, error) {
	r.cancel()
	return copy(buf, "blob"), nil
}

func TestUploadBlobCancellationAfterSourceRead(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	bw := &failingBlobWriter{n: 4}
	writer := uploadWriter{start: func(context.Context, string, int) (oci.BlobWriter, error) { return bw, nil }}
	_, err := UploadBlob(ctx, writer, "repo", cancelSource{cancel: cancel}, &UploadOptions{ChunkSize: 4})
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, bw.writes)
	require.True(t, bw.canceled)
	require.False(t, bw.committed)
}
