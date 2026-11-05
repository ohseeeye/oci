package ocisqlite

import (
	"testing"

	"github.com/ohseeeye/oci"
	"github.com/stretchr/testify/require"
)

func TestPushManifestReturnsOwnedUniqueTags(t *testing.T) {
	r := newRegistry(t, t.TempDir())
	data := manifestBytes(t, oci.IndexOrManifest{SchemaVersion: 2, MediaType: oci.MediaTypeImageIndex})
	params := &oci.PushManifestParameters{Tags: []string{"v1", "latest", "v1"}}
	result, err := r.PushManifest(t.Context(), "example", data, oci.MediaTypeImageIndex, params)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"latest", "v1"}, result.Tags)
	require.Equal(t, []string{"v1", "latest", "v1"}, params.Tags)
	for _, tag := range result.Tags {
		got, err := r.ResolveTag(t.Context(), "example", tag)
		require.NoError(t, err)
		require.Equal(t, result.Descriptor, got)
	}
	params.Tags[0] = "caller-changed"
	require.ElementsMatch(t, []string{"latest", "v1"}, result.Tags)
	result.Tags[0] = "result-changed"
	require.Equal(t, []string{"caller-changed", "latest", "v1"}, params.Tags)
}
