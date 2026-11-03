// Modified for dreg: local imports; see UPSTREAM.md.

package ociui

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

// Pretends to implement Seek because ServeContent only cares about checking
// for the size by calling Seek(0, io.SeekEnd)
type sizeSeeker struct {
	rc   io.Reader
	size int64
}

func (s *sizeSeeker) Seek(offset int64, whence int) (int64, error) {
	return 0, fmt.Errorf("not implemented")
}

func (s *sizeSeeker) Read(p []byte) (int, error) {
	return s.rc.Read(p)
}

func (s *sizeSeeker) Size() int64 {
	return s.size
}

type sizeBlob struct {
	rc   io.ReadCloser
	size int64

	n int64

	w http.ResponseWriter

	progress int
	total    int
	started  bool
	start    time.Time
}

func (s *sizeBlob) Read(p []byte) (int, error) {
	n, err := s.rc.Read(p)
	s.n += int64(n)

	if s.started && s.w != nil {
		s.flushProgress()
	}
	return n, err
}

func (s *sizeBlob) StartLoading() {
	s.started = true
	if s.start.IsZero() {
		s.start = time.Now()
	}
	if flusher, ok := s.w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (s *sizeBlob) StopLoading() {
	s.started = false
}

func (s *sizeBlob) LoadingDuration() time.Duration {
	if s.start.IsZero() {
		return 0
	}
	return time.Since(s.start)
}

func (s *sizeBlob) flushProgress() {
	if s.w == nil || s.size <= 0 {
		return
	}
	next := int(float64(s.total) * (float64(s.n) / float64(s.size)))
	if next > s.progress {
		fmt.Fprintf(s.w, "<span slot=\"progress\">.</span>\n")
		s.progress = next
	}
	if flusher, ok := s.w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (s *sizeBlob) Size() int64 {
	return s.size
}

func (s *sizeBlob) Close() error {
	return s.rc.Close()
}
