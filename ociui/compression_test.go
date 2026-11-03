package ociui

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ohseeeye/oci/ocimem"
)

type compressionCase struct {
	name, accept, method, contentType, encoding                 string
	body                                                        string
	status                                                      int
	flush, noCompression, contentLength, alreadyEncoded, ranges bool
}

func compressionCases() []compressionCase {
	large := strings.Repeat("<p>registry explorer</p>\n", 100)
	return []compressionCase{
		{name: "identity", body: large},
		{name: "explicit identity", accept: "identity", body: large},
		{name: "gzip HTML", accept: "gzip", body: large, encoding: "gzip"},
		{name: "gzip JSON", accept: "gzip", contentType: "application/json", body: large, encoding: "gzip"},
		{name: "gzip quality", accept: "gzip;q=0.5", body: large, encoding: "gzip"},
		{name: "gzip excluded", accept: "gzip;q=0", body: large},
		{name: "zstd only stays identity", accept: "zstd", body: large},
		{name: "zstd preference still gzip", accept: "zstd, gzip;q=0.5", body: large, encoding: "gzip"},
		{name: "zstd with gzip excluded", accept: "zstd, gzip;q=0", body: large},
		{name: "wildcard stays identity", accept: "*", body: large},
		{name: "small response", accept: "gzip", body: "small"},
		{name: "below threshold", accept: "gzip", body: strings.Repeat("x", 1023)},
		{name: "at threshold", accept: "gzip", body: strings.Repeat("x", 1024), encoding: "gzip"},
		{name: "known content length", accept: "gzip", body: large, contentLength: true, encoding: "gzip"},
		{name: "HEAD", method: http.MethodHead, accept: "gzip", body: large, contentLength: true},
		{name: "empty 204", accept: "gzip", status: http.StatusNoContent},
		{name: "empty 304", accept: "gzip", status: http.StatusNotModified},
		{name: "empty 200", accept: "gzip"},
		{name: "already encoded", accept: "gzip", body: large, alreadyEncoded: true, encoding: "br"},
		{name: "compressed content type", accept: "gzip", contentType: "application/gzip", body: large},
		{name: "image content type", accept: "gzip", contentType: "image/png", body: large, encoding: "gzip"},
		{name: "compression disabled", accept: "gzip", body: large, noCompression: true},
		{name: "range response", accept: "gzip", status: http.StatusPartialContent, body: large, ranges: true},
		{name: "streaming flush", accept: "gzip", body: large, flush: true, encoding: "gzip"},
		{name: "flush below threshold", accept: "gzip", body: "small streamed response", flush: true, encoding: "gzip"},
	}
}

func compressionFixture(tc compressionCase) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct := tc.contentType
		if ct == "" {
			ct = "text/html; charset=utf-8"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("ETag", `"fixture"`)
		w.Header().Add("Vary", "Origin")
		w.Header().Set("Accept-Ranges", "bytes")
		if tc.noCompression {
			w.Header().Set("No-Gzip-Compression", "true")
		}
		if tc.alreadyEncoded {
			w.Header().Set("Content-Encoding", "br")
		}
		if tc.contentLength {
			w.Header().Set("Content-Length", strconv.Itoa(len(tc.body)))
		}
		if tc.ranges {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(tc.body)-1, len(tc.body)))
		}
		status := tc.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if r.Method == http.MethodHead {
			return
		}
		if tc.flush {
			flusher, ok := w.(http.Flusher)
			if !ok {
				panic("compression middleware lost http.Flusher")
			}
			_, _ = io.WriteString(w, tc.body[:len(tc.body)/2])
			flusher.Flush()
			_, _ = io.WriteString(w, tc.body[len(tc.body)/2:])
			return
		}
		_, _ = io.WriteString(w, tc.body)
	})
}

func compressionResponse(t *testing.T, wrap func(http.Handler) http.Handler, tc compressionCase) (int, http.Header, []byte, bool) {
	t.Helper()
	method := tc.method
	if method == "" {
		method = http.MethodGet
	}
	req := httptest.NewRequest(method, "/", nil)
	req.Header.Set("Accept-Encoding", tc.accept)
	recorder := httptest.NewRecorder()
	wrap(compressionFixture(tc)).ServeHTTP(recorder, req)
	res := recorder.Result()
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		body, err = io.ReadAll(zr)
		if err != nil {
			t.Fatal(err)
		}
		if err := zr.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return res.StatusCode, res.Header, body, recorder.Flushed
}

func TestGzipHandlerCompatibility(t *testing.T) {
	for _, tc := range compressionCases() {
		t.Run(tc.name, func(t *testing.T) {
			status, headers, body, flushed := compressionResponse(t, gzipHandler, tc)
			wantStatus := tc.status
			if wantStatus == 0 {
				wantStatus = http.StatusOK
			}
			if status != wantStatus {
				t.Fatalf("status = %d, want %d", status, wantStatus)
			}
			if got := headers.Get("Content-Encoding"); got != tc.encoding {
				t.Errorf("encoding = %q, want %q", got, tc.encoding)
			}
			wantBody := tc.body
			if tc.method == http.MethodHead {
				wantBody = ""
			}
			if string(body) != wantBody {
				t.Errorf("decoded body differs: got %d bytes, want %d", len(body), len(wantBody))
			}
			if headers.Get("ETag") != `"fixture"` {
				t.Errorf("ETag changed: %v", headers)
			}
			vary := strings.Join(headers.Values("Vary"), ",")
			if !strings.Contains(vary, "Accept-Encoding") || !strings.Contains(vary, "Origin") {
				t.Errorf("Vary changed: %v", headers)
			}
			if headers.Get("No-Gzip-Compression") != "" {
				t.Error("internal compression header leaked")
			}
			if tc.encoding == "gzip" && (headers.Get("Content-Length") != "" || headers.Get("Accept-Ranges") != "") {
				t.Errorf("compressed response advertises original length/ranges: %v", headers)
			}
			if tc.ranges && headers.Get("Content-Range") == "" {
				t.Error("Content-Range lost")
			}
			if tc.flush && !flushed {
				t.Error("streaming response was not flushed")
			}
		})
	}
}

func TestExplorerHTTPCompression(t *testing.T) {
	handler := New(ocimem.New())
	var identity []byte
	for _, accept := range []string{"identity", "gzip", "zstd, gzip", "zstd"} {
		t.Run(accept, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("User-Agent", "dreg-compression-test")
			req.Header.Set("Accept-Encoding", accept)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			res := w.Result()
			defer res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", res.StatusCode)
			}
			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatal(err)
			}
			wantGzip := strings.Contains(accept, "gzip")
			if wantGzip {
				if res.Header.Get("Content-Encoding") != "gzip" {
					t.Fatalf("encoding = %q", res.Header.Get("Content-Encoding"))
				}
				zr, err := gzip.NewReader(bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				body, err = io.ReadAll(zr)
				if err != nil {
					t.Fatal(err)
				}
				if err := zr.Close(); err != nil {
					t.Fatal(err)
				}
			} else if res.Header.Get("Content-Encoding") != "" {
				t.Fatalf("unexpected encoding = %q", res.Header.Get("Content-Encoding"))
			}
			if accept == "identity" {
				identity = body
			} else if !bytes.Equal(body, identity) {
				t.Error("compression changed rendered catalog")
			}
		})
	}
}

type hijackResponseWriter struct {
	*httptest.ResponseRecorder
	conn net.Conn
}

func (w *hijackResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}

func TestGzipHandlerResponseController(t *testing.T) {
	for _, accept := range []string{"identity", "gzip"} {
		t.Run(accept, func(t *testing.T) {
			server, client := net.Pipe()
			defer server.Close()
			defer client.Close()
			writer := &hijackResponseWriter{ResponseRecorder: httptest.NewRecorder(), conn: server}
			handler := gzipHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, _, err := http.NewResponseController(w).Hijack()
				if err != nil || conn != server {
					t.Errorf("Hijack = %v, %v", conn, err)
				}
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Accept-Encoding", accept)
			handler.ServeHTTP(writer, req)
		})
	}
}
