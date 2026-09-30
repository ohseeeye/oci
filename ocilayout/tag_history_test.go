package ocilayout

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/ohseeeye/oci"
	"github.com/stretchr/testify/require"
)

func pushHistoryIndex(t *testing.T, r oci.Registry, repo, tag, version string) oci.Descriptor {
	t.Helper()
	data := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[],"annotations":{"version":%q}}`, oci.MediaTypeImageIndex, version))
	desc, err := r.PushManifest(t.Context(), repo, data, oci.MediaTypeImageIndex, &oci.PushManifestParameters{Tags: []string{tag}})
	require.NoError(t, err)
	return desc
}

func layoutHistory(t *testing.T, r oci.Registry, repo, tag string, params *oci.TagHistoryParameters) []oci.Descriptor {
	t.Helper()
	history, ok := r.(oci.TagHistory)
	require.True(t, ok)
	entries, err := oci.All(history.TagHistory(t.Context(), repo, tag, params))
	require.NoError(t, err)
	return entries
}

func entryTime(t *testing.T, entry oci.Descriptor) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, entry.Annotations[oci.TagHistoryTimestampAnnotation])
	require.NoError(t, err)
	return ts
}

func TestTagHistoryPersistsInIndex(t *testing.T) {
	dir := t.TempDir()
	r, err := New(dir, nil)
	require.NoError(t, err)
	first := pushHistoryIndex(t, r, "example/app", "latest", "first")
	second := pushHistoryIndex(t, r, "example/app", "latest", "second")
	other := pushHistoryIndex(t, r, "other/app", "latest", "other")
	require.NoError(t, r.DeleteTag(t.Context(), "example/app", "latest"))

	index := readIndex(t, filepath.Join(dir, "index.json"))
	require.NotEmpty(t, index.Annotations[tagHistoryAnnotation])
	reopened, err := New(dir, nil)
	require.NoError(t, err)
	entries := layoutHistory(t, reopened, "example/app", "latest", nil)
	require.Len(t, entries, 3)
	require.Equal(t, []oci.Digest{second.Digest, second.Digest, first.Digest}, []oci.Digest{entries[0].Digest, entries[1].Digest, entries[2].Digest})
	require.Equal(t, []string{oci.TagHistoryEventDeleted, oci.TagHistoryEventCreated, oci.TagHistoryEventCreated}, []string{
		entries[0].Annotations[oci.TagHistoryEventAnnotation],
		entries[1].Annotations[oci.TagHistoryEventAnnotation],
		entries[2].Annotations[oci.TagHistoryEventAnnotation],
	})
	for i, entry := range entries {
		require.Equal(t, oci.MediaTypeImageIndex, entry.MediaType)
		require.Positive(t, entry.Size)
		if i > 0 {
			require.True(t, entryTime(t, entries[i-1]).After(entryTime(t, entry)))
		}
	}
	require.Equal(t, other.Digest, layoutHistory(t, reopened, "other/app", "latest", nil)[0].Digest)
	require.Empty(t, layoutHistory(t, reopened, "missing/app", "latest", nil))
	require.Empty(t, layoutHistory(t, reopened, "example/app", "missing", nil))

	limit := 2
	require.Equal(t, entries[:2], layoutHistory(t, reopened, "example/app", "latest", &oci.TagHistoryParameters{Limit: &limit}))
	zero := 0
	require.Empty(t, layoutHistory(t, reopened, "example/app", "latest", &oci.TagHistoryParameters{Limit: &zero}))
	require.Equal(t, entries[1:], layoutHistory(t, reopened, "example/app", "latest", &oci.TagHistoryParameters{Before: entryTime(t, entries[0])}))
	require.Equal(t, entries[:2], layoutHistory(t, reopened, "example/app", "latest", &oci.TagHistoryParameters{Since: entryTime(t, entries[2])}))
	require.Equal(t, entries[2:], layoutHistory(t, reopened, "example/app", "latest", &oci.TagHistoryParameters{Digest: first.Digest}))
	entries[0].Annotations[oci.TagHistoryEventAnnotation] = "changed"
	require.Equal(t, oci.TagHistoryEventDeleted, layoutHistory(t, reopened, "example/app", "latest", nil)[0].Annotations[oci.TagHistoryEventAnnotation])
}

func TestPerRepositoryTagHistory(t *testing.T) {
	dir := t.TempDir()
	r, err := NewPerRepository(dir, nil)
	require.NoError(t, err)
	first := pushHistoryIndex(t, r, "example/app", "latest", "first")
	pushHistoryIndex(t, r, "example/app", "latest", "second")
	reopened, err := NewPerRepository(dir, nil)
	require.NoError(t, err)
	entries := layoutHistory(t, reopened, "example/app", "latest", nil)
	require.Len(t, entries, 2)
	require.Equal(t, first.Digest, entries[1].Digest)
	require.NotEmpty(t, readIndex(t, filepath.Join(dir, "example", "app", "index.json")).Annotations[tagHistoryAnnotation])
}

func TestTagHistoryOnExistingLayoutIsEmpty(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, ensureLayout(dir))
	r, err := New(dir, nil)
	require.NoError(t, err)
	require.Empty(t, layoutHistory(t, r, "example/app", "latest", nil))
}

func TestTagHistoryRejectsCorruptAnnotation(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, ensureLayout(dir))
	index := readIndex(t, filepath.Join(dir, "index.json"))
	index.Annotations = map[string]string{tagHistoryAnnotation: "not json"}
	require.NoError(t, saveIndex(dir, index))
	r, err := New(dir, nil)
	require.NoError(t, err)
	_, err = oci.All(r.TagHistory(t.Context(), "example/app", "latest", nil))
	require.ErrorContains(t, err, "invalid tag history annotation")
}
