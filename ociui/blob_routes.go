package ociui

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ociref"
)

// The repo capture cannot cross the ':' in a digest. Greediness can therefore
// resolve nested repos containing /blobs/ but cannot consume an archive path
// containing another /blobs/<digest>. Keep this boundary aligned with routes().
const (
	blobRepoExpression   = `[^:@]+`
	blobDigestExpression = `[^/@:]+:[^/@]+`
)

var blobPathPattern = regexp.MustCompile(`^/(` + blobRepoExpression + `)/blobs/(` + blobDigestExpression + `)(?:/|$)`)

func parseBlobPath(path string) (ociref.Reference, string, error) {
	parts := blobPathPattern.FindStringSubmatch(path)
	if parts == nil {
		return ociref.Reference{}, "", fmt.Errorf("invalid blob path %q", path)
	}
	ref, err := parseDigestReference(parts[1] + "@" + parts[2])
	if err != nil || ref.Repository != parts[1] || ref.Tag != "" {
		return ociref.Reference{}, "", fmt.Errorf("invalid blob reference in %q", path)
	}
	return ref, "/" + parts[1] + "/blobs/" + parts[2], nil
}

// renderBlob is the single entry point for structured blob data, binary/text
// previews, images, and auto-detected archives. /fs/ remains the explicit layer
// filesystem endpoint; merged filesystem and size endpoints are unchanged.
func (h *handler) renderBlob(w http.ResponseWriter, r *http.Request) error {
	ref, prefix, err := parseBlobPath(r.URL.Path)
	if err != nil {
		http.Error(w, "invalid blob reference", http.StatusBadRequest)
		return nil
	}
	archivePath := strings.TrimPrefix(r.URL.Path, prefix)
	for _, part := range strings.Split(archivePath, "/") {
		if part == "." || part == ".." {
			http.Error(w, "invalid archive path", http.StatusBadRequest)
			return nil
		}
	}
	if archivePath != "" && archivePath != "/" {
		return h.renderFS(w, r)
	}

	query := r.URL.Query()
	mt := query.Get("mt")
	if contentKindForMT(mt) == "json" || query.Has("jq") || (query.Get("render") != "" && query.Get("render") != "elf") {
		return h.renderBlobJSON(w, r, ref.String())
	}
	if mt != "" && mt != "application/octet-stream" {
		return h.renderFS(w, r)
	}

	// No useful hint: inspect a bounded prefix without consuming it or fetching
	// the blob twice. Preserve both the descriptor and the original closer.
	blob, err := h.openBlob(w, r, ref)
	if err != nil {
		return err
	}
	buffered := &previewBlob{BlobReader: blob, reader: bufio.NewReader(blob)}
	sample, err := buffered.reader.Peek(512)
	if err != nil && err != io.EOF {
		blob.Close()
		return err
	}
	if mt == "" || mt == "application/octet-stream" {
		mt = blob.Descriptor().MediaType
		if mt == "" || mt == "application/octet-stream" {
			mt = http.DetectContentType(sample)
		}
	}
	r = r.Clone(r.Context())
	query.Set("mt", mt)
	r.URL.RawQuery = query.Encode()
	trimmed := bytes.TrimSpace(sample)
	if contentKindForMT(mt) == "json" || (len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[')) {
		return h.renderBlobJSONContent(w, r, ref, buffered)
	}
	return h.renderFSWithBlob(w, r, &sizeBlob{rc: buffered, size: blobSize(r, blob.Descriptor().Size)})
}

type previewBlob struct {
	oci.BlobReader
	reader *bufio.Reader
}

func (b *previewBlob) Read(p []byte) (int, error) { return b.reader.Read(p) }
