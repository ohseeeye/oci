package ocitransfer

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/ocimem"
	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/stretchr/testify/require"
)

// Embedding only Reader verifies that transfers do not require a full Registry.
type rangeReader struct {
	oci.Reader
	desc oci.Descriptor
	get  func(context.Context, int64, int64) (oci.BlobReader, error)
}

func (r rangeReader) ResolveBlob(context.Context, string, oci.Digest) (oci.Descriptor, error) {
	return r.desc, nil
}

func (r rangeReader) GetBlobRange(ctx context.Context, _ string, _ oci.Digest, start, end int64) (oci.BlobReader, error) {
	return r.get(ctx, start, end)
}

func testRangeReader(data []byte) rangeReader {
	desc := oci.Descriptor{Digest: ocidigest.FromBytes(data), Size: int64(len(data))}
	return rangeReader{
		desc: desc,
		get: func(_ context.Context, start, end int64) (oci.BlobReader, error) {
			return ocimem.NewBytesReader(data[start:end], desc), nil
		},
	}
}

func TestDownloadBlobSizesAndDefaults(t *testing.T) {
	for _, size := range []int{0, 17, int(probeSize) + 17} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			data := bytes.Repeat([]byte("x"), size)
			reader := testRangeReader(data)
			blob, err := DownloadBlob(t.Context(), reader, "repo", reader.desc.Digest, nil)
			require.NoError(t, err)
			defer blob.Close()
			require.Equal(t, reader.desc, blob.Descriptor())
			got, err := io.ReadAll(blob)
			require.NoError(t, err)
			require.Equal(t, data, got)
			_, err = blob.Read(make([]byte, 1))
			require.ErrorIs(t, err, io.EOF)
		})
	}
}

func TestDownloadBlobOrderedAndBounded(t *testing.T) {
	data := []byte("abcdefghijklmnop")
	reader := testRangeReader(data)
	firstStarted, secondFinished := make(chan struct{}), make(chan struct{})
	var active, peak atomic.Int32
	reader.get = func(ctx context.Context, start, end int64) (oci.BlobReader, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		switch start {
		case 0:
			close(firstStarted)
			select {
			case <-secondFinished:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		case 4:
			select {
			case <-firstStarted:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			close(secondFinished)
		}
		return ocimem.NewBytesReader(data[start:end], reader.desc), nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	blob, err := DownloadBlob(ctx, reader, "repo", reader.desc.Digest, &DownloadOptions{ChunkSize: 4, Concurrency: 2})
	require.NoError(t, err)
	defer blob.Close()
	got, err := io.ReadAll(blob)
	require.NoError(t, err)
	require.Equal(t, data, got)
	require.EqualValues(t, 2, peak.Load())
}

func TestDownloadBlobVerifiesDigest(t *testing.T) {
	reader := testRangeReader([]byte("good"))
	reader.get = func(context.Context, int64, int64) (oci.BlobReader, error) {
		return ocimem.NewBytesReader([]byte("evil"), reader.desc), nil
	}
	blob, err := DownloadBlob(t.Context(), reader, "repo", reader.desc.Digest, nil)
	require.NoError(t, err)
	defer blob.Close()
	got, err := io.ReadAll(blob)
	require.Equal(t, []byte("evil"), got)
	require.ErrorIs(t, err, oci.ErrDigestInvalid)
}

func TestDownloadBlobRetriesRanges(t *testing.T) {
	for _, attempts := range []int{1, 2} {
		t.Run(strconv.Itoa(attempts), func(t *testing.T) {
			reader := testRangeReader([]byte("blob"))
			var calls atomic.Int32
			reader.get = func(context.Context, int64, int64) (oci.BlobReader, error) {
				if calls.Add(1) == 1 {
					return ocimem.NewBytesReader([]byte("bl"), reader.desc), nil
				}
				return ocimem.NewBytesReader([]byte("blob"), reader.desc), nil
			}
			blob, err := DownloadBlob(t.Context(), reader, "repo", reader.desc.Digest, &DownloadOptions{MaxAttempts: attempts})
			require.NoError(t, err)
			defer blob.Close()
			got, err := io.ReadAll(blob)
			if attempts == 1 {
				require.ErrorIs(t, err, io.ErrUnexpectedEOF)
			} else {
				require.NoError(t, err)
				require.Equal(t, []byte("blob"), got)
			}
			require.EqualValues(t, attempts, calls.Load())
		})
	}
}

func TestDownloadBlobRangeTimeout(t *testing.T) {
	reader := testRangeReader([]byte("blob"))
	reader.get = func(ctx context.Context, _, _ int64) (oci.BlobReader, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	blob, err := DownloadBlob(t.Context(), reader, "repo", reader.desc.Digest, &DownloadOptions{RangeTimeout: time.Millisecond, MaxAttempts: 1})
	require.NoError(t, err)
	defer blob.Close()
	_, err = io.ReadAll(blob)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestDownloadBlobCloseCancelsPrefetch(t *testing.T) {
	reader := testRangeReader([]byte("abcdefghijkl"))
	started := make(chan struct{}, 3)
	var active atomic.Int32
	reader.get = func(ctx context.Context, _, _ int64) (oci.BlobReader, error) {
		active.Add(1)
		defer active.Add(-1)
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	blob, err := DownloadBlob(ctx, reader, "repo", reader.desc.Digest, &DownloadOptions{ChunkSize: 4, Concurrency: 3})
	require.NoError(t, err)
	defer blob.Close()
	for range 3 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("prefetch did not start")
		}
	}
	require.NoError(t, blob.Close())
	require.Zero(t, active.Load())
}

func TestDownloadBlobCancelUnblocksUnreadPipe(t *testing.T) {
	reader := testRangeReader([]byte("blob"))
	fetched := make(chan struct{})
	reader.get = func(context.Context, int64, int64) (oci.BlobReader, error) {
		close(fetched)
		return ocimem.NewBytesReader([]byte("blob"), reader.desc), nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	blob, err := DownloadBlob(ctx, reader, "repo", reader.desc.Digest, nil)
	require.NoError(t, err)
	defer blob.Close()
	select {
	case <-fetched:
	case <-time.After(5 * time.Second):
		t.Fatal("range not fetched")
	}
	cancel()
	select {
	case <-blob.(*blobReader).done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not unblock pipeline")
	}
	_, err = io.ReadAll(blob)
	require.ErrorIs(t, err, context.Canceled)
}

func TestDownloadBlobRejectsInvalidInputs(t *testing.T) {
	reader := testRangeReader([]byte("blob"))
	for _, opts := range []*DownloadOptions{
		{Concurrency: -1}, {ChunkSize: -1}, {MaxAttempts: -1}, {RangeTimeout: -1},
	} {
		_, err := DownloadBlob(t.Context(), reader, "repo", reader.desc.Digest, opts)
		require.Error(t, err)
	}
	_, err := DownloadBlob(t.Context(), reader, "repo", "invalid", nil)
	require.Error(t, err)
	wrong := reader
	wrong.desc.Digest = ocidigest.FromBytes([]byte("other"))
	_, err = DownloadBlob(t.Context(), wrong, "repo", reader.desc.Digest, nil)
	require.ErrorIs(t, err, oci.ErrDigestInvalid)
	wrong = reader
	wrong.desc.Size = -1
	_, err = DownloadBlob(t.Context(), wrong, "repo", reader.desc.Digest, nil)
	require.ErrorIs(t, err, oci.ErrSizeInvalid)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = DownloadBlob(ctx, reader, "repo", reader.desc.Digest, nil)
	require.ErrorIs(t, err, context.Canceled)
}
