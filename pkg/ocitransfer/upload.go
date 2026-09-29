package ocitransfer

import (
	"context"
	"fmt"
	"io"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ocidigest"
)

const defaultUploadChunkSize = 100 * 1024 * 1024

// UploadOptions controls a sequential chunked upload. Zero values select defaults.
type UploadOptions struct {
	// ChunkSize is the buffer size and chunk-size hint passed to the writer.
	// The default is 100 MiB. The registry may choose a different chunk size.
	ChunkSize int
	// Algorithm determines the committed digest. Zero uses ocidigest.Canonical.
	Algorithm ocidigest.Algorithm
}

// UploadBlob reads source, writes sequential chunks, and commits their digest.
// A nil options value selects defaults. The caller owns source; UploadBlob never
// closes it. Any failure cancels the upload. Failed writes are not replayed,
// because the registry might already have accepted some bytes. This function
// does not resume sessions; use oci.Writer.PushBlobChunkedResume for that.
// Cancellation is checked between source reads; a blocking source must provide
// its own cancellation mechanism.
func UploadBlob(ctx context.Context, writer oci.Writer, repo string, source io.Reader, options *UploadOptions) (oci.Descriptor, error) {
	var opts UploadOptions
	if options != nil {
		opts = *options
	}
	if opts.ChunkSize < 0 {
		return oci.Descriptor{}, fmt.Errorf("upload chunk size must not be negative")
	}
	if opts.ChunkSize == 0 {
		opts.ChunkSize = defaultUploadChunkSize
	}
	if opts.Algorithm.String() == "" {
		opts.Algorithm = ocidigest.Canonical
	}
	digester, err := opts.Algorithm.New()
	if err != nil {
		return oci.Descriptor{}, fmt.Errorf("creating digester: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return oci.Descriptor{}, err
	}
	bw, err := writer.PushBlobChunked(ctx, repo, opts.ChunkSize)
	if err != nil {
		return oci.Descriptor{}, fmt.Errorf("starting upload: %w", err)
	}
	defer func() { _ = bw.Cancel() }()
	buf := make([]byte, opts.ChunkSize)
	for {
		n, readErr := io.ReadFull(contextReader{ctx: ctx, source: source}, buf)
		if err := ctx.Err(); err != nil {
			return oci.Descriptor{}, err
		}
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return oci.Descriptor{}, fmt.Errorf("reading source: %w", readErr)
		}
		if n > 0 {
			written, err := bw.Write(buf[:n])
			if err != nil {
				return oci.Descriptor{}, fmt.Errorf("writing upload: %w", err)
			}
			if written != n {
				return oci.Descriptor{}, fmt.Errorf("writing upload: %w", io.ErrShortWrite)
			}
			if _, err := digester.Write(buf[:n]); err != nil {
				return oci.Descriptor{}, err
			}
		}
		if readErr != nil {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return oci.Descriptor{}, err
	}
	digest, err := digester.Digest()
	if err != nil {
		return oci.Descriptor{}, err
	}
	desc, err := bw.Commit(digest)
	if err != nil {
		return oci.Descriptor{}, fmt.Errorf("committing upload: %w", err)
	}
	return desc, nil
}

type contextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r contextReader) Read(buf []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(buf)
}
