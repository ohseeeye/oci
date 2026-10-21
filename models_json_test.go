package oci

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/stretchr/testify/require"
)

// Decode only the top-level keys in wire order, skipping nested values. Decoding
// into a map or comparing JSON semantically would not detect field-order changes.
func topLevelJSONKeys(t *testing.T, data []byte) []string {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	require.NoError(t, err)
	require.Equal(t, json.Delim('{'), token)
	var keys []string
	for decoder.More() {
		token, err := decoder.Token()
		require.NoError(t, err)
		key, ok := token.(string)
		require.True(t, ok)
		keys = append(keys, key)
		var value json.RawMessage
		require.NoError(t, decoder.Decode(&value))
	}
	token, err = decoder.Token()
	require.NoError(t, err)
	require.Equal(t, json.Delim('}'), token)
	_, err = decoder.Token()
	require.ErrorIs(t, err, io.EOF)
	return keys
}

func TestIndexOrManifestJSONFieldOrder(t *testing.T) {
	descriptor := Descriptor{
		MediaType: MediaTypeImageConfig,
		Digest:    ocidigest.FromBytes([]byte("config")),
		Size:      6,
	}
	for _, test := range []struct {
		name, mediaType string
		contentKeys     []string
	}{
		{"OCI index", MediaTypeImageIndex, []string{"manifests"}},
		{"Docker manifest list", MediaTypeDockerManifestList, []string{"manifests"}},
		{"OCI manifest", MediaTypeImageManifest, []string{"config", "layers"}},
		{"Docker manifest", MediaTypeDockerManifest, []string{"config", "layers"}},
		{"unknown media type", "application/example", []string{"manifests", "config", "layers"}},
		{"omitted media type", "", []string{"manifests", "config", "layers"}},
	} {
		for _, optional := range []bool{false, true} {
			optionalName := "without optional fields"
			if optional {
				optionalName = "with optional fields"
			}
			t.Run(test.name+"/"+optionalName, func(t *testing.T) {
				manifest := IndexOrManifest{
					SchemaVersion: 2,
					MediaType:     test.mediaType,
				}
				if test.mediaType == MediaTypeImageIndex || test.mediaType == MediaTypeDockerManifestList {
					manifest.Manifests = []Descriptor{descriptor}
				} else {
					manifest.Config = &descriptor
					manifest.Layers = []Descriptor{descriptor}
				}
				want := []string{"schemaVersion"}
				if test.mediaType != "" {
					want = append(want, "mediaType")
				}
				if optional {
					manifest.ArtifactType = "application/example.artifact"
					manifest.Subject = &descriptor
					manifest.Annotations = map[string]string{"z": "last", "a": "first"}
					want = append(want, "artifactType", "subject")
				}
				want = append(want, test.contentKeys...)
				if optional {
					want = append(want, "annotations")
				}
				for _, encoder := range []struct {
					name   string
					encode func(any) ([]byte, error)
				}{
					{"Marshal", json.Marshal},
					{"MarshalIndent", func(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }},
					{"Encoder", func(v any) ([]byte, error) {
						var buffer bytes.Buffer
						err := json.NewEncoder(&buffer).Encode(v)
						return buffer.Bytes(), err
					}},
				} {
					t.Run(encoder.name, func(t *testing.T) {
						data, err := encoder.encode(manifest)
						require.NoError(t, err)
						require.Equal(t, want, topLevelJSONKeys(t, data))
						var decoded IndexOrManifest
						require.NoError(t, json.Unmarshal(data, &decoded))
						require.Equal(t, manifest, decoded)
					})
				}
			})
		}
	}
}

func TestIndexOrManifestMarshalJSONValidation(t *testing.T) {
	for _, manifest := range []IndexOrManifest{
		{SchemaVersion: 1},
		{SchemaVersion: 2, MediaType: MediaTypeImageManifest},
		{SchemaVersion: 2, MediaType: MediaTypeImageIndex, Config: &Descriptor{}},
		{SchemaVersion: 2, MediaType: MediaTypeImageIndex, Subject: &Descriptor{Digest: "sha256:bad"}},
	} {
		_, err := json.Marshal(manifest)
		require.Error(t, err)
	}
}
