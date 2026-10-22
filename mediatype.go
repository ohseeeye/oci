package oci

// OCI media types.
const (
	// MediaTypeDescriptor is the media type for an OCI content descriptor.
	MediaTypeDescriptor = "application/vnd.oci.descriptor.v1+json"

	// MediaTypeImageIndex is the media type for an OCI image index.
	MediaTypeImageIndex = "application/vnd.oci.image.index.v1+json"

	// MediaTypeImageManifest is the media type for an OCI image manifest.
	MediaTypeImageManifest = "application/vnd.oci.image.manifest.v1+json"

	// MediaTypeImageConfig is the media type for an OCI image configuration.
	MediaTypeImageConfig = "application/vnd.oci.image.config.v1+json"

	// MediaTypeImageLayer is the media type for an uncompressed OCI tar layer.
	MediaTypeImageLayer = "application/vnd.oci.image.layer.v1.tar"

	// MediaTypeImageLayerGzip is the media type for a gzip-compressed OCI tar layer.
	MediaTypeImageLayerGzip = "application/vnd.oci.image.layer.v1.tar+gzip"

	// MediaTypeImageLayerZstd is the media type for a zstd-compressed OCI tar layer.
	MediaTypeImageLayerZstd = "application/vnd.oci.image.layer.v1.tar+zstd"

	// MediaTypeImageLayerNonDistributable identifies an uncompressed legacy OCI
	// non-distributable tar layer. The OCI image spec deprecates this media type;
	// it is provided for compatibility with existing content.
	MediaTypeImageLayerNonDistributable = "application/vnd.oci.image.layer.nondistributable.v1.tar"

	// MediaTypeImageLayerNonDistributableGzip identifies a gzip-compressed legacy
	// OCI non-distributable tar layer. The OCI image spec deprecates this media
	// type; it is provided for compatibility with existing content.
	MediaTypeImageLayerNonDistributableGzip = "application/vnd.oci.image.layer.nondistributable.v1.tar+gzip"
)

// Docker schema 2 media types. Legacy schema 1 types are kept in schema1.go.
const (
	// MediaTypeDockerManifestList is the media type for a Docker manifest list.
	MediaTypeDockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"

	// MediaTypeDockerManifest is the media type for a Docker schema 2 manifest.
	MediaTypeDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"

	// MediaTypeDockerConfig is the media type for a Docker image configuration.
	MediaTypeDockerConfig = "application/vnd.docker.container.image.v1+json"

	// MediaTypeDockerLayer is the media type for an uncompressed Docker tar layer.
	MediaTypeDockerLayer = "application/vnd.docker.image.rootfs.diff.tar"

	// MediaTypeDockerLayerGzip is the media type for a gzip-compressed Docker tar layer.
	MediaTypeDockerLayerGzip = "application/vnd.docker.image.rootfs.diff.tar.gzip"

	// MediaTypeDockerForeignLayerGzip is the media type for a gzip-compressed
	// Docker foreign tar layer.
	MediaTypeDockerForeignLayerGzip = "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip"
)
