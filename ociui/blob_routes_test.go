package ociui

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/ocimem"
	"github.com/ohseeeye/oci/pkg/ocidigest"
)

func TestCanonicalBlobPreviews(t *testing.T) {
	t.Setenv("CACHE_DIR", t.TempDir())
	reg := ocimem.New()
	const repo = "namespace/fs"
	for _, tc := range []struct {
		name, mt, hint, want string
		body                 []byte
	}{
		{"config", oci.MediaTypeImageConfig, oci.MediaTypeImageConfig, "hello", []byte(`{"message":"hello"}`)},
		{"JSON without hint", "application/octet-stream", "", "hello", []byte(`{"message":"hello"}`)},
		{"JSON octet-stream hint", "application/octet-stream", "application/octet-stream", "hello", []byte(`{"message":"hello"}`)},
		{"text", "text/plain", "", "plain text preview", []byte("plain text preview")},
		{"escaped text", "text/plain", "text/plain", "&lt;script&gt;", []byte("<script>not executable</script>")},
		{"binary", "application/octet-stream", "application/octet-stream", "00000000", []byte{0x00, 0x01, 0xff, 0x02}},
		{"image", "image/png", "image/png", `<img src=`, []byte("\x89PNG\r\n\x1a\n")},
		{"image without hint", "image/png", "", `<img src=`, []byte("\x89PNG\r\n\x1a\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desc := pushTestBlob(t, reg, repo, tc.body, tc.mt)
			target := "/" + repo + "/blobs/" + desc.Digest.String()
			if tc.hint != "" {
				target += "?mt=" + url.QueryEscape(tc.hint)
			}
			body := backendRequest(t, New(reg), target)
			if !strings.Contains(body, tc.want) || strings.Contains(body, "failed:") {
				t.Fatalf("preview missing %q:\n%s", tc.want, body)
			}
		})
	}
}

func TestCanonicalBlobArchiveFormats(t *testing.T) {
	t.Setenv("CACHE_DIR", t.TempDir())
	reg := ocimem.New()
	files := []tarFile{{name: "hello.txt", body: "archive format payload"}}
	for _, tc := range []struct {
		name   string
		encode func(*testing.T, []tarFile) []byte
	}{{"tar", buildTar}, {"gzip", buildTarGz}, {"zstd", buildTarZst}} {
		t.Run(tc.name, func(t *testing.T) {
			desc := pushTestBlob(t, reg, "namespace/fs", tc.encode(t, files), "application/octet-stream")
			h := New(reg)
			root := "/namespace/fs/blobs/" + desc.Digest.String() + "/"
			if body := backendRequest(t, h, root); !strings.Contains(body, "hello.txt") {
				t.Fatalf("missing archive directory:\n%s", body)
			}
			if body := backendRequest(t, h, root+"hello.txt"); !strings.Contains(body, "archive format payload") {
				t.Fatalf("missing archive file:\n%s", body)
			}
		})
	}
}

func TestParseBlobPathBoundary(t *testing.T) {
	for _, algorithm := range []ocidigest.Algorithm{ocidigest.SHA256, ocidigest.SHA512} {
		digest := algorithm.FromBytes([]byte("digest boundary")).String()
		for _, repo := range []string{"namespace/fs", "namespace/blobs/image"} {
			root := "/" + repo + "/blobs/" + digest
			ref, prefix, err := parseBlobPath(root + "/fs/blobs/" + digest + "/tags/list")
			if err != nil || ref.Repository != repo || ref.Digest.String() != digest || prefix != root {
				t.Errorf("parse blob boundary: ref=%v prefix=%q err=%v", ref, prefix, err)
			}
		}
	}
}

func TestBlobArchiveRepositoryBoundary(t *testing.T) {
	t.Setenv("CACHE_DIR", t.TempDir())
	reg := ocimem.New()
	other := ocidigest.FromBytes([]byte("not the archive digest")).String()
	names := []string{
		"fs/hello.txt", "nested/blobs/" + other + "/tags/list",
		"tags/list", "referrers/" + other, "a@b/file.txt", "special?#.txt",
	}
	files := []tarFile{{name: "fs/", typ: '5'}, {name: "nested/", typ: '5'}, {name: "tags/", typ: '5'}, {name: "referrers/", typ: '5'}, {name: "a@b/", typ: '5'}}
	for i, name := range names {
		files = append(files, tarFile{name: name, body: fmt.Sprintf("archive payload %d", i)})
	}
	archive := buildTarGz(t, files)
	for _, repo := range []string{"namespace/fs", "namespace/blobs", "namespace/blobs/image"} {
		t.Run(repo, func(t *testing.T) {
			desc := pushTestBlob(t, reg, repo, archive, "application/octet-stream")
			check := func(gotRepo string, gotDigest oci.Digest) {
				if gotRepo != repo || gotDigest != desc.Digest {
					t.Errorf("archive path reinterpreted as object: repo=%q digest=%q", gotRepo, gotDigest)
				}
			}
			h := New(&oci.Funcs{
				GetBlob_: func(ctx context.Context, gotRepo string, digest oci.Digest) (oci.BlobReader, error) {
					check(gotRepo, digest)
					return reg.GetBlob(ctx, gotRepo, digest)
				},
				GetBlobRange_: func(ctx context.Context, gotRepo string, digest oci.Digest, start, end int64) (oci.BlobReader, error) {
					check(gotRepo, digest)
					return reg.GetBlobRange(ctx, gotRepo, digest, start, end)
				},
				NewError: func(_ context.Context, method, repo string) error {
					t.Errorf("archive entry escaped into registry UI: %s(%q)", method, repo)
					return oci.ErrUnsupported
				},
			})
			root := "/" + repo + "/blobs/" + desc.Digest.String()
			for range 2 { // Streaming and indexed filesystem paths.
				body := backendRequest(t, h, root+"/")
				if !strings.Contains(body, "hello.txt") && !strings.Contains(body, "fs/") {
					t.Fatalf("no archive directory listing:\n%s", body)
				}
				for i, name := range names {
					target := (&url.URL{Path: root + "/" + name}).String()
					body := backendRequest(t, h, target)
					if !strings.Contains(body, fmt.Sprintf("archive payload %d", i)) {
						t.Fatalf("archive file %s was not served:\n%s", name, body)
					}
				}
			}
			// '@' in an archive directory is not a reference delimiter. Flat
			// listings should stay under that directory, not show the whole blob.
			body := backendRequest(t, h, root+"/a@b/?all=true")
			if !strings.Contains(body, "file.txt") || strings.Contains(body, "hello.txt") {
				t.Fatalf("flat listing lost the object/directory boundary:\n%s", body)
			}
		})
	}
}

func TestInvalidBlobPaths(t *testing.T) {
	t.Setenv("CACHE_DIR", t.TempDir())
	h := New(&oci.Funcs{NewError: func(_ context.Context, method, repo string) error {
		t.Errorf("invalid blob path reached registry: %s(%q)", method, repo)
		return oci.ErrUnsupported
	}})
	digest := ocidigest.FromBytes([]byte("blob")).String()
	for _, target := range []string{"/namespace/fs/blobs/sha256:bad", "/namespace/fs/blobs/" + digest + "/../outside", "/namespace/fs/blobs/" + digest + "/%2e%2e/outside"} {
		if rec := routeResponse(h, http.MethodGet, target); rec.Code != http.StatusBadRequest {
			t.Errorf("invalid blob path %s: %d", target, rec.Code)
		}
	}
}

type trackedPreviewBlob struct {
	*bytes.Reader
	desc   oci.Descriptor
	closed int
}

func (b *trackedPreviewBlob) Descriptor() oci.Descriptor { return b.desc }
func (b *trackedPreviewBlob) Close() error               { b.closed++; return nil }

func TestBlobSniffingReusesAndClosesReader(t *testing.T) {
	t.Setenv("CACHE_DIR", t.TempDir())
	for _, data := range []string{`{"message":"buffered JSON"}`, "buffered text <tag>"} {
		blob := &trackedPreviewBlob{Reader: bytes.NewReader([]byte(data)), desc: oci.Descriptor{Digest: ocidigest.FromBytes([]byte(data)), Size: int64(len(data)), MediaType: "application/octet-stream"}}
		calls := 0
		h := New(&oci.Funcs{GetBlob_: func(_ context.Context, repo string, digest oci.Digest) (oci.BlobReader, error) {
			calls++
			return blob, nil
		}})
		body := backendRequest(t, h, "/namespace/fs/blobs/"+blob.desc.Digest.String())
		if !strings.Contains(body, "buffered") || (strings.Contains(data, "<tag>") && !strings.Contains(body, html.EscapeString("<tag>"))) {
			t.Fatalf("sniffing lost or failed to escape bytes:\n%s", body)
		}
		if calls != 1 || blob.closed != 1 {
			t.Errorf("sniffing fetched %d times and closed %d times, want once each", calls, blob.closed)
		}
	}
}

var _ io.ReadCloser = (*trackedPreviewBlob)(nil)
