package ociui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/ocimem"
	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/ohseeeye/oci/pkg/ociref"
)

func pushTestBlob(t *testing.T, reg oci.Registry, repo string, body []byte, mt string) oci.Descriptor {
	t.Helper()
	desc := oci.Descriptor{Digest: ocidigest.FromBytes(body), Size: int64(len(body)), MediaType: mt}
	got, err := reg.PushBlob(context.Background(), repo, desc, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return got
}
func pushTestImage(t *testing.T, reg oci.Registry, repo string, tags ...string) (oci.Descriptor, oci.Descriptor) {
	t.Helper()
	cfg := pushTestBlob(t, reg, repo, []byte(`{"architecture":"amd64","os":"linux","config":{"Cmd":["hello-boundary"]},"rootfs":{"type":"layers","diff_ids":[]}}`), oci.MediaTypeImageConfig)
	body, err := json.Marshal(oci.IndexOrManifest{SchemaVersion: 2, MediaType: oci.MediaTypeImageManifest, Config: &cfg, Layers: []oci.Descriptor{}})
	if err != nil {
		t.Fatal(err)
	}
	image, err := reg.PushManifest(context.Background(), repo, body, oci.MediaTypeImageManifest, &oci.PushManifestParameters{Tags: tags})
	if err != nil {
		t.Fatal(err)
	}
	return image, cfg
}
func backendRequest(t *testing.T, h http.Handler, target string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Accept-Encoding", "identity")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d\n%s", target, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func TestRegistryBoundaryMemory(t *testing.T) {
	t.Setenv("CACHE_DIR", t.TempDir())
	reg := ocimem.New()
	image, cfg := pushTestImage(t, reg, "team/widget", "v1", "v2", "v3")
	pushTestImage(t, reg, "team/another", "latest")
	bare, _ := pushTestImage(t, reg, "ubuntu", "latest")
	// This is a literal repository namespace, not a routing instruction.
	dotted, _ := pushTestImage(t, reg, "example.com/team/widget", "v1")
	h := New(reg)
	for _, tc := range []struct{ target, want string }{
		{"/team/widget:v1", image.Digest.String()},
		{"/team/widget@" + image.Digest.String(), cfg.Digest.String()},
		{"/team/widget/blobs/" + cfg.Digest.String() + "?mt=" + oci.MediaTypeImageConfig, "hello-boundary"},
		{"/team/widget/tags/list", "v3"},
		{"/", "team/another"},
		{"/team/widget/referrers/" + image.Digest.String(), "manifests"},
		{"/ubuntu:latest", bare.Digest.String()},
		{"/ubuntu:latest@" + bare.Digest.String(), bare.Digest.String()},
		{"/example.com/team/widget:v1", dotted.Digest.String()},
	} {
		t.Run(tc.target, func(t *testing.T) {
			body := backendRequest(t, h, tc.target)
			if !strings.Contains(body, tc.want) {
				t.Fatalf("missing %q:\n%s", tc.want, body)
			}
			if strings.Contains(body, "index.docker.io") || strings.Contains(body, "library/ubuntu") {
				t.Fatalf("implicit Docker defaults: %s", body)
			}
		})
	}
	body := backendRequest(t, h, "/ubuntu:latest")
	if !strings.Contains(body, `href="/ubuntu/tags/list"`) || strings.Contains(body, "registry.invalid") {
		t.Fatalf("non-local links: %s", body)
	}
}

func TestRegistryRootAndOptionalHistory(t *testing.T) {
	t.Setenv("CACHE_DIR", t.TempDir())
	reg := ocimem.New()
	image, _ := pushTestImage(t, reg, "ubuntu", "latest")
	h := New(reg).(*handler)
	root := backendRequest(t, h, "/")
	if !strings.Contains(root, "ubuntu") || !strings.Contains(root, `name="image"`) || !strings.Contains(root, `name="repo"`) || !strings.Contains(root, `action="/_open"`) {
		t.Fatalf("root is missing catalog/forms: %s", root)
	}
	if strings.Contains(root, "input { display: none;") {
		t.Fatal("browsing forms are hidden by tab CSS")
	}
	history := backendRequest(t, h, "/ubuntu/_oci/tag-history/latest?n=1")
	if !strings.Contains(history, image.Digest.String()) {
		t.Fatalf("missing local tag history: %s", history)
	}
	h.registry = &oci.Funcs{}
	ref, err := ociref.ParseRelative("ubuntu:latest")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.tagHistory(httptest.NewRequest(http.MethodGet, "/", nil), ref); !errors.Is(err, oci.ErrUnsupported) {
		t.Fatalf("unsupported history: %v", err)
	}
}

func TestRegistryBoundaryPagination(t *testing.T) {
	t.Setenv("CACHE_DIR", t.TempDir())
	reg := ocimem.New()
	pushTestImage(t, reg, "team/widget", "v1", "v2", "v3")
	pushTestImage(t, reg, "team/another", "latest")
	h := New(reg).(*handler)
	repo := "team/widget"
	for _, tc := range []struct {
		query string
		want  []string
		next  string
	}{
		{"n=2", []string{"v1", "v2"}, "v2"},
		{"n=2&next=v2", []string{"v3"}, ""},
		{"n=3", []string{"v1", "v2", "v3"}, ""},
	} {
		tags, err := h.listTags(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/?"+tc.query, nil), repo)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(tags.Tags, tc.want) || tags.Next != tc.next {
			t.Fatalf("%s: %+v", tc.query, tags)
		}
	}
	for _, tc := range []struct {
		query string
		want  []string
		next  string
	}{
		{"n=1", []string{"team/another"}, "team/another"},
		{"n=1&next=team/another", []string{"team/widget"}, ""},
	} {
		page, err := h.listCatalog(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/?"+tc.query, nil))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(page.Repos, tc.want) || page.Next != tc.next {
			t.Fatalf("catalog: %+v", page)
		}
	}
}

func TestRegistryBoundaryRangeReads(t *testing.T) {
	t.Setenv("CACHE_DIR", t.TempDir())
	reg := ocimem.New()
	archive := buildTarGz(t, []tarFile{{name: "hello.txt", body: "hello from memory"}})
	desc := pushTestBlob(t, reg, "team/widget", archive, oci.MediaTypeImageLayerGzip)
	var reads, ranges int
	counted := &oci.Funcs{
		GetBlob_: func(ctx context.Context, repo string, dig oci.Digest) (oci.BlobReader, error) {
			reads++
			return reg.GetBlob(ctx, repo, dig)
		},
		GetBlobRange_: func(ctx context.Context, repo string, dig oci.Digest, start, end int64) (oci.BlobReader, error) {
			ranges++
			return reg.GetBlobRange(ctx, repo, dig, start, end)
		},
	}
	h := New(counted)
	base := fmt.Sprintf("/fs/team/widget@%s/", desc.Digest)
	query := fmt.Sprintf("?mt=application/vnd.oci.image.layer.v1.tar%%2Bgzip&size=%d", desc.Size)
	backendRequest(t, h, base+query)
	beforeReads, beforeRanges := reads, ranges
	backendRequest(t, h, base+query)
	if reads != beforeReads || ranges != beforeRanges {
		t.Fatal("cached directory listing fetched blob bytes")
	}
	if body := backendRequest(t, h, base+"hello.txt"+query); !strings.Contains(body, "hello from memory") {
		t.Fatalf("bad file: %s", body)
	}
	if ranges == 0 {
		t.Fatal("file read did not use GetBlobRange")
	}
}

func TestRegistryBoundaryNoGlobalManifestCache(t *testing.T) {
	t.Setenv("CACHE_DIR", t.TempDir())
	mem := ocimem.New()
	image, _ := pushTestImage(t, mem, "team/widget", "v1")
	var calls int
	sentinel := errors.New("backend unavailable")
	reg := &oci.Funcs{GetManifest_: func(ctx context.Context, repo string, digest oci.Digest) (oci.BlobReader, error) {
		calls++
		if calls > 1 {
			return nil, sentinel
		}
		return mem.GetManifest(ctx, repo, digest)
	}}
	h := New(reg).(*handler)
	ref, err := parseDigestReference("team/widget@" + image.Digest.String())
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, err := h.fetchManifest(httptest.NewRecorder(), req, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := h.fetchManifest(httptest.NewRecorder(), req, ref); !errors.Is(err, sentinel) {
		t.Fatalf("backend bypassed: %v", err)
	}
}

func TestRegistryRequired(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("nil registry accepted")
		}
	}()
	New(nil)
}
func TestRegistryBoundaryContext(t *testing.T) {
	t.Setenv("CACHE_DIR", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := New(&oci.Funcs{GetTag_: func(got context.Context, repo, tag string) (oci.BlobReader, error) {
		if got != ctx || repo != "ubuntu" || tag != "latest" {
			t.Fatalf("wrong arguments: %v %s %s", got, repo, tag)
		}
		return nil, got.Err()
	}}).(*handler)
	ref, err := ociref.ParseRelative("ubuntu")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	if _, err := h.fetchManifest(httptest.NewRecorder(), req, ref); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

type trackedManifestReader struct {
	io.ReadCloser
	desc   oci.Descriptor
	closed bool
}

func (r *trackedManifestReader) Descriptor() oci.Descriptor { return r.desc }
func (r *trackedManifestReader) Close() error               { r.closed = true; return r.ReadCloser.Close() }
func TestReadManifestPreservesBytesAndDescriptor(t *testing.T) {
	body := []byte("{\n  \"schemaVersion\": 2\n}\n")
	desc := oci.Descriptor{Digest: ocidigest.FromBytes(body), Size: int64(len(body)), MediaType: oci.MediaTypeImageManifest,
		URLs: []string{"https://example.invalid/manifest"}, Annotations: map[string]string{"test": "value"},
		ArtifactType: "application/example", Data: append([]byte(nil), body...),
		Platform: &oci.Platform{OS: "linux", Architecture: "arm64", Variant: "v8", OSVersion: "test", OSFeatures: []string{"feature"}}}
	reader := &trackedManifestReader{ReadCloser: io.NopCloser(bytes.NewReader(body)), desc: desc}
	got, err := readManifest(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Manifest, body) {
		t.Fatal("manifest bytes rewritten")
	}
	if !reflect.DeepEqual(got.Descriptor, desc) {
		t.Fatalf("descriptor changed: %+v", got.Descriptor)
	}
	if !reader.closed {
		t.Fatal("reader not closed")
	}
}
func TestRegistryBoundaryReferrers(t *testing.T) {
	t.Setenv("CACHE_DIR", t.TempDir())
	reg := ocimem.New()
	image, cfg := pushTestImage(t, reg, "team/widget", "v1")
	body, err := json.Marshal(oci.IndexOrManifest{SchemaVersion: 2, MediaType: oci.MediaTypeImageManifest, Config: &cfg, Layers: []oci.Descriptor{}, Subject: &image, ArtifactType: "application/example"})
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := reg.PushManifest(context.Background(), "team/widget", body, oci.MediaTypeImageManifest, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := New(reg).(*handler)
	ref, err := parseDigestReference("team/widget@" + image.Digest.String())
	if err != nil {
		t.Fatal(err)
	}
	got, err := h.fetchReferrers(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), ref)
	if err != nil {
		t.Fatal(err)
	}
	var index oci.IndexOrManifest
	if err := json.Unmarshal(got.Manifest, &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Manifests) != 1 || index.Manifests[0].Digest != attachment.Digest || index.Manifests[0].ArtifactType != "application/example" {
		t.Fatalf("unexpected referrers: %+v", index.Manifests)
	}
	if got.Digest.String() != ocidigest.FromBytes(got.Manifest).String() {
		t.Fatal("invalid index digest")
	}
}
func TestReadManifestLimitAndClose(t *testing.T) {
	for _, size := range []int64{0, tooBig + 1} {
		reader := &trackedManifestReader{ReadCloser: io.NopCloser(strings.NewReader(strings.Repeat("x", tooBig+1))), desc: oci.Descriptor{Size: size}}
		if _, err := readManifest(reader); err == nil {
			t.Fatal("oversized manifest accepted")
		}
		if !reader.closed {
			t.Fatal("reader not closed on error")
		}
	}
}
