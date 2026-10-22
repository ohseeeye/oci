package oci

const (
	// MediaTypeDockerSchema1Manifest is the media type of an unsigned legacy
	// Docker schema 1 manifest.
	MediaTypeDockerSchema1Manifest = "application/vnd.docker.distribution.manifest.v1+json"

	// MediaTypeDockerSchema1SignedManifest is the media type of a legacy Docker
	// schema 1 manifest with a libtrust signature envelope. ociclient strips
	// that envelope when reading, returning MediaTypeDockerSchema1Manifest.
	MediaTypeDockerSchema1SignedManifest = "application/vnd.docker.distribution.manifest.v1+prettyjws"
)

// Schema1Manifest describes the unsigned payload of a legacy Docker schema 1
// manifest for decoding and inspection. Signatures are not represented here;
// envelope handling belongs to ociclient. This model does not enable schema 1
// validation, storage, or transfer through IndexOrManifest.
type Schema1Manifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	Name          string `json:"name,omitempty"`
	Tag           string `json:"tag,omitempty"`
	Architecture  string `json:"architecture,omitempty"`

	// FSLayers lists layer blob digests in wire order, from the topmost layer
	// toward the base. Repeated digests are retained.
	FSLayers []Schema1FSLayer `json:"fsLayers"`

	// History lists compatibility documents in the corresponding wire order.
	History []Schema1History `json:"history"`
}

// Schema1FSLayer references a layer blob. Unlike a modern Descriptor, schema 1
// does not supply a layer's size or media type.
type Schema1FSLayer struct {
	BlobSum Digest `json:"blobSum"`
}

// Schema1History contains a legacy image compatibility document.
type Schema1History struct {
	// V1Compatibility is JSON encoded inside a JSON string, not a nested JSON
	// object. Callers can decode it separately using encoding/json.Unmarshal.
	V1Compatibility string `json:"v1Compatibility"`
}
