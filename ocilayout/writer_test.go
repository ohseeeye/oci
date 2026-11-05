package ocilayout

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ohseeeye/oci"
	"github.com/stretchr/testify/require"
)

func TestPushManifestReturnsOwnedUniqueTags(t *testing.T) {
	r, err := NewPerRepository(t.TempDir(), nil)
	require.NoError(t, err)
	data := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	params := &oci.PushManifestParameters{Tags: []string{"v1", "latest", "v1"}}
	result, err := r.PushManifest(t.Context(), "example", data, oci.MediaTypeImageIndex, params)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"latest", "v1"}, result.Tags)
	require.Equal(t, []string{"v1", "latest", "v1"}, params.Tags)
	for _, tag := range result.Tags {
		got, err := r.ResolveTag(t.Context(), "example", tag)
		require.NoError(t, err)
		require.Equal(t, result.Digest, got.Digest)
	}
	params.Tags[0] = "caller-changed"
	require.ElementsMatch(t, []string{"latest", "v1"}, result.Tags)
	result.Tags[0] = "result-changed"
	require.Equal(t, []string{"caller-changed", "latest", "v1"}, params.Tags)
}

func TestPushManifestFailedIndexWriteReportsNoTags(t *testing.T) {
	dir := t.TempDir()
	r, err := New(dir, nil)
	require.NoError(t, err)
	first := pushHistoryIndex(t, r, "example", "latest", "first")
	history := layoutHistory(t, r, "example", "latest", nil)
	indexPath := filepath.Join(dir, "index.json")
	backupPath := filepath.Join(dir, "saved-index.json")
	require.NoError(t, os.Rename(indexPath, backupPath))
	require.NoError(t, os.Mkdir(indexPath, 0o700))

	data := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	result, err := r.PushManifest(t.Context(), "example", data, oci.MediaTypeImageIndex, &oci.PushManifestParameters{Tags: []string{"latest", "new"}})
	require.Error(t, err)
	require.Empty(t, result.Tags)

	require.NoError(t, os.Remove(indexPath))
	require.NoError(t, os.Rename(backupPath, indexPath))
	reopened, err := New(dir, nil)
	require.NoError(t, err)
	for _, reg := range []*Registry{r, reopened} {
		got, err := reg.ResolveTag(t.Context(), "example", "latest")
		require.NoError(t, err)
		require.Equal(t, first.Digest, got.Digest)
		_, err = reg.ResolveTag(t.Context(), "example", "new")
		require.ErrorIs(t, err, oci.ErrManifestUnknown)
		require.Equal(t, history, layoutHistory(t, reg, "example", "latest", nil))
		require.Empty(t, layoutHistory(t, reg, "example", "new", nil))
	}
}
