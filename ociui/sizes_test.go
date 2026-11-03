package ociui

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/ocimem"
)

func pushSizeTestImage(t *testing.T, reg oci.Registry, repo, tag string, files []tarFile) oci.Descriptor {
	t.Helper()
	layer := pushTestBlob(t, reg, repo, buildTarGz(t, files), oci.MediaTypeImageLayerGzip)
	_, cfg := pushTestImage(t, reg, repo)
	body, err := json.Marshal(oci.IndexOrManifest{SchemaVersion: 2, MediaType: oci.MediaTypeImageManifest, Config: &cfg, Layers: []oci.Descriptor{layer}})
	if err != nil {
		t.Fatal(err)
	}
	desc, err := reg.PushManifest(context.Background(), repo, body, oci.MediaTypeImageManifest, &oci.PushManifestParameters{Tags: []string{tag}})
	if err != nil {
		t.Fatal(err)
	}
	return desc
}

func TestManifestSizeLinkListsFiles(t *testing.T) {
	t.Setenv("CACHE_DIR", t.TempDir())
	reg := ocimem.New()
	const repo = "org/team/widget"
	pushSizeTestImage(t, reg, repo, "v1", []tarFile{{name: "etc/small.txt", body: "small"}, {name: "var/large.bin", body: strings.Repeat("x", 2048)}})
	h := New(reg)
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	manifest := backendRequest(t, h, "/"+repo+":v1")
	var target string
	for _, u := range pageLinks(manifest) {
		if strings.HasPrefix(u.Path, "/sizes/") {
			target = u.String()
			break
		}
	}
	if target == "" {
		t.Fatal("manifest has no size link")
	}
	for range 2 { // Both cold and warm archive indexes.
		body := backendRequest(t, h, target)
		large, small := strings.Index(body, "var/large.bin"), strings.Index(body, "etc/small.txt")
		if large < 0 || small < 0 || large > small || !strings.Contains(body, "</html>") {
			t.Fatalf("missing or unsorted file listing:\n%s", body)
		}
	}
	if strings.Contains(logs.String(), "apk scan:") || strings.Contains(logs.String(), "should not be called") {
		t.Fatalf("optional APK lookup read a missing file:\n%s", logs.String())
	}
}

func TestIndexSizeLinkListsPlatforms(t *testing.T) {
	t.Setenv("CACHE_DIR", t.TempDir())
	reg := ocimem.New()
	const repo = "org/team/multi"
	amd := pushSizeTestImage(t, reg, repo, "amd", []tarFile{{name: "amd64.txt", body: "amd64 content"}})
	arm := pushSizeTestImage(t, reg, repo, "arm", []tarFile{{name: "arm64.txt", body: "arm64 content"}})
	amd.Platform = &oci.Platform{OS: "linux", Architecture: "amd64"}
	arm.Platform = &oci.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}
	for _, mt := range []string{oci.MediaTypeImageIndex, oci.MediaTypeDockerManifestList} {
		t.Run(mt, func(t *testing.T) {
			body, err := json.Marshal(oci.IndexOrManifest{SchemaVersion: 2, MediaType: mt, Manifests: []oci.Descriptor{amd, arm}})
			if err != nil {
				t.Fatal(err)
			}
			index, err := reg.PushManifest(context.Background(), repo, body, mt, &oci.PushManifestParameters{Tags: []string{"multi"}})
			if err != nil {
				t.Fatal(err)
			}
			var blobReads atomic.Int64
			h := New(&oci.Funcs{
				GetTag_: reg.GetTag, GetManifest_: reg.GetManifest,
				GetBlob_: func(ctx context.Context, repo string, digest oci.Digest) (oci.BlobReader, error) {
					blobReads.Add(1)
					return reg.GetBlob(ctx, repo, digest)
				},
				GetBlobRange_: reg.GetBlobRange,
			})
			manifest := backendRequest(t, h, "/"+repo+":multi")
			var listing string
			for _, u := range pageLinks(manifest) {
				if u.Path == "/sizes/"+repo+"@"+index.Digest.String() {
					listing = backendRequest(t, h, u.String())
					break
				}
			}
			for _, want := range []string{"linux/amd64", "linux/arm64/v8", amd.Digest.String(), arm.Digest.String()} {
				if !strings.Contains(listing, want) {
					t.Fatalf("index size page missing %q:\n%s", want, listing)
				}
			}
			if blobReads.Load() != 0 {
				t.Fatal("platform directory eagerly downloaded image layers")
			}
			for _, child := range []struct {
				desc oci.Descriptor
				file string
			}{{amd, "amd64.txt"}, {arm, "arm64.txt"}} {
				found := false
				for _, u := range pageLinks(listing) {
					if u.Path == "/sizes/"+repo+"@"+child.desc.Digest.String() {
						found = true
						if got := backendRequest(t, h, u.String()); !strings.Contains(got, child.file) {
							t.Fatalf("platform link did not list %s:\n%s", child.file, got)
						}
					}
				}
				if !found {
					t.Errorf("no size-listing link for %s", child.desc.Digest)
				}
			}
		})
	}
}

func TestSizeListingEmptyStates(t *testing.T) {
	t.Setenv("CACHE_DIR", t.TempDir())
	reg := ocimem.New()
	const repo = "org/team/empty"
	image, _ := pushTestImage(t, reg, repo, "empty")
	body, err := json.Marshal(oci.IndexOrManifest{SchemaVersion: 2, MediaType: oci.MediaTypeImageIndex, Manifests: []oci.Descriptor{}})
	if err != nil {
		t.Fatal(err)
	}
	index, err := reg.PushManifest(context.Background(), repo, body, oci.MediaTypeImageIndex, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := New(reg)
	for _, tc := range []struct {
		desc oci.Descriptor
		want string
	}{{image, "No files to display"}, {index, "This index contains no manifests"}} {
		if got := backendRequest(t, h, "/sizes/"+repo+"@"+tc.desc.Digest.String()); !strings.Contains(got, tc.want) {
			t.Errorf("empty state missing %q:\n%s", tc.want, got)
		}
	}
}

// The APK ownership lookup is optional, but a present database should still
// label files with their package owner in the size listing.
func TestSizeListingAPKOwners(t *testing.T) {
	t.Setenv("CACHE_DIR", t.TempDir())
	reg := ocimem.New()
	const repo = "org/team/alpine"
	pushSizeTestImage(t, reg, repo, "v1", []tarFile{
		{name: "lib/apk/db/installed", body: "P:busybox\nF:bin\nR:hello\n\n"},
		{name: "bin/hello", body: "hello from busybox"},
	})
	h := New(reg)
	desc, err := reg.ResolveTag(context.Background(), repo, "v1")
	if err != nil {
		t.Fatal(err)
	}
	rec := routeResponse(h, http.MethodGet, "/sizes/"+repo+"@"+desc.Digest.String())
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "busybox") || !strings.Contains(rec.Body.String(), "bin/hello") {
		t.Fatalf("APK ownership listing: %d\n%s", rec.Code, rec.Body.String())
	}
}
