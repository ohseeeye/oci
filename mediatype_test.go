package oci

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMediaTypes(t *testing.T) {
	for _, test := range []struct {
		name, got, want string
	}{
		{"descriptor", MediaTypeDescriptor, "application/vnd.oci.descriptor.v1+json"},
		{"image index", MediaTypeImageIndex, "application/vnd.oci.image.index.v1+json"},
		{"image manifest", MediaTypeImageManifest, "application/vnd.oci.image.manifest.v1+json"},
		{"image config", MediaTypeImageConfig, "application/vnd.oci.image.config.v1+json"},
		{"image layer", MediaTypeImageLayer, "application/vnd.oci.image.layer.v1.tar"},
		{"image layer gzip", MediaTypeImageLayerGzip, "application/vnd.oci.image.layer.v1.tar+gzip"},
		{"image layer zstd", MediaTypeImageLayerZstd, "application/vnd.oci.image.layer.v1.tar+zstd"},
		{"image layer non-distributable", MediaTypeImageLayerNonDistributable, "application/vnd.oci.image.layer.nondistributable.v1.tar"},
		{"image layer non-distributable gzip", MediaTypeImageLayerNonDistributableGzip, "application/vnd.oci.image.layer.nondistributable.v1.tar+gzip"},
		{"Docker manifest list", MediaTypeDockerManifestList, "application/vnd.docker.distribution.manifest.list.v2+json"},
		{"Docker manifest", MediaTypeDockerManifest, "application/vnd.docker.distribution.manifest.v2+json"},
		{"Docker config", MediaTypeDockerConfig, "application/vnd.docker.container.image.v1+json"},
		{"Docker layer", MediaTypeDockerLayer, "application/vnd.docker.image.rootfs.diff.tar"},
		{"Docker layer gzip", MediaTypeDockerLayerGzip, "application/vnd.docker.image.rootfs.diff.tar.gzip"},
		{"Docker foreign layer gzip", MediaTypeDockerForeignLayerGzip, "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip"},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, test.got)
		})
	}
}
