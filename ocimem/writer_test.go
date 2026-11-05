package ocimem

import (
	"testing"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/stretchr/testify/require"
)

func TestPushManifestConfirmsExistingImmutableAndNewTags(t *testing.T) {
	r := NewWithConfig(&Config{ImmutableTags: true})
	data := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	first, err := r.PushManifest(t.Context(), "example", data, oci.MediaTypeImageIndex, &oci.PushManifestParameters{Tags: []string{"existing"}})
	require.NoError(t, err)

	result, err := r.PushManifest(t.Context(), "example", data, oci.MediaTypeImageIndex, &oci.PushManifestParameters{Tags: []string{"existing", "new"}})
	require.NoError(t, err)
	require.Equal(t, first.Descriptor, result.Descriptor)
	require.ElementsMatch(t, []string{"existing", "new"}, result.Tags)
	for _, tag := range result.Tags {
		got, err := r.ResolveTag(t.Context(), "example", tag)
		require.NoError(t, err)
		require.Equal(t, result.Descriptor, got)
		history, err := oci.All(r.TagHistory(t.Context(), "example", tag, nil))
		require.NoError(t, err)
		require.Len(t, history, 1)
	}
}

func TestPushManifestValidatesTagsAfterExistingImmutableTag(t *testing.T) {
	for _, lastTag := range []string{"conflict", "invalid/tag"} {
		t.Run(lastTag, func(t *testing.T) {
			r := NewWithConfig(&Config{ImmutableTags: true})
			data := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
			first, err := r.PushManifest(t.Context(), "example", data, oci.MediaTypeImageIndex, &oci.PushManifestParameters{Tags: []string{"existing"}})
			require.NoError(t, err)
			conflict := pushTaggedIndex(t, r, "example", "conflict", "other")

			result, err := r.PushManifest(t.Context(), "example", data, oci.MediaTypeImageIndex, &oci.PushManifestParameters{Tags: []string{"existing", "new", lastTag}})
			require.Error(t, err)
			require.Empty(t, result.Tags)
			_, err = r.ResolveTag(t.Context(), "example", "new")
			require.ErrorIs(t, err, oci.ErrManifestUnknown)
			got, err := r.ResolveTag(t.Context(), "example", "conflict")
			require.NoError(t, err)
			require.Equal(t, conflict, got)
			history, err := oci.All(r.TagHistory(t.Context(), "example", "existing", nil))
			require.NoError(t, err)
			require.Len(t, history, 1)
			require.Equal(t, first.Digest, history[0].Digest)
			history, err = oci.All(r.TagHistory(t.Context(), "example", "new", nil))
			require.NoError(t, err)
			require.Empty(t, history)
		})
	}
}

func TestPushManifestReturnsOwnedUniqueTags(t *testing.T) {
	r := New()
	data := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	params := &oci.PushManifestParameters{Tags: []string{"v1", "latest", "v1"}}
	result, err := r.PushManifest(t.Context(), "example", data, oci.MediaTypeImageIndex, params)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"latest", "v1"}, result.Tags)
	require.Equal(t, []string{"v1", "latest", "v1"}, params.Tags)
	params.Tags[0] = "caller-changed"
	require.ElementsMatch(t, []string{"latest", "v1"}, result.Tags)
	result.Tags[0] = "result-changed"
	require.Equal(t, []string{"caller-changed", "latest", "v1"}, params.Tags)
	tags, err := oci.All(r.Tags(t.Context(), "example", nil))
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"latest", "v1"}, tags)
}

func TestPushManifestWithoutTagsReturnsDescriptor(t *testing.T) {
	r := New()
	data := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	result, err := r.PushManifest(t.Context(), "example", data, oci.MediaTypeImageIndex, nil)
	require.NoError(t, err)
	require.Empty(t, result.Tags)
	require.Equal(t, oci.Descriptor{MediaType: oci.MediaTypeImageIndex, Digest: ocidigest.FromBytes(data), Size: int64(len(data))}, result.Descriptor)
	got, err := r.ResolveManifest(t.Context(), "example", result.Digest)
	require.NoError(t, err)
	require.Equal(t, result.Descriptor, got)
}
