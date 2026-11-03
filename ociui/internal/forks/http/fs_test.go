package http

import (
	"archive/tar"
	"bytes"
	"errors"
	"html"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestServeContentSniffing(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantType string
	}{
		{"empty", "", "text/plain; charset=utf-8"},
		{"text", "plain text 日本語", "text/plain; charset=utf-8"},
		{"html", "<!DOCTYPE HTML><p>test</p>", "text/html; charset=utf-8"},
		{"xml", "<?xml version=\"1.0\"?>", "text/xml; charset=utf-8"},
		{"png", "\x89PNG\r\n\x1a\n", "image/png"},
		{"gzip", "\x1f\x8b\x08", "application/x-gzip"},
		{"zip", "PK\x03\x04", "application/zip"},
		{"wasm", "\x00asm", "application/wasm"},
		{"binary", "\x00\x01\x02", "application/octet-stream"},
		{"sniff limit", strings.Repeat("x", 512) + "\x00", "text/plain; charset=utf-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/file", nil)
			ServeContent(w, r, "", time.Time{}, strings.NewReader(tc.body), nil)
			if w.Code != http.StatusOK || w.Header().Get("Content-Type") != tc.wantType || w.Body.String() != tc.body {
				t.Fatalf("response = (%d, %q, %q), want (200, %q, %q)", w.Code, w.Header().Get("Content-Type"), w.Body.String(), tc.wantType, tc.body)
			}
		})
	}
}

func TestServeContentPreviewEscapesHTML(t *testing.T) {
	body := strings.Repeat(`<tag a="日本語">&'</tag>`, 500)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/file", nil)
	var detected string
	ServeContent(w, r, "", time.Time{}, bytes.NewReader([]byte(body)), func(w http.ResponseWriter, r *http.Request, ctype string) error {
		detected = ctype
		return nil
	})
	if detected != "text/plain; charset=utf-8" {
		t.Fatalf("detected = %q", detected)
	}
	if want := "<pre>" + html.EscapeString(body) + "</pre>\n</body>\n</html>\n"; w.Body.String() != want {
		t.Fatalf("preview differs from escaped body")
	}
}

type apkProbeFS struct {
	FileSystem
	file File
	err  error
}

func (f apkProbeFS) Open(string) (File, error) { return f.file, f.err }

type apkProbeFile struct {
	File
	reader  io.Reader
	info    fs.FileInfo
	statErr error
	read    bool
	closed  bool
}

func (f *apkProbeFile) Read(p []byte) (int, error) {
	f.read = true
	return f.reader.Read(p)
}
func (f *apkProbeFile) Stat() (fs.FileInfo, error) { return f.info, f.statErr }
func (f *apkProbeFile) Close() error               { f.closed = true; return nil }

func TestAPKOwnersOptionalAndClosed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		typeflag byte
		statErr  error
		read     bool
	}{
		{name: "regular", typeflag: tar.TypeReg, read: true},
		{name: "directory", typeflag: tar.TypeDir},
		{name: "symlink", typeflag: tar.TypeSymlink},
		{name: "stat error", statErr: errors.New("stat failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := &apkProbeFile{
				reader:  strings.NewReader("P:busybox\nF:bin\nR:hello\n\n"),
				info:    (&tar.Header{Name: "installed", Typeflag: tc.typeflag, Mode: 0644}).FileInfo(),
				statErr: tc.statErr,
			}
			owners, width := apkOwners(apkProbeFS{file: file})
			if file.read != tc.read || !file.closed {
				t.Fatalf("read=%v, want %v; closed=%v", file.read, tc.read, file.closed)
			}
			if tc.read {
				if owners["bin/hello"] != "busybox" || width != len("busybox") {
					t.Fatalf("APK ownership = %v, width=%d", owners, width)
				}
			} else if len(owners) != 0 || width != 0 {
				t.Fatalf("optional database returned owners: %v, width=%d", owners, width)
			}
		})
	}
	if owners, width := apkOwners(apkProbeFS{err: fs.ErrNotExist}); len(owners) != 0 || width != 0 {
		t.Fatal("missing APK database was not optional")
	}
}

func TestContainsDotDot(t *testing.T) {
	// This validation is still needed by archive symlink rendering even though
	// the unused native-filesystem ServeFile API has been removed.
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"../target", true}, {"dir/../target", true}, {`dir\..\target`, true},
		{"..", true}, {"dir/.../target", false}, {"dir/file..txt", false},
	} {
		if got := containsDotDot(tc.path); got != tc.want {
			t.Errorf("containsDotDot(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}
