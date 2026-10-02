package ocis3

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ohseeeye/oci"
	"github.com/stretchr/testify/require"
)

func pushTaggedIndex(t *testing.T, r *Registry, repo, tag, version string) oci.Descriptor {
	t.Helper()
	data := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[],"annotations":{"version":%q}}`, oci.MediaTypeImageIndex, version))
	desc, err := r.PushManifest(t.Context(), repo, data, oci.MediaTypeImageIndex, &oci.PushManifestParameters{Tags: []string{tag}})
	require.NoError(t, err)
	return desc
}

func historyTime(t *testing.T, desc oci.Descriptor) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, desc.Annotations[oci.TagHistoryTimestampAnnotation])
	require.NoError(t, err)
	return ts
}

func TestTagHistory(t *testing.T) {
	r := newRegistry(t)
	first := pushTaggedIndex(t, r, "example", "latest", "first")
	second := pushTaggedIndex(t, r, "example", "latest", "second")
	// Reassigning the same digest is still a new event.
	pushTaggedIndex(t, r, "example", "latest", "second")
	require.NoError(t, r.DeleteTag(t.Context(), "example", "latest"))
	require.NoError(t, r.DeleteManifest(t.Context(), "example", second.Digest))

	entries, err := oci.All(r.TagHistory(t.Context(), "example", "latest", nil))
	require.NoError(t, err)
	require.Len(t, entries, 4)
	require.Equal(t, []oci.Digest{second.Digest, second.Digest, second.Digest, first.Digest}, []oci.Digest{entries[0].Digest, entries[1].Digest, entries[2].Digest, entries[3].Digest})
	require.Equal(t, []string{oci.TagHistoryEventDeleted, oci.TagHistoryEventCreated, oci.TagHistoryEventCreated, oci.TagHistoryEventCreated}, []string{
		entries[0].Annotations[oci.TagHistoryEventAnnotation],
		entries[1].Annotations[oci.TagHistoryEventAnnotation],
		entries[2].Annotations[oci.TagHistoryEventAnnotation],
		entries[3].Annotations[oci.TagHistoryEventAnnotation],
	})
	for i, entry := range entries {
		require.Equal(t, oci.MediaTypeImageIndex, entry.MediaType)
		require.Positive(t, entry.Size)
		if i > 0 {
			require.True(t, historyTime(t, entries[i-1]).After(historyTime(t, entry)), "timestamps must descend strictly")
		}
	}

	limit := 2
	page, err := oci.All(r.TagHistory(t.Context(), "example", "latest", &oci.TagHistoryParameters{Limit: &limit}))
	require.NoError(t, err)
	require.Equal(t, entries[:2], page)
	older, err := oci.All(r.TagHistory(t.Context(), "example", "latest", &oci.TagHistoryParameters{Before: historyTime(t, page[1])}))
	require.NoError(t, err)
	require.Equal(t, entries[2:], older)
	newer, err := oci.All(r.TagHistory(t.Context(), "example", "latest", &oci.TagHistoryParameters{Since: historyTime(t, entries[2])}))
	require.NoError(t, err)
	require.Equal(t, entries[:2], newer)
	interval, err := oci.All(r.TagHistory(t.Context(), "example", "latest", &oci.TagHistoryParameters{
		Before: historyTime(t, entries[0]), Since: historyTime(t, entries[3]),
	}))
	require.NoError(t, err)
	require.Equal(t, entries[1:3], interval)
	matching, err := oci.All(r.TagHistory(t.Context(), "example", "latest", &oci.TagHistoryParameters{Digest: first.Digest}))
	require.NoError(t, err)
	require.Equal(t, entries[3:], matching)

	// Callers cannot mutate the stored annotations through a returned snapshot.
	entries[0].Annotations[oci.TagHistoryEventAnnotation] = "changed"
	again, err := oci.All(r.TagHistory(t.Context(), "example", "latest", nil))
	require.NoError(t, err)
	require.Equal(t, oci.TagHistoryEventDeleted, again[0].Annotations[oci.TagHistoryEventAnnotation])
}

func TestTagHistoryEmptyAndInvalidFilters(t *testing.T) {
	r := newRegistry(t)
	pushTaggedIndex(t, r, "example", "latest", "first")
	for _, tc := range []struct{ repo, tag string }{
		{"missing", "latest"}, {"example", "missing"},
	} {
		entries, err := oci.All(r.TagHistory(t.Context(), tc.repo, tc.tag, nil))
		require.NoError(t, err)
		require.Empty(t, entries)
	}
	zero, negative := 0, -1
	entries, err := oci.All(r.TagHistory(t.Context(), "example", "latest", &oci.TagHistoryParameters{Limit: &zero}))
	require.NoError(t, err)
	require.Empty(t, entries)
	_, err = oci.All(r.TagHistory(t.Context(), "example", "latest", &oci.TagHistoryParameters{Limit: &negative}))
	require.Error(t, err)
	_, err = oci.All(r.TagHistory(t.Context(), "example", "latest", &oci.TagHistoryParameters{Digest: "bad"}))
	require.Error(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = oci.All(r.TagHistory(ctx, "example", "latest", nil))
	require.ErrorIs(t, err, context.Canceled)
}
