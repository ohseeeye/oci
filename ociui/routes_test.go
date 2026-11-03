package ociui

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/ocimem"
)

func routeResponse(h http.Handler, method, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.Header.Set("Accept-Encoding", "identity")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRegistryURLBuilders(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{repoURL(""), "/"},
		{repoURL("org/team/widget"), "/org/team/widget/tags/list"},
		{imageURL("org/team/widget"), "/org/team/widget:latest"},
		{imageURL("org/team/widget:v1"), "/org/team/widget:v1"},
		{imageURL("org/team/widget@sha256:abc"), "/org/team/widget@sha256:abc"},
		{blobURL("org/team/widget:v1@sha256:abc"), "/org/team/widget/blobs/sha256:abc"},
		{contentURL(oci.MediaTypeImageConfig, "org/team/widget@sha256:abc"), "/org/team/widget/blobs/sha256:abc"},
		{contentURL(oci.MediaTypeImageLayerGzip, "org/team/widget@sha256:abc"), "/fs/org/team/widget@sha256:abc"},
		{contentURL("application/octet-stream", "org/team/widget@sha256:abc"), "/org/team/widget/blobs/sha256:abc"},
	} {
		if tc.got != tc.want {
			t.Errorf("URL = %q, want %q", tc.got, tc.want)
		}
	}
}

func TestRegistryUIPaths(t *testing.T) {
	t.Setenv("CACHE_DIR", t.TempDir())
	reg := ocimem.New()
	for _, repo := range []string{"widget", "blob", "blob/team", "http/team", "namespace/fs", "org/team/sub/widget", "example.com/team/widget", "team/tags/list", "team/blobs/widget/leaf", "team/referrers/widget/leaf", "team/v2/widget"} {
		image, config := pushTestImage(t, reg, repo, "v1", "v2", "latest")
		h := New(reg)
		for _, tc := range []struct{ target, want string }{
			{"/", `href="/` + repo + `/tags/list"`},
			{"/" + repo, `href="/` + repo + `:v1"`},
			{"/" + repo + "/tags/list", `href="/` + repo + `:v2"`},
			{"/" + repo + ":v1", config.Digest.String()},
			{"/" + repo + "@" + image.Digest.String(), config.Digest.String()},
			{"/" + repo + ":v1@" + image.Digest.String(), config.Digest.String()},
			{"/" + repo + "/blobs/" + config.Digest.String(), "hello-boundary"},
			{"/" + repo + "/referrers/" + image.Digest.String(), "manifests"},
			{"/" + repo + "/_oci/tag-history/v1", image.Digest.String()},
		} {
			// /team/tags/list deliberately selects the tags resource for team,
			// not the identically named repository. Its explicit tags URL works.
			if repo == "team/tags/list" && tc.target == "/"+repo {
				continue
			}
			t.Run(tc.target, func(t *testing.T) {
				rec := routeResponse(h, http.MethodGet, tc.target)
				if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tc.want) {
					t.Fatalf("GET %s: %d, missing %q\n%s", tc.target, rec.Code, tc.want, rec.Body.String())
				}
				if rec.Header().Get("Location") != "" {
					t.Fatal("canonical path unexpectedly redirected")
				}
			})
		}
	}
}

func TestCatalogForms(t *testing.T) {
	t.Setenv("CACHE_DIR", t.TempDir())
	h := New(&oci.Funcs{NewError: func(_ context.Context, method, repo string) error {
		t.Errorf("form navigation fetched %s(%q)", method, repo)
		return oci.ErrUnsupported
	}})
	for _, tc := range []struct{ query, path string }{
		{"image=org/team/widget:v1", "/org/team/widget:v1"},
		{"image=widget", "/widget:latest"},
		{"repo=org/team/widget", "/org/team/widget/tags/list"},
		{"repo=", "/"},
		{"image=%20org%2Fteam%2Fwidget%3Av1%20", "/org/team/widget:v1"},
	} {
		rec := routeResponse(h, http.MethodGet, "/_open?"+tc.query)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != tc.path {
			t.Errorf("form navigation %s: %d %q", tc.query, rec.Code, rec.Header().Get("Location"))
		}
	}
	for _, query := range []string{"image=https://evil.invalid/repo:v1", "repo=//evil.invalid", "image=UPPER:v1", "image=team/widget@sha256:bad"} {
		if rec := routeResponse(h, http.MethodGet, "/_open?"+query); rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
			t.Errorf("invalid form reference %s: %d %v", query, rec.Code, rec.Header())
		}
	}
}

func TestRegistryRouteValidationAndReservedPaths(t *testing.T) {
	t.Setenv("CACHE_DIR", t.TempDir())
	h := New(&oci.Funcs{NewError: func(_ context.Context, method, repo string) error {
		t.Errorf("invalid/reserved path reached registry: %s(%q)", method, repo)
		return oci.ErrUnsupported
	}})
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, target := range []string{
		"/UPPER", "/team//widget", "/team/../widget", "/team/widget/", "/team/widget:", "/team/widget:bad:tag",
		"/team/widget@sha256:bad", "/team/widget/tags/list@bad", "/team/widget/blobs/sha256:bad",
		"/team/widget/referrers/sha256:bad", "/team/widget:v1/blobs/" + digest,
		"/team/widget/_oci/tag-history/bad:tag", "/team/widget/_oci/tag-history/v1@" + digest,
		"/team/widget%3Av1%2Fextra", "/team/widget%3Av1%22onclick=x",
	} {
		if rec := routeResponse(h, http.MethodGet, target); rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s: status=%d, want 400", target, rec.Code)
		}
	}
	for _, target := range []string{"/fs", "/fs/", "/layers", "/layers/", "/size/", "/sizes/", "/cache/", "/raw/", "/zurl/"} {
		if rec := routeResponse(h, http.MethodGet, target); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status=%d, want 404", target, rec.Code)
		}
	}
	for _, target := range []string{"/", "/team/widget", "/team/widget:v1", "/team/widget/tags/list", "/team/widget/blobs/" + digest} {
		if rec := routeResponse(h, http.MethodPost, target); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: status=%d, want 405", target, rec.Code)
		}
	}
	for _, target := range []string{"/favicon.svg", "/favicon.ico", "/robots.txt"} {
		if rec := routeResponse(h, http.MethodGet, target); rec.Code != http.StatusOK {
			t.Errorf("asset %s: status=%d", target, rec.Code)
		}
	}
}

func TestRegistryResourceSuffixPrecedence(t *testing.T) {
	t.Setenv("CACHE_DIR", t.TempDir())
	reg := ocimem.New()
	pushTestImage(t, reg, "team", "parent")
	pushTestImage(t, reg, "team/tags/list", "child")
	h := New(reg)
	if body := backendRequest(t, h, "/team/tags/list"); !strings.Contains(body, `href="/team:parent"`) {
		t.Fatal("resource suffix did not take precedence over the repository alias")
	}
	if body := backendRequest(t, h, "/team/tags/list/tags/list"); !strings.Contains(body, `href="/team/tags/list:child"`) {
		t.Fatal("explicit tags URL did not reach the ambiguous repository")
	}
}

var hrefPattern = regexp.MustCompile(`href="([^"]+)"`)

func pageLinks(body string) []*url.URL {
	var links []*url.URL
	for _, match := range hrefPattern.FindAllStringSubmatch(body, -1) {
		if u, err := url.Parse(html.UnescapeString(match[1])); err == nil && u.Host == "" && u.Scheme == "" {
			links = append(links, u)
		}
	}
	return links
}

func TestRegistryTagIdentityAndViewLinks(t *testing.T) {
	t.Setenv("CACHE_DIR", t.TempDir())
	reg := ocimem.New()
	const repo = "org/team/widget"
	image, config := pushTestImage(t, reg, repo, "v1", "v2")
	h := New(reg)
	for _, target := range []string{"/" + repo + ":v1", "/" + repo + "@" + image.Digest.String()} {
		rec := routeResponse(h, http.MethodGet, target)
		if rec.Code != http.StatusOK || rec.Header().Get("Location") != "" {
			t.Fatalf("manifest changed browser location: %d %v", rec.Code, rec.Header())
		}
		if immutable := strings.Contains(rec.Header().Get("Cache-Control"), "immutable"); immutable != strings.Contains(target, "@") {
			t.Fatalf("cache policy for %s: %v", target, rec.Header())
		}
		var foundConfig, foundLayers, foundDigest bool
		for _, u := range pageLinks(rec.Body.String()) {
			switch u.Path {
			case "/" + repo + "/blobs/" + config.Digest.String():
				foundConfig = true
				if got := u.Query().Get("manifest"); got != repo+"@"+image.Digest.String() {
					t.Errorf("config association = %q", got)
				}
				if r := routeResponse(h, http.MethodGet, u.String()); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "hello-boundary") {
					t.Errorf("broken config link %s: %d", u, r.Code)
				}
			case "/layers/" + repo + "@" + image.Digest.String() + "/":
				foundLayers = true
			case "/" + repo + "@" + image.Digest.String():
				foundDigest = true
			}
		}
		if !foundConfig || !foundLayers || !foundDigest {
			t.Fatalf("missing links: config=%v layers=%v digest=%v", foundConfig, foundLayers, foundDigest)
		}
	}

	// View links retain the tag path and repeated jq stages, without smuggling
	// the resource identity back into the query string.
	viewManifest, err := json.Marshal(oci.IndexOrManifest{
		SchemaVersion: 2, MediaType: oci.MediaTypeImageManifest, Config: &config,
		Layers: []oci.Descriptor{}, Annotations: map[string]string{"dev.sigstore.cosign/bundle": `{"payload":"hello"}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.PushManifest(context.Background(), repo, viewManifest, oci.MediaTypeImageManifest, &oci.PushManifestParameters{Tags: []string{"vjson"}}); err != nil {
		t.Fatal(err)
	}
	body := backendRequest(t, h, "/"+repo+":vjson?jq=jq&jq=jq")
	found := false
	for _, u := range pageLinks(body) {
		if len(u.Query()["jq"]) > 2 {
			found = true
			if u.Path != "/"+repo+":vjson" || !reflect.DeepEqual(u.Query()["jq"][:2], []string{"jq", "jq"}) {
				t.Errorf("view link lost tag/stages: %s", u)
			}
		}
	}
	if !found {
		t.Fatal("no jq view links")
	}

	for _, target := range []string{"/" + repo, "/" + repo + "/tags/list"} {
		foundNext := false
		for _, u := range pageLinks(backendRequest(t, h, target+"?n=1")) {
			if u.Query().Get("next") == "v1" {
				foundNext = true
				if u.Path != target || u.Query().Get("n") != "1" {
					t.Errorf("pagination changed resource: %s", u)
				}
				if body := backendRequest(t, h, u.String()); !strings.Contains(body, "v2") {
					t.Error("next page missing v2")
				}
			}
		}
		if !foundNext {
			t.Fatalf("no next page link on %s", target)
		}
	}
}
