package ociui

import (
	"net/http"

	"github.com/klauspost/compress/gzhttp"
)

// gzipHandler retains the copied middleware's gzip-only policy. The upstream
// GzipHandler now also enables zstd by default, so use an explicit wrapper.
func gzipHandler(next http.Handler) http.Handler {
	wrap, err := gzhttp.NewWrapper(gzhttp.EnableZstd(false))
	if err != nil {
		// Only fixed, library-provided options are used here.
		panic(err)
	}
	return wrap(next)
}
