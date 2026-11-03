package ociui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ocidigest"
)

func TestSchema1HistoryUsesUpstreamModels(t *testing.T) {
	t.Setenv("CACHE_DIR", t.TempDir())
	base := ocidigest.FromBytes([]byte("base layer"))
	top := ocidigest.FromBytes([]byte("top layer"))
	history := func(command string) oci.Schema1History {
		t.Helper()
		b, err := json.Marshal(map[string]any{"container_config": map[string]any{"Cmd": []string{"/bin/sh", "-c", command}}})
		if err != nil {
			t.Fatal(err)
		}
		return oci.Schema1History{V1Compatibility: string(b)}
	}
	manifest := oci.Schema1Manifest{
		SchemaVersion: 1, Name: "team/widget", Tag: "legacy", Architecture: "amd64",
		FSLayers: []oci.Schema1FSLayer{{BlobSum: top}, {BlobSum: emptyDigest}, {BlobSum: base}, {BlobSum: base}},
		History: []oci.Schema1History{
			history("echo top_step"), history("#(nop) ENV marker=metadata_step"),
			history("echo middle_step"), history("echo base_step"),
		},
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, mediaType string
		sizes           bool
	}{
		{"unsigned", oci.MediaTypeDockerSchema1Manifest, true},
		{"signed-media-type", oci.MediaTypeDockerSchema1SignedManifest, true},
		{"sizes-unavailable", oci.MediaTypeDockerSchema1Manifest, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var resolved []oci.Digest
			var reader *trackedManifestReader
			reg := &oci.Funcs{
				GetTag_: func(ctx context.Context, repo, tag string) (oci.BlobReader, error) {
					if repo != "team/widget" || tag != "legacy" {
						t.Fatalf("unexpected manifest reference: %s:%s", repo, tag)
					}
					reader = &trackedManifestReader{ReadCloser: io.NopCloser(bytes.NewReader(body)), desc: oci.Descriptor{
						MediaType: tc.mediaType, Digest: ocidigest.FromBytes(body), Size: int64(len(body)),
					}}
					return reader, nil
				},
				ResolveBlob_: func(ctx context.Context, repo string, digest oci.Digest) (oci.Descriptor, error) {
					if repo != "team/widget" {
						t.Fatalf("unexpected blob repository: %s", repo)
					}
					resolved = append(resolved, digest)
					if !tc.sizes {
						return oci.Descriptor{}, oci.ErrUnsupported
					}
					size := int64(123)
					if digest == top {
						size = 456
					}
					return oci.Descriptor{Digest: digest, Size: size}, nil
				},
				NewError: func(_ context.Context, method, repo string) error {
					t.Errorf("unexpected registry operation: %s(%q)", method, repo)
					return oci.ErrUnsupported
				},
			}
			h := New(reg)
			page := backendRequest(t, h, "/team/widget:legacy?render=history&mt="+url.QueryEscape(tc.mediaType))
			previous := -1
			for _, command := range []string{"base_step", "middle_step", "metadata_step", "top_step"} {
				pos := strings.Index(page, command)
				if pos <= previous {
					t.Fatalf("history command missing or out of order: %s", command)
				}
				previous = pos
			}
			if !reflect.DeepEqual(resolved, []oci.Digest{base, base, top}) {
				t.Fatalf("size lookups=%v; empty layers should be skipped and repeated layers preserved", resolved)
			}
			for _, digest := range []oci.Digest{base, top} {
				if !strings.Contains(page, "/fs/team/widget@"+digest.String()+"/") {
					t.Errorf("missing repository-local layer link for %s", digest)
				}
			}
			if tc.sizes && (!strings.Contains(page, `title="123 bytes"`) || !strings.Contains(page, `title="456 bytes"`)) {
				t.Error("missing layer sizes")
			}
			if reader == nil || !reader.closed {
				t.Error("manifest reader was not closed")
			}
		})
	}
}

func TestSchema1HistoryRejectsMalformedJSON(t *testing.T) {
	h := &handler{}
	for _, raw := range []string{
		`{`,
		`{"history":[{"v1Compatibility":{}}]}`,
		`{"history":[{"v1Compatibility":"{"}]}`,
		`{"history":[{"v1Compatibility":"{\"container_config\":{\"Cmd\":42}}"}]}`,
	} {
		err := h.renderDockerfileSchema1(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), []byte(raw), "team/widget")
		if err == nil {
			t.Errorf("accepted malformed schema-1 history: %s", raw)
		}
	}
}
