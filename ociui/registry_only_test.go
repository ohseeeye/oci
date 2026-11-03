package ociui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/ocimem"
)

func TestDescriptorURLsDoNotCreateProxyLinks(t *testing.T) {
	t.Setenv("CACHE_DIR", t.TempDir())
	reg := ocimem.New()
	_, cfg := pushTestImage(t, reg, "team/widget")
	layer := pushTestBlob(t, reg, "team/widget", buildTarGz(t, []tarFile{{name: "hello.txt", body: "from the registry"}}), oci.MediaTypeImageLayerGzip)
	layer.URLs = []string{"https://mirror.example.com/blob.tar.gz", "http://backup.example.com/blob.tar.gz"}
	body, err := json.Marshal(oci.IndexOrManifest{SchemaVersion: 2, MediaType: oci.MediaTypeImageManifest, Config: &cfg, Layers: []oci.Descriptor{layer}})
	if err != nil {
		t.Fatal(err)
	}
	image, err := reg.PushManifest(context.Background(), "team/widget", body, oci.MediaTypeImageManifest, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := New(reg)
	page := backendRequest(t, h, "/team/widget@"+image.Digest.String())
	for _, external := range layer.URLs {
		if !strings.Contains(page, external) {
			t.Errorf("descriptor URL not displayed: %s", external)
		}
	}
	for _, prefix := range []string{`href="/http/`, `href="/https/`} {
		if strings.Contains(page, prefix) {
			t.Errorf("generated removed proxy route: %s", prefix)
		}
	}
	if !strings.Contains(page, "/fs/team/widget@"+layer.Digest.String()) {
		t.Fatal("missing registry-local layer link")
	}
	file := backendRequest(t, h, "/layers/team/widget@"+image.Digest.String()+"/hello.txt")
	if !strings.Contains(file, "from the registry") {
		t.Fatalf("layer file was not read through the registry: %s", file)
	}
}
