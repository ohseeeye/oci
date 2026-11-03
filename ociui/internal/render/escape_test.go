package render

import (
	"bytes"
	"errors"
	"html"
	"io"
	"strings"
	"testing"
)

func TestEscapeWriter(t *testing.T) {
	allBytes := make([]byte, 256)
	for i := range allBytes {
		allBytes[i] = byte(i)
	}
	for _, input := range []string{"", "plain text 日本語", `&<>"'`, string(allBytes), strings.Repeat(`<tag a="日本語">&</tag>`, 1000)} {
		for _, chunkSize := range []int{1, 7, 4096} {
			var got bytes.Buffer
			w := NewEscapeWriter(&got)
			for start := 0; start < len(input); start += chunkSize {
				end := min(start+chunkSize, len(input))
				before := got.Len()
				n, err := w.Write([]byte(input[start:end]))
				if err != nil || n != end-start {
					t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, end-start)
				}
				// Every Write must be visible without a final Close or Flush.
				if want := html.EscapeString(input[start:end]); string(got.Bytes()[before:]) != want {
					t.Fatalf("chunk size %d, offset %d: got %q, want %q", chunkSize, end, got.Bytes()[before:], want)
				}
			}
			if want := html.EscapeString(input); got.String() != want {
				t.Fatal("escaped output differs from html.EscapeString")
			}
			if n, err := w.Write(nil); n != 0 || err != nil {
				t.Fatalf("empty Write = (%d, %v)", n, err)
			}
		}
	}
}

type failingWriter struct {
	err error
}

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestEscapeWriterErrors(t *testing.T) {
	failure := errors.New("destination failed")
	for _, tc := range []struct {
		name  string
		input string
		err   error
		wantN int
	}{
		{"flush", `<&>`, failure, 3},
		{"buffer full", strings.Repeat("x", 8192), failure, 4096},
		{"short write", "text", nil, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := NewEscapeWriter(failingWriter{tc.err})
			wantErr := tc.err
			if wantErr == nil {
				wantErr = io.ErrShortWrite
			}
			n, err := w.Write([]byte(tc.input))
			if n != tc.wantN || !errors.Is(err, wantErr) {
				t.Fatalf("Write = (%d, %v), want (%d, %v)", n, err, tc.wantN, wantErr)
			}
			if n, err := w.Write([]byte("more")); n != 0 || !errors.Is(err, wantErr) {
				t.Fatalf("Write after failure = (%d, %v), want (0, %v)", n, err, wantErr)
			}
		})
	}
}
