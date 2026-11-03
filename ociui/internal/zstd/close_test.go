package zstd

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

type closeTrackingReader struct {
	io.Reader
	name   string
	events *[]string
	err    error
}

func (r *closeTrackingReader) Close() error {
	*r.events = append(*r.events, r.name)
	return r.err
}

func TestDecompressedReaderClosesDecoderThenSource(t *testing.T) {
	decoderErr, sourceErr := errors.New("decoder failed"), errors.New("source close failed")
	var events []string
	decoder := &closeTrackingReader{Reader: strings.NewReader("decoded"), name: "decoder", events: &events, err: decoderErr}
	source := &closeTrackingReader{name: "source", events: &events, err: sourceErr}
	reader := &decompressedReadCloser{ReadCloser: decoder, source: source}
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != "decoded" {
		t.Fatalf("ReadAll = %q, %v", data, err)
	}
	if len(events) != 0 {
		t.Fatal("reading closed a resource")
	}
	if err := reader.Close(); err != sourceErr {
		t.Fatalf("Close = %v, want source error", err)
	}
	if !reflect.DeepEqual(events, []string{"decoder", "source"}) {
		t.Fatalf("close order = %v", events)
	}
}

func TestUnzipReadCloserRetainsSourceCloser(t *testing.T) {
	const payload = "compressed payload"
	compressed := ReadCloser(io.NopCloser(strings.NewReader(payload)))
	encoded, err := io.ReadAll(compressed)
	if err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	for _, read := range []bool{false, true} {
		var events []string
		sourceErr := errors.New("source close failed")
		source := &closeTrackingReader{Reader: bytes.NewReader(encoded), name: "source", events: &events, err: sourceErr}
		reader, err := UnzipReadCloser(source)
		if err != nil {
			t.Fatal(err)
		}
		if read {
			data, err := io.ReadAll(reader)
			if err != nil || string(data) != payload {
				t.Fatalf("ReadAll = %q, %v", data, err)
			}
		}
		if len(events) != 0 {
			t.Fatal("decompression closed the source early")
		}
		if err := reader.Close(); err != sourceErr {
			t.Fatalf("Close = %v", err)
		}
		if !reflect.DeepEqual(events, []string{"source"}) {
			t.Fatalf("source close events = %v", events)
		}
	}
}
