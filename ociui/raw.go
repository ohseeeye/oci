package ociui

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

// serveRawBlob is the HTTP adapter for the <img> URLs emitted by renderImage.
// GetBlob supplies bytes to this Go process; the browser needs a separate HTTP
// response containing those bytes, rather than the explorer's HTML wrapper.
// No signed URL discovery or registry credentials are exposed to the browser.
func (h *handler) serveRawBlob(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return nil
	}
	mt := r.URL.Query().Get("mt")
	if !strings.HasPrefix(mt, "image/") || strings.ContainsAny(mt, "\r\n") {
		http.Error(w, "raw endpoint only serves image media types", http.StatusBadRequest)
		return nil
	}
	ref, err := parseDigestReference(strings.TrimPrefix(r.URL.Path, "/raw/"))
	if err != nil {
		return err
	}
	blob, err := h.openBlob(w, r, ref)
	if err != nil {
		return err
	}
	defer blob.Close()
	w.Header().Set("Content-Type", mt)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// In particular, direct navigation to a registry-supplied SVG must not run
	// scripts with the explorer's origin privileges.
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("Cache-Control", "private, max-age=3600, immutable")
	if size := blob.Descriptor().Size; size >= 0 {
		w.Header().Set("Content-Length", fmt.Sprint(size))
	}
	if r.Method == http.MethodHead {
		return nil
	}
	_, err = io.Copy(w, blob)
	return err
}
