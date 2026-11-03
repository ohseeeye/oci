package soci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"testing"

	"github.com/klauspost/compress/zstd"
)

type checkpointBlob struct {
	data []byte
}

func (b checkpointBlob) Reader(_ context.Context, start, end int64) (io.ReadCloser, error) {
	if end == -1 {
		end = int64(len(b.data))
	}
	if start < 0 || end < start || end > int64(len(b.data)) {
		return nil, fmt.Errorf("invalid range [%d, %d) for %d bytes", start, end, len(b.data))
	}
	return io.NopCloser(bytes.NewReader(b.data[start:end])), nil
}

func TestIndexAndExtractAcrossCheckpoints(t *testing.T) {
	// Fresh and repeated data produce enough deflate blocks and dictionary
	// matches to exercise restored mid-stream checkpoints, not just the first span.
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	random := rand.New(rand.NewSource(1))
	files := make([][]byte, 20)
	for i := range files {
		files[i] = make([]byte, 32<<10)
		if _, err := random.Read(files[i]); err != nil {
			t.Fatal(err)
		}
		copy(files[i][16<<10:], files[i][:16<<10])
		if err := tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("file-%02d", i), Mode: 0644, Size: int64(len(files[i]))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(files[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	for _, kind := range []string{"tar", "tar+gzip", "tar+zstd"} {
		t.Run(kind, func(t *testing.T) {
			var encoded bytes.Buffer
			switch kind {
			case "tar":
				encoded.Write(archive.Bytes())
			case "tar+gzip":
				zw := gzip.NewWriter(&encoded)
				if _, err := zw.Write(archive.Bytes()); err != nil {
					t.Fatal(err)
				}
				if err := zw.Close(); err != nil {
					t.Fatal(err)
				}
			case "tar+zstd":
				zw, err := zstd.NewWriter(&encoded)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := zw.Write(archive.Bytes()); err != nil {
					t.Fatal(err)
				}
				if err := zw.Close(); err != nil {
					t.Fatal(err)
				}
			}
			var indexArchive bytes.Buffer
			indexer, detected, _, _, err := NewIndexer(io.NopCloser(bytes.NewReader(encoded.Bytes())), &indexArchive, 16<<10, "")
			if err != nil || indexer == nil || detected != kind {
				t.Fatalf("NewIndexer = (%v, %q, %v)", indexer, detected, err)
			}
			t.Cleanup(func() { _ = indexer.Close() })
			for {
				_, err := indexer.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := indexer.TOC(); err != nil {
				t.Fatal(err)
			}
			// Reload from the stored index, rather than reusing in-memory state.
			index, err := NewIndex(checkpointBlob{indexArchive.Bytes()}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "tar+gzip" {
				last := &index.TOC().Files[len(files)-1]
				cp := index.TOC().Checkpoint(last)
				if cp.index == 0 || cp.Checkpoint.IsEmpty() {
					t.Fatal("fixture did not exercise a mid-stream deflate checkpoint")
				}
			}
			for i, want := range files {
				file, err := index.Locate(fmt.Sprintf("file-%02d", i))
				if err != nil {
					t.Fatal(err)
				}
				rc, err := ExtractFile(t.Context(), index, checkpointBlob{encoded.Bytes()}, file)
				if err != nil {
					t.Fatal(err)
				}
				got, readErr := io.ReadAll(rc)
				closeErr := rc.Close()
				if readErr != nil || closeErr != nil || !bytes.Equal(got, want) {
					t.Fatalf("file %d: %d bytes, read error %v, close error %v; want original %d bytes", i, len(got), readErr, closeErr, len(want))
				}
			}
		})
	}
}
