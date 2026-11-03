package soci

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

type trackedReader struct {
	io.Reader
	closed   int
	closeErr error
}

func (r *trackedReader) Close() error {
	r.closed++
	return r.closeErr
}

func TestPeekPreservesSourceCloser(t *testing.T) {
	closeErr := errors.New("source close failed")
	for _, partial := range []bool{false, true} {
		source := &trackedReader{Reader: strings.NewReader("plain content"), closeErr: closeErr}
		kind, original, _, err := Peek(source)
		if err != nil || kind != "" {
			t.Fatalf("Peek = %q, %v", kind, err)
		}
		if source.closed != 0 {
			t.Fatal("Peek closed its source")
		}
		if partial {
			buf := make([]byte, 1)
			if _, err := io.ReadFull(original, buf); err != nil {
				t.Fatal(err)
			}
		} else {
			data, err := io.ReadAll(original)
			if err != nil || string(data) != "plain content" {
				t.Fatalf("ReadAll = %q, %v", data, err)
			}
		}
		if source.closed != 0 {
			t.Fatal("reading closed the source")
		}
		if err := original.Close(); !errors.Is(err, closeErr) {
			t.Fatalf("Close = %v", err)
		}
		if source.closed != 1 {
			t.Fatalf("source closed %d times", source.closed)
		}
	}
}

func TestTarPeekPreservesSourceCloser(t *testing.T) {
	var encoded bytes.Buffer
	tw := tar.NewWriter(&encoded)
	if err := tw.WriteHeader(&tar.Header{Name: "empty", Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	closeErr := errors.New("tar source close failed")
	source := &trackedReader{Reader: bytes.NewReader(encoded.Bytes()), closeErr: closeErr}
	ok, reader, err := tarPeek(source)
	if err != nil || !ok {
		t.Fatalf("tarPeek = %v, %v", ok, err)
	}
	data, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(data, encoded.Bytes()) {
		t.Fatalf("buffered tar changed: %v", err)
	}
	if source.closed != 0 {
		t.Fatal("tarPeek closed the source")
	}
	if err := reader.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("Close = %v", err)
	}
	if source.closed != 1 {
		t.Fatalf("source closed %d times", source.closed)
	}
}

type memoryBlob struct {
	data     []byte
	reader   *trackedReader
	closeErr error
}

func (b *memoryBlob) Reader(_ context.Context, start, end int64) (io.ReadCloser, error) {
	b.reader = &trackedReader{Reader: bytes.NewReader(b.data[start:end]), closeErr: b.closeErr}
	return b.reader, nil
}

func TestExtractFileLimitsReadsAndClosesSource(t *testing.T) {
	closeErr := errors.New("blob source close failed")
	blob := &memoryBlob{data: []byte("beforewantedafter"), closeErr: closeErr}
	file := &TOCFile{Name: "file", Offset: 6, Size: 6}
	index, err := NewIndex(blob, &TOC{Type: "tar", Files: []TOCFile{*file}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := ExtractFile(t.Context(), index, blob, file)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != "wanted" {
		t.Fatalf("ReadAll = %q, %v", data, err)
	}
	if blob.reader.closed != 0 {
		t.Fatal("reading closed the source")
	}
	if err := reader.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("Close = %v", err)
	}
	if blob.reader.closed != 1 {
		t.Fatalf("source closed %d times", blob.reader.closed)
	}
}

func TestBufferedIndexWriterFinishesGzip(t *testing.T) {
	var encoded bytes.Buffer
	buffer := bufio.NewWriter(&encoded)
	encoder := gzip.NewWriter(buffer)
	writer := &bufferedIndexWriter{WriteCloser: encoder, buffer: buffer}
	const payload = "index contents"
	if _, err := io.WriteString(writer, payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != payload {
		t.Fatalf("incomplete gzip output: %q, %v", data, err)
	}
}

type failingIndexSink struct {
	events *[]string
	err    error
}

func (w failingIndexSink) Write([]byte) (int, error) {
	*w.events = append(*w.events, "flush")
	return 0, w.err
}

type failingIndexEncoder struct {
	io.Writer
	events *[]string
	err    error
}

func (w failingIndexEncoder) Close() error {
	*w.events = append(*w.events, "close")
	return w.err
}

func TestBufferedIndexWriterAttemptsBothCleanupSteps(t *testing.T) {
	closeErr, flushErr := errors.New("encoder close failed"), errors.New("buffer flush failed")
	var events []string
	buffer := bufio.NewWriter(failingIndexSink{events: &events, err: flushErr})
	if _, err := buffer.WriteString("pending data"); err != nil {
		t.Fatal(err)
	}
	encoder := failingIndexEncoder{Writer: buffer, events: &events, err: closeErr}
	writer := &bufferedIndexWriter{WriteCloser: encoder, buffer: buffer}
	err := writer.Close()
	if !errors.Is(err, closeErr) || !errors.Is(err, flushErr) {
		t.Fatalf("Close lost cleanup errors: %v", err)
	}
	if !reflect.DeepEqual(events, []string{"close", "flush"}) {
		t.Fatalf("cleanup order = %v", events)
	}
}
