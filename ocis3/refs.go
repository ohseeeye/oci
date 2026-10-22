package ocis3

import (
	"cmp"
	"encoding/json"
	"fmt"

	"github.com/ohseeeye/oci"
)

type childReference struct {
	desc     oci.Descriptor
	manifest bool
}

type manifestInfo struct {
	children     []childReference
	subject      oci.Digest
	artifactType string
	annotations  map[string]string
}

// Unknown media types remain opaque, matching the other backends.
func manifestInfoFromBytes(mediaType string, data []byte) (manifestInfo, error) {
	switch mediaType {
	case oci.MediaTypeImageManifest, oci.MediaTypeDockerManifest, oci.MediaTypeImageIndex, oci.MediaTypeDockerManifestList:
	default:
		return manifestInfo{}, nil
	}
	var m oci.IndexOrManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return manifestInfo{}, err
	}
	if m.MediaType == "" {
		m.MediaType = mediaType
	}
	if m.MediaType != mediaType {
		return manifestInfo{}, fmt.Errorf("manifest media type does not match content")
	}
	if m.SchemaVersion != 2 {
		return manifestInfo{}, fmt.Errorf("schema version must be 2")
	}
	if err := m.Validate(); err != nil {
		return manifestInfo{}, err
	}
	info := manifestInfo{artifactType: m.ArtifactType, annotations: m.Annotations}
	for _, child := range m.Manifests {
		info.children = append(info.children, childReference{child, true})
	}
	for _, child := range m.Layers {
		info.children = append(info.children, childReference{child, false})
	}
	if m.Config != nil {
		info.children = append(info.children, childReference{*m.Config, false})
		info.artifactType = cmp.Or(m.ArtifactType, m.Config.MediaType)
	}
	if m.Subject != nil {
		info.subject = m.Subject.Digest
	}
	for _, child := range info.children {
		if child.desc.Size < 0 || child.desc.MediaType == "" {
			return manifestInfo{}, fmt.Errorf("invalid child descriptor")
		}
	}
	return info, nil
}
