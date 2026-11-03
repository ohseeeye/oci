package ociui

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/ohseeeye/dreg/internal/explore/internal/soci"
)

func TestDirCacheRangeReader(t *testing.T) {
	d := &dirCache{dir: t.TempDir()}
	const key = "sha256:test.0"
	if err := os.WriteFile(d.file(key)+".tar.gz", []byte("0123456789"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name           string
		offset, length int64
		want           string
	}{
		{"middle", 3, 3, "345"},
		{"end", 7, 3, "789"},
		{"past end", 8, 5, "89"},
		{"empty", 3, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc, err := d.RangeReader(context.Background(), key, tc.offset, tc.length)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = rc.Close() })
			b, err := io.ReadAll(rc)
			if err != nil || string(b) != tc.want {
				t.Fatalf("ReadAll = (%q, %v), want (%q, nil)", b, err, tc.want)
			}
			if err := rc.Close(); err != nil {
				t.Fatal(err)
			}
			// A no-op closer would return nil here and leak the file.
			if err := rc.Close(); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("second Close = %v, want os.ErrClosed", err)
			}
		})
	}
	if _, err := d.RangeReader(context.Background(), "missing", 0, 1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing range = %v, want os.ErrNotExist", err)
	}
}

func TestDirCacheRangeReaderClosesBeforeEOF(t *testing.T) {
	d := &dirCache{dir: t.TempDir()}
	if err := os.WriteFile(d.file("test")+".tar.gz", []byte("0123456789"), 0600); err != nil {
		t.Fatal(err)
	}
	rc, err := d.RangeReader(context.Background(), "test", 2, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := rc.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Read after Close = %v, want os.ErrClosed", err)
	}
}

type cacheCloseWriter struct {
	err    error
	closed bool
}

func (w *cacheCloseWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *cacheCloseWriter) Close() error {
	w.closed = true
	return w.err
}

func TestMultiWriterCloseJoinsErrors(t *testing.T) {
	first, second := errors.New("first close"), errors.New("second close")
	writers := []*cacheCloseWriter{{err: first}, {}, {err: second}}
	w := MultiWriter(writers[0], writers[1], writers[2])
	err := w.Close()
	if !errors.Is(err, first) || !errors.Is(err, second) || err.Error() != "first close\nsecond close" {
		t.Fatalf("Close = %v, want both errors in order", err)
	}
	for i, w := range writers {
		if !w.closed {
			t.Errorf("writer %d not closed", i)
		}
	}
	if err := MultiWriter(&cacheCloseWriter{}).Close(); err != nil {
		t.Fatalf("successful Close = %v", err)
	}
	if err := MultiWriter().Close(); err != nil {
		t.Fatalf("empty Close = %v", err)
	}
}

type putErrorCache struct {
	cache
	err    error
	called bool
}

func (c *putErrorCache) Put(context.Context, string, *soci.TOC) error {
	c.called = true
	return c.err
}

func TestMultiCachePutJoinsErrors(t *testing.T) {
	first, second := errors.New("first put"), errors.New("second put")
	caches := []*putErrorCache{{err: first}, {}, {err: second}}
	m := &multiCache{caches: []cache{caches[0], caches[1], caches[2]}}
	err := m.Put(context.Background(), "test", &soci.TOC{})
	if !errors.Is(err, first) || !errors.Is(err, second) || err.Error() != "first put\nsecond put" {
		t.Fatalf("Put = %v, want both errors in order", err)
	}
	for i, c := range caches {
		if !c.called {
			t.Errorf("cache %d not called", i)
		}
	}
	for _, m := range []*multiCache{{}, {caches: []cache{&putErrorCache{}}}} {
		if err := m.Put(context.Background(), "test", &soci.TOC{}); err != nil {
			t.Fatalf("successful Put = %v", err)
		}
	}
}
