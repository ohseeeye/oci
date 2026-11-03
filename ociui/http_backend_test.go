package ociui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/ociclient"
	"github.com/ohseeeye/oci/ocimem"
	"github.com/ohseeeye/oci/ociserver"
	"github.com/ohseeeye/oci/pkg/ociauth"
	"github.com/ohseeeye/oci/pkg/ocidigest"
)

func testHTTPRegistry(t *testing.T, host string, config ociauth.Config, agent string) oci.Registry {
	t.Helper()
	reg, err := ociclient.New(host, &ociclient.Options{Insecure: true, UserAgent: agent,
		Transport: ociauth.NewStdTransport(ociauth.StdTransportParams{Config: config})})
	if err != nil {
		t.Fatal(err)
	}
	return reg
}
func TestHTTPBackend(t *testing.T) {
	t.Setenv("CACHE_DIR", t.TempDir())
	mem := ocimem.New()
	image, cfg := pushTestImage(t, mem, "team/widget", "v1", "v2", "v3")
	serve, err := ociserver.New(mem, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(serve)
	defer server.Close()
	h := New(testHTTPRegistry(t, strings.TrimPrefix(server.URL, "http://"), nil, "backend-test")).(*handler)
	for _, tc := range []struct{ path, want string }{
		{"/team/widget:v1", image.Digest.String()},
		{"/team/widget@" + image.Digest.String(), cfg.Digest.String()},
		{"/team/widget/blobs/" + cfg.Digest.String() + "?mt=" + oci.MediaTypeImageConfig, "hello-boundary"},
		{"/team/widget/tags/list", "v3"}, {"/", "team/widget"},
		{"/team/widget/referrers/" + image.Digest.String(), "manifests"},
	} {
		body := backendRequest(t, h, tc.path)
		if !strings.Contains(body, tc.want) {
			t.Fatalf("GET %s: missing %s\n%s", tc.path, tc.want, body)
		}
		if strings.Contains(body, strings.TrimPrefix(server.URL, "http://")) {
			t.Fatal("registry host leaked into local links")
		}
	}
	repo := "team/widget"
	for _, tc := range []struct {
		query string
		tags  []string
		next  string
	}{
		{"n=2", []string{"v1", "v2"}, "v2"}, {"n=2&next=v2", []string{"v3"}, ""},
	} {
		got, err := h.listTags(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/?"+tc.query, nil), repo)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Tags, tc.tags) || got.Next != tc.next {
			t.Fatalf("%s: %+v", tc.query, got)
		}
	}
}
func TestHTTPRangeReads(t *testing.T) {
	for _, supportsRange := range []bool{false, true} {
		t.Run(fmt.Sprint(supportsRange), func(t *testing.T) {
			t.Setenv("CACHE_DIR", t.TempDir())
			data := "0123456789"
			digest := ocidigest.FromBytes([]byte(data))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/v2/") {
					http.Redirect(w, r, "/storage/data", http.StatusTemporaryRedirect)
					return
				}
				if r.Header.Get("User-Agent") != "range-test" {
					t.Errorf("user agent: %q", r.Header.Get("User-Agent"))
				}
				if r.Header.Get("Range") != "bytes=4-7" {
					t.Errorf("range: %q", r.Header.Get("Range"))
				}
				if supportsRange {
					w.Header().Set("Content-Range", "bytes 4-7/10")
					w.Header().Set("Content-Length", "4")
					w.WriteHeader(206)
					io.WriteString(w, data[4:8])
				} else {
					io.WriteString(w, data)
				}
			}))
			defer server.Close()
			h := New(testHTTPRegistry(t, strings.TrimPrefix(server.URL, "http://"), nil, "range-test")).(*handler)
			ref, err := parseDigestReference("team/widget@" + digest.String())
			if err != nil {
				t.Fatal(err)
			}
			blob, err := h.registryBlob(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), ref)
			if err != nil {
				t.Fatal(err)
			}
			rc, err := blob.Reader(context.Background(), 4, 8)
			if err != nil {
				t.Fatal(err)
			}
			defer rc.Close()
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "4567" {
				t.Fatalf("wrong range bytes %q", got)
			}
		})
	}
}
func TestHTTPAuthTransport(t *testing.T) {
	t.Setenv("CACHE_DIR", t.TempDir())
	mem := ocimem.New()
	image, _ := pushTestImage(t, mem, "team/widget", "v1")
	serve, err := ociserver.New(mem, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "user" || pass != "pass" {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			http.Error(w, "authentication required", 401)
			return
		}
		serve.ServeHTTP(w, r)
	}))
	defer server.Close()
	h := New(testHTTPRegistry(t, strings.TrimPrefix(server.URL, "http://"), ociauth.NewStatic("user", "pass"), "auth-test"))
	if body := backendRequest(t, h, "/team/widget:v1"); !strings.Contains(body, image.Digest.String()) {
		t.Fatalf("authenticated fetch failed: %s", body)
	}
}
func TestHTTPFilesystemIsLazy(t *testing.T) {
	t.Setenv("CACHE_DIR", t.TempDir())
	fr := newFakeRegistry(t)
	archive := buildTarGz(t, []tarFile{{name: "hello.txt", body: "hello over HTTP"}})
	digest := fr.addBlob("team/widget", archive)
	h := New(testHTTPRegistry(t, fr.Host(), nil, "filesystem-test"))
	base := "/fs/team/widget@" + digest + "/"
	query := fmt.Sprintf("?mt=application/vnd.oci.image.layer.v1.tar%%2Bgzip&size=%d", len(archive))
	backendRequest(t, h, base+query)
	fr.mu.Lock()
	before := len(fr.requests)
	fr.mu.Unlock()
	backendRequest(t, h, base+query)
	fr.mu.Lock()
	after := len(fr.requests)
	fr.mu.Unlock()
	if after != before {
		t.Fatal("cached directory listing made HTTP requests")
	}
	if body := backendRequest(t, h, base+"hello.txt"+query); !strings.Contains(body, "hello over HTTP") {
		t.Fatalf("bad file read: %s", body)
	}
}
func TestAnonymousBearerFlow(t *testing.T) {
	t.Setenv("CACHE_DIR", t.TempDir())
	mem := ocimem.New()
	image, cfg := pushTestImage(t, mem, "team/widget", "v1")
	serve, err := ociserver.New(mem, nil)
	if err != nil {
		t.Fatal(err)
	}
	var tokens atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokens.Add(1)
			if r.URL.Query().Get("scope") != "repository:team/widget:pull" {
				t.Errorf("token scope: %s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"token": "anonymous-token", "expires_in": 3600})
			return
		}
		if r.Header.Get("Authorization") != "Bearer anonymous-token" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="test",scope="repository:team/widget:pull"`, server.URL))
			http.Error(w, "authentication required", 401)
			return
		}
		serve.ServeHTTP(w, r)
	}))
	defer server.Close()
	h := New(testHTTPRegistry(t, strings.TrimPrefix(server.URL, "http://"), nil, "bearer-test")).(*handler)
	if body := backendRequest(t, h, "/team/widget:v1"); !strings.Contains(body, image.Digest.String()) {
		t.Fatalf("bearer flow failed: %s", body)
	}
	ref, err := parseDigestReference("team/widget@" + cfg.Digest.String())
	if err != nil {
		t.Fatal(err)
	}
	blob, err := h.registryBlob(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), ref)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := blob.Reader(context.Background(), 1, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil || len(data) != 4 {
		t.Fatalf("authenticated range: %q, %v", data, err)
	}
	if tokens.Load() != 1 {
		t.Fatalf("token not reused: %d", tokens.Load())
	}
}
func TestRegistryImageEndpoint(t *testing.T) {
	t.Setenv("CACHE_DIR", t.TempDir())
	mem := ocimem.New()
	content := []byte("image bytes")
	desc := pushTestBlob(t, mem, "team/widget", content, "image/png")
	h := New(mem)
	ref := "team/widget@" + desc.Digest.String()
	page := backendRequest(t, h, "/fs/"+ref+"?mt=image/png")
	raw := "/raw/" + ref + "?mt=image%2Fpng"
	if !strings.Contains(page, raw) {
		t.Fatalf("missing local image URL: %s", page)
	}
	req := httptest.NewRequest(http.MethodGet, raw, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Body.String() != string(content) || rec.Header().Get("Content-Type") != "image/png" || rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("raw image: %v %s", rec.Header(), rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodHead, raw, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Body.Len() != 0 {
		t.Fatal("HEAD sent a body")
	}
	req = httptest.NewRequest(http.MethodGet, "/raw/"+ref+"?mt=text/html", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatal("raw HTML accepted")
	}
}
