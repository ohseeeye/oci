package ocimiddleware

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ocidigest"
)

type cacheFillingReader struct {
	oci.BlobReader
	owner      *cacheRegistry
	ctx        context.Context
	key        cacheKey
	generation uint64
	file       *os.File
	verifier   *ocidigest.Writer
	release    func()
	done       bool
	closed     bool
	terminal   error
}

func (r *cacheRegistry) stream(ctx context.Context, k cacheKey, generation uint64, reader oci.BlobReader, release func()) (oci.BlobReader, error) {
	desc := reader.Descriptor()
	if desc.Digest.String() != k.value || desc.Digest.Validate() != nil || desc.Size < 0 {
		_ = reader.Close()
		release()
		return nil, fmt.Errorf("%w: invalid upstream blob descriptor", oci.ErrDigestInvalid)
	}
	verifier, err := ocidigest.NewWriter(nil, desc.Digest.Algorithm())
	if err != nil {
		_ = reader.Close()
		release()
		return nil, err
	}
	f, err := os.CreateTemp(r.opts.TempDir, "oci-cache-*")
	r.report(err)
	return &cacheFillingReader{BlobReader: reader, owner: r, ctx: ctx, key: k, generation: generation, file: f, verifier: verifier, release: release}, nil
}

func (r *cacheFillingReader) cleanup() {
	if r.file != nil {
		_ = r.file.Close()
		_ = os.Remove(r.file.Name()) // #nosec G703 -- file was created by os.CreateTemp in the configured temporary directory.
		r.file = nil
	}
}

func (r *cacheFillingReader) finish(err error) {
	if r.done {
		return
	}
	r.done = true
	r.terminal = err
	r.cleanup()
	r.release()
}

func (r *cacheFillingReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, os.ErrClosed
	}
	if r.done {
		return 0, r.terminal
	}
	if err := r.ctx.Err(); err != nil {
		r.finish(err)
		return 0, err
	}
	n, err := r.BlobReader.Read(p)
	if n > 0 {
		if _, verifyErr := r.verifier.Write(p[:n]); verifyErr != nil {
			r.finish(verifyErr)
			return n, verifyErr
		}
		if r.file != nil {
			if _, writeErr := r.file.Write(p[:n]); writeErr != nil {
				r.owner.report(writeErr)
				r.cleanup()
			}
		}
		if r.verifier.Size() > r.Descriptor().Size {
			r.finish(oci.ErrSizeInvalid)
			return n, oci.ErrSizeInvalid
		}
	}
	if !errors.Is(err, io.EOF) {
		if err != nil {
			r.finish(err)
		}
		return n, err
	}
	desc := r.Descriptor()
	digest, verifyErr := r.verifier.Digest()
	if verifyErr != nil {
		err = verifyErr
	} else if r.verifier.Size() != desc.Size {
		err = oci.ErrSizeInvalid
	} else if digest != desc.Digest {
		err = oci.ErrDigestInvalid
	}
	if err == io.EOF && r.ctx.Err() != nil {
		err = r.ctx.Err()
	}
	if err == io.EOF && r.file != nil {
		_, cacheErr := r.file.Seek(0, io.SeekStart)
		if cacheErr == nil {
			s := r.owner.state(r.key.repo)
			s.mu.Lock()
			if s.generation == r.generation {
				_, cacheErr = r.owner.cache.PushBlob(r.ctx, r.key.repo, desc, r.file)
				if cacheErr == nil {
					delete(s.invalid, r.key)
				}
			}
			s.mu.Unlock()
		}
		r.owner.report(cacheErr)
	}
	r.finish(err)
	return n, err
}

func (r *cacheFillingReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	r.finish(os.ErrClosed)
	return r.BlobReader.Close()
}
