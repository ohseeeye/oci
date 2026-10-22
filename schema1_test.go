package oci

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/stretchr/testify/require"
)

func TestSchema1ManifestJSON(t *testing.T) {
	top := ocidigest.FromBytes([]byte("top layer"))
	base := ocidigest.FromBytes([]byte("base layer"))
	const topCompatibility = "{\n  \"container_config\": {\"Cmd\": [\"/bin/sh\", \"-c\", \"echo hello\"]}\n}"
	const baseCompatibility = `{"container_config":{"Cmd":["base"]},"unknown":{"preserved":true}}`
	wire := fmt.Sprintf(`{
		"schemaVersion": 1,
		"name": "team/app",
		"tag": "legacy",
		"architecture": "amd64",
		"fsLayers": [{"blobSum": %q}, {"blobSum": %q}, {"blobSum": %q}],
		"history": [{"v1Compatibility": %q}, {"v1Compatibility": %q}, {"v1Compatibility": %q}]
	}`, top, base, top, topCompatibility, baseCompatibility, topCompatibility)

	var manifest Schema1Manifest
	require.NoError(t, json.Unmarshal([]byte(wire), &manifest))
	require.Equal(t, Schema1Manifest{
		SchemaVersion: 1,
		Name:          "team/app",
		Tag:           "legacy",
		Architecture:  "amd64",
		FSLayers:      []Schema1FSLayer{{BlobSum: top}, {BlobSum: base}, {BlobSum: top}},
		History: []Schema1History{
			{V1Compatibility: topCompatibility},
			{V1Compatibility: baseCompatibility},
			{V1Compatibility: topCompatibility},
		},
	}, manifest, "wire order, repeated digests, and inner document bytes must be retained")

	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.JSONEq(t, wire, string(data), "compatibility documents must remain JSON strings, not objects")
}

func TestSchema1ManifestOptionalMetadata(t *testing.T) {
	const wire = `{"schemaVersion":1,"fsLayers":[],"history":[]}`
	var manifest Schema1Manifest
	require.NoError(t, json.Unmarshal([]byte(wire), &manifest))
	require.Equal(t, Schema1Manifest{
		SchemaVersion: 1,
		FSLayers:      []Schema1FSLayer{},
		History:       []Schema1History{},
	}, manifest)
	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.JSONEq(t, wire, string(data))
}

func TestSchema1HistoryRequiresCompatibilityString(t *testing.T) {
	var history Schema1History
	err := json.Unmarshal([]byte(`{"v1Compatibility":{"container_config":{"Cmd":["example"]}}}`), &history)
	require.Error(t, err, "the schema 1 wire format contains a string, not an embedded object")
}

func TestSchema1RemainsSeparateFromIndexOrManifest(t *testing.T) {
	var manifest IndexOrManifest
	require.NoError(t, json.Unmarshal([]byte(`{"schemaVersion":1,"fsLayers":[],"history":[]}`), &manifest))
	require.ErrorContains(t, manifest.Validate(), "schema version 1")
}
