package ocisqlite

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ocidigest"
)

func blobPath(dir string, digest oci.Digest) (string, error) {
	if err := digest.Validate(); err != nil {
		return "", fmt.Errorf("%w: %v", oci.ErrDigestInvalid, err)
	}
	return filepath.Join(dir, "blobs", digest.Algorithm().String(), digest.Encoded()), nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// writeBlob verifies even duplicate uploads. Publication never replaces an
// existing file, and file/directory syncs precede the metadata transaction.
func writeBlob(ctx context.Context, dir string, desc oci.Descriptor, content io.Reader) (oci.Descriptor, error) {
	if desc.Size < 0 {
		return oci.Descriptor{}, oci.ErrSizeInvalid
	}
	final, err := blobPath(dir, desc.Digest)
	if err != nil {
		return oci.Descriptor{}, err
	}
	f, err := os.CreateTemp(filepath.Join(dir, "uploads"), "blob-")
	if err != nil {
		return oci.Descriptor{}, err
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name()) // #nosec G703 -- path returned by os.CreateTemp in the trusted registry directory
	}()
	dw, err := ocidigest.NewWriter(f, desc.Digest.Algorithm())
	if err != nil {
		return oci.Descriptor{}, err
	}
	if _, err := io.Copy(dw, contextReader{ctx, content}); err != nil {
		return oci.Descriptor{}, err
	}
	if err := ctx.Err(); err != nil {
		return oci.Descriptor{}, err
	}
	if dw.Size() != desc.Size {
		return oci.Descriptor{}, oci.ErrSizeInvalid
	}
	got, err := dw.Digest()
	if err != nil {
		return oci.Descriptor{}, err
	}
	if got != desc.Digest {
		return oci.Descriptor{}, oci.ErrDigestInvalid
	}
	if err := f.Sync(); err != nil {
		return oci.Descriptor{}, err
	}
	if err := f.Close(); err != nil {
		return oci.Descriptor{}, err
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return oci.Descriptor{}, err
	} // #nosec G703 -- validated digest
	// Hard-link publication is atomic and refuses to overwrite existing content.
	if err := os.Link(f.Name(), final); err != nil { // #nosec G703 -- validated digest
		if !errors.Is(err, os.ErrExist) {
			return oci.Descriptor{}, err
		}
		info, err := os.Stat(final) // #nosec G703 -- validated digest
		if err != nil {
			return oci.Descriptor{}, err
		}
		if !info.Mode().IsRegular() || info.Size() != desc.Size {
			return oci.Descriptor{}, oci.ErrSizeInvalid
		}
	}
	for _, path := range []string{filepath.Dir(final), filepath.Join(dir, "blobs"), dir} {
		if err := syncDir(path); err != nil {
			return oci.Descriptor{}, err
		}
	}
	desc = oci.Descriptor{Digest: desc.Digest, Size: desc.Size, MediaType: "application/octet-stream"}
	return desc, nil
}

func syncDir(path string) error {
	f, err := os.Open(path) // #nosec G703 -- trusted registry directory or validated digest
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

type blobReader struct {
	ctx    context.Context
	reader io.Reader
	file   *os.File
	desc   oci.Descriptor
}

func (r *blobReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
func (r *blobReader) Close() error               { return r.file.Close() }
func (r *blobReader) Descriptor() oci.Descriptor { return r.desc }

func (r *Registry) openBlob(ctx context.Context, desc oci.Descriptor, start, end int64) (oci.BlobReader, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if end < 0 || end > desc.Size {
		end = desc.Size
	}
	if start < 0 || start > end {
		return nil, oci.ErrRangeInvalid
	}
	path, err := blobPath(r.dir, desc.Digest)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path) // #nosec G703 -- validated digest
	if err != nil {
		return nil, err
	}
	return &blobReader{ctx: ctx, reader: io.NewSectionReader(f, start, end-start), file: f, desc: desc}, nil
}

func newUploadID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf[:])
}

func validUploadID(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == 16 && hex.EncodeToString(b) == id
}
