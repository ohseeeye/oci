package ocitransfer

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ocidigest"
)

const (
	probeSize           int64 = 4 * 1024 * 1024
	minChunkSize        int64 = 4 * 1024 * 1024
	maxChunkSize        int64 = 256 * 1024 * 1024
	targetChunkDuration       = 2 * time.Second
)

// DownloadOptions controls range fetching. Zero values select defaults.
type DownloadOptions struct {
	// Concurrency is the maximum number of prefetched ranges. The default is 6.
	Concurrency int
	// ChunkSize is the range size in bytes. Zero measures an initial 4 MiB probe
	// and selects a size between 4 and 256 MiB. Small blobs use a single range.
	ChunkSize int64
	// MaxAttempts is the number of attempts per range, including the first.
	// The default is 3. A value of 1 disables retries.
	MaxAttempts int
	// RangeTimeout limits each range attempt, including reading its body.
	// The default is 30 seconds.
	RangeTimeout time.Duration
}

func downloadOptions(options *DownloadOptions) (DownloadOptions, error) {
	var opts DownloadOptions
	if options != nil {
		opts = *options
	}
	if opts.Concurrency < 0 || opts.ChunkSize < 0 || opts.MaxAttempts < 0 || opts.RangeTimeout < 0 {
		return opts, fmt.Errorf("download options must not be negative")
	}
	if opts.Concurrency == 0 {
		opts.Concurrency = 6
	}
	if opts.MaxAttempts == 0 {
		opts.MaxAttempts = 3
	}
	if opts.RangeTimeout == 0 {
		opts.RangeTimeout = 30 * time.Second
	}
	return opts, nil
}

// DownloadBlob returns an ordered stream of the blob using concurrent range
// reads. A nil options value selects adaptive chunk sizing and other defaults.
// The reader's descriptor is resolved before this function returns; range
// failures are reported by Read. Digest and size verification complete only
// when the caller reads to EOF. The caller must close the reader even on error.
// Closing it or canceling ctx cancels outstanding requests and stops prefetching.
// The underlying reader must support concurrent, context-aware range reads.
func DownloadBlob(ctx context.Context, reader oci.Reader, repo string, digest oci.Digest, options *DownloadOptions) (oci.BlobReader, error) {
	opts, err := downloadOptions(options)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := digest.Validate(); err != nil {
		return nil, fmt.Errorf("invalid digest: %w", err)
	}
	desc, err := reader.ResolveBlob(ctx, repo, digest)
	if err != nil {
		return nil, fmt.Errorf("resolving blob: %w", err)
	}
	if desc.Digest != digest {
		return nil, fmt.Errorf("resolved digest does not match requested digest: %w", oci.ErrDigestInvalid)
	}
	if desc.Size < 0 {
		return nil, fmt.Errorf("negative blob size: %w", oci.ErrSizeInvalid)
	}
	digester, err := digest.Algorithm().New()
	if err != nil {
		return nil, fmt.Errorf("creating digester: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		stop := context.AfterFunc(ctx, func() { _ = pw.CloseWithError(ctx.Err()) })
		err := downloadPipeline(ctx, reader, repo, digest, desc.Size, opts, pw)
		stop()
		_ = pw.CloseWithError(err)
	}()
	return &blobReader{r: pr, desc: desc, digester: digester, cancel: cancel, done: done}, nil
}

func deriveChunkSize(probeBytes int64, elapsed time.Duration) int64 {
	if elapsed <= 0 {
		return maxChunkSize
	}
	// Clamp before converting to int64 so extremely fast probes cannot overflow.
	size := float64(probeBytes) * float64(targetChunkDuration) / float64(elapsed)
	if size >= float64(maxChunkSize) {
		return maxChunkSize
	}
	return max(minChunkSize, int64(size))
}

func fetchRange(ctx context.Context, reader oci.Reader, repo string, digest oci.Digest, start, end int64, opts DownloadOptions) ([]byte, error) {
	var lastErr error
	for range opts.MaxAttempts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, opts.RangeTimeout)
		br, err := reader.GetBlobRange(attemptCtx, repo, digest, start, end)
		var data []byte
		if err == nil {
			data, err = io.ReadAll(io.LimitReader(br, end-start))
			closeErr := br.Close()
			if err == nil {
				err = closeErr
			}
			if err == nil {
				err = attemptCtx.Err()
			}
			if err == nil && int64(len(data)) != end-start {
				err = fmt.Errorf("short range read: got %d bytes, want %d: %w", len(data), end-start, io.ErrUnexpectedEOF)
			}
		}
		cancel()
		if err == nil {
			return data, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("range [%d, %d) failed after %d attempts: %w", start, end, opts.MaxAttempts, lastErr)
}

type chunkResult struct {
	data []byte
	err  error
}

func downloadPipeline(ctx context.Context, reader oci.Reader, repo string, digest oci.Digest, size int64, opts DownloadOptions, out io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
	}()
	var offset int64
	chunkSize := opts.ChunkSize
	if chunkSize == 0 {
		if size <= probeSize {
			chunkSize = max(size, 1)
		} else {
			start := time.Now()
			data, err := fetchRange(ctx, reader, repo, digest, 0, probeSize, opts)
			if err != nil {
				return fmt.Errorf("bandwidth probe: %w", err)
			}
			chunkSize = deriveChunkSize(int64(len(data)), time.Since(start))
			if _, err := out.Write(data); err != nil {
				return err
			}
			offset = int64(len(data))
		}
	}
	if offset == size {
		return ctx.Err()
	}
	count := (size-offset-1)/chunkSize + 1
	window := int(min(int64(opts.Concurrency), count))
	slots := make([]chan chunkResult, window)
	launch := func(index int64) {
		start := offset + index*chunkSize
		end := start + min(chunkSize, size-start)
		ch := make(chan chunkResult, 1)
		slots[index%int64(window)] = ch
		workers.Add(1)
		go func() {
			defer workers.Done()
			data, err := fetchRange(ctx, reader, repo, digest, start, end, opts)
			ch <- chunkResult{data: data, err: err}
		}()
	}
	for i := range window {
		launch(int64(i))
	}
	next := int64(window)
	for i := range count {
		var result chunkResult
		select {
		case <-ctx.Done():
			return ctx.Err()
		case result = <-slots[i%int64(window)]:
		}
		if result.err != nil {
			return result.err
		}
		if _, err := out.Write(result.data); err != nil {
			return err
		}
		if next < count {
			launch(next)
			next++
		}
	}
	return ctx.Err()
}

type blobReader struct {
	r        *io.PipeReader
	desc     oci.Descriptor
	digester ocidigest.Digester
	n        int64
	terminal error
	cancel   context.CancelFunc
	done     <-chan struct{}
}

func (r *blobReader) Descriptor() oci.Descriptor { return r.desc }

func (r *blobReader) Read(buf []byte) (int, error) {
	if r.terminal != nil {
		return 0, r.terminal
	}
	n, err := r.r.Read(buf)
	r.n += int64(n)
	if _, hashErr := r.digester.Write(buf[:n]); hashErr != nil {
		err = hashErr
	}
	if r.n > r.desc.Size {
		err = fmt.Errorf("blob exceeds declared size: %w", oci.ErrSizeInvalid)
	}
	if err == io.EOF {
		if r.n != r.desc.Size {
			err = fmt.Errorf("blob size %d, want %d: %w", r.n, r.desc.Size, oci.ErrSizeInvalid)
		} else if got, hashErr := r.digester.Digest(); hashErr != nil {
			err = hashErr
		} else if got != r.desc.Digest {
			err = fmt.Errorf("blob digest mismatch: %w", oci.ErrDigestInvalid)
		}
	}
	if err != nil {
		r.terminal = err
	}
	return n, err
}

func (r *blobReader) Close() error {
	r.cancel()
	err := r.r.Close()
	<-r.done
	return err
}
