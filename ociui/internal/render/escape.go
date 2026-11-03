// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be found
// in ../forks/LICENSE-Go.

package render

import (
	"bufio"
	"io"
)

// NewEscapeWriter returns a streaming HTML-escaping writer. Each Write flushes
// its output so that previews remain visible while their sources are loading.
// It does not close the underlying writer.
func NewEscapeWriter(w io.Writer) io.Writer {
	return &escapeWriter{buf: bufio.NewWriter(w)}
}

type escapeWriter struct {
	buf *bufio.Writer
}

var (
	amp = []byte("&amp;")
	lt  = []byte("&lt;")
	gt  = []byte("&gt;")
	dq  = []byte("&#34;")
	sq  = []byte("&#39;")
)

func (w *escapeWriter) Write(p []byte) (n int, err error) {
	for i, b := range p {
		switch b {
		case '&':
			_, err = w.buf.Write(amp)
		case '<':
			_, err = w.buf.Write(lt)
		case '>':
			_, err = w.buf.Write(gt)
		case '"':
			_, err = w.buf.Write(dq)
		case '\'':
			_, err = w.buf.Write(sq)
		default:
			err = w.buf.WriteByte(b)
		}
		if err != nil {
			return i, err
		}
	}
	return len(p), w.buf.Flush()
}
