package ociui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/ohseeeye/oci/pkg/ociref"
)

func TestRepositoryRelativeReferences(t *testing.T) {
	digest := ocidigest.FromBytes([]byte("image"))
	for _, tc := range []struct {
		input, repo, tag string
		digest           oci.Digest
	}{
		{"ubuntu", "ubuntu", "", ""},
		{"ubuntu:v1", "ubuntu", "v1", ""},
		{"team/widget:v1", "team/widget", "v1", ""},
		{"example.com/team/widget:v1", "example.com/team/widget", "v1", ""},
		{"docker.io/ubuntu", "docker.io/ubuntu", "", ""},
		{"team/widget@" + digest.String(), "team/widget", "", digest},
		{"team/widget:v1@" + digest.String(), "team/widget", "v1", digest},
	} {
		t.Run(tc.input, func(t *testing.T) {
			ref, err := ociref.ParseRelative(tc.input)
			if err != nil {
				t.Fatal(err)
			}
			want := ociref.Reference{Repository: tc.repo, Tag: tc.tag, Digest: tc.digest}
			if ref != want || ref.String() != tc.input {
				t.Fatalf("reference=%+v (%q), want=%+v (%q)", ref, ref.String(), want, tc.input)
			}
		})
	}
	for _, input := range []string{"", "/ubuntu", "https://ghcr.io/team/widget", "localhost:5000/team/widget:v1", "repo@sha256:short", "repo:bad tag", "../repo"} {
		if _, err := ociref.ParseRelative(input); err == nil {
			t.Errorf("accepted invalid local reference %q", input)
		}
	}
	for _, input := range []string{"ubuntu", "ubuntu:v1"} {
		if _, err := parseDigestReference(input); err == nil {
			t.Errorf("accepted digest-less reference %q", input)
		}
	}
}

func TestArchivePathReferences(t *testing.T) {
	h := &handler{}
	for _, digest := range []oci.Digest{ocidigest.FromBytes([]byte("archive")), ocidigest.SHA512.FromBytes([]byte("archive"))} {
		image := "example.com/team/widget@" + digest.String()
		for _, prefix := range []string{"/fs/", "/layers/", "/size/", "/sizes/", "/zurl/", "/cache/checkpoint/"} {
			req := httptest.NewRequest(http.MethodGet, prefix+image+"/directory/file.txt", nil)
			ref, path, err := h.getDigest(httptest.NewRecorder(), req)
			if err != nil {
				t.Fatal(err)
			}
			wantPath := prefix + image
			if strings.HasPrefix(prefix, "/cache/") {
				wantPath = "checkpoint"
			}
			if ref.Host != "" || ref.Repository != "example.com/team/widget" || ref.Digest != digest || path != wantPath {
				t.Fatalf("GET %s: reference=%+v, prefix=%q", req.URL, ref, path)
			}
		}
		for _, suffix := range []string{"garbage", "@another-digest"} {
			req := httptest.NewRequest(http.MethodGet, "/fs/"+image+suffix+"/file", nil)
			if _, _, err := h.getDigest(httptest.NewRecorder(), req); err == nil {
				t.Errorf("accepted invalid digest suffix %q", suffix)
			}
		}
	}
	for _, path := range []string{"/fs/team/widget:v1/", "/fs/team/widget@sha256:short/file", "/cache/missing-reference", "/fs/https://example.com/repo@sha256:short"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if _, _, err := h.getDigest(httptest.NewRecorder(), req); err == nil {
			t.Errorf("accepted invalid archive reference %q", path)
		}
	}
}

func TestJSONDigestsAreValidated(t *testing.T) {
	for _, digest := range []oci.Digest{ocidigest.FromBytes([]byte("blob")), ocidigest.SHA512.FromBytes([]byte("blob"))} {
		raw, err := json.Marshal(digest)
		if err != nil {
			t.Fatal(err)
		}
		got, err := parseJSONDigest(raw)
		if err != nil || got != digest {
			t.Fatalf("digest=%s, error=%v", got, err)
		}
	}
	for _, raw := range []string{`null`, `{}`, `42`, `""`, `"sha256:short"`, `"sha256:` + strings.Repeat("z", 64) + `"`, `"unknown:` + strings.Repeat("a", 64) + `"`} {
		if _, err := parseJSONDigest(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted invalid digest %s", raw)
		}
	}
}

func TestMediaTypeRouting(t *testing.T) {
	for _, tc := range []struct {
		mediaType, handler string
		history            bool
	}{
		{oci.MediaTypeImageManifest, "manifest", false},
		{oci.MediaTypeDockerManifest, "manifest", false},
		{oci.MediaTypeImageIndex, "manifest", false},
		{oci.MediaTypeDockerManifestList, "manifest", false},
		{oci.MediaTypeImageConfig, "json", true},
		{oci.MediaTypeDockerConfig, "json", true},
		{oci.MediaTypeDescriptor, "json", false},
		{oci.MediaTypeDockerSchema1Manifest, "manifest", true},
		{oci.MediaTypeDockerSchema1SignedManifest, "manifest", true},
		{oci.MediaTypeImageLayerGzip, "fs", false},
		{oci.MediaTypeImageLayer, "fs", false},
		{oci.MediaTypeDockerLayerGzip, "fs", false},
		{oci.MediaTypeDockerLayer, "fs", false},
		{oci.MediaTypeDockerForeignLayerGzip, "fs", false},
		{oci.MediaTypeImageLayerNonDistributableGzip, "fs", false},
		{oci.MediaTypeImageLayerNonDistributable, "fs", false},
		{"application/octet-stream", "blob", false},
	} {
		if got := contentKindForMT(tc.mediaType); got != tc.handler {
			t.Errorf("contentKindForMT(%q)=%q, want %q", tc.mediaType, got, tc.handler)
		}
		if got := shouldHistory(tc.mediaType); got != tc.history {
			t.Errorf("shouldHistory(%q)=%v, want %v", tc.mediaType, got, tc.history)
		}
	}
}
