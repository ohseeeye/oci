package ocisqlite

import (
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/ociclient"
	"github.com/ohseeeye/oci/ociserver"
	"github.com/stretchr/testify/require"
)

func TestTagHistoryOverHTTP(t *testing.T) {
	backend := newRegistry(t, t.TempDir())
	handler, err := ociserver.New(backend, nil)
	require.NoError(t, err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	client, err := ociclient.New(u.Host, &ociclient.Options{Insecure: true})
	require.NoError(t, err)
	first := pushTaggedIndex(t, backend, "example", "latest", "first")
	pushTaggedIndex(t, backend, "example", "latest", "second")
	require.NoError(t, client.DeleteTag(t.Context(), "example", "latest"))
	entries, err := oci.All(client.TagHistory(t.Context(), "example", "latest", nil))
	require.NoError(t, err)
	require.Len(t, entries, 3)
	require.Equal(t, first.Digest, entries[2].Digest)
	require.Equal(t, oci.TagHistoryEventDeleted, entries[0].Annotations[oci.TagHistoryEventAnnotation])
	limit := 1
	older, err := oci.All(client.TagHistory(t.Context(), "example", "latest", &oci.TagHistoryParameters{Before: historyTime(t, entries[0]), Limit: &limit}))
	require.NoError(t, err)
	require.Equal(t, entries[1:2], older)
}

func TestConditionalManifestPushOverHTTP(t *testing.T) {
	backend := newRegistry(t, t.TempDir())
	handler, err := ociserver.New(backend, nil)
	require.NoError(t, err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	client, err := ociclient.New(u.Host, &ociclient.Options{Insecure: true})
	require.NoError(t, err)
	old := pushTaggedIndex(t, backend, "example", "latest", "first")
	require.False(t, client.SupportsIfMatch())
	observed, err := client.ResolveTag(t.Context(), "example", "latest")
	require.NoError(t, err)
	require.Equal(t, old.Digest, observed.Digest)
	require.True(t, client.SupportsIfMatch())
	data := manifestBytes(t, oci.IndexOrManifest{
		SchemaVersion: 2, MediaType: oci.MediaTypeImageIndex,
		Annotations: map[string]string{"version": "second"},
	})
	params := &oci.PushManifestParameters{Tags: []string{"latest"}, IfMatch: strconv.Quote(old.Digest.String())}
	next, err := client.PushManifest(t.Context(), "example", data, oci.MediaTypeImageIndex, params)
	require.NoError(t, err)
	_, err = client.PushManifest(t.Context(), "example", data, oci.MediaTypeImageIndex, params)
	require.ErrorIs(t, err, oci.ErrManifestInvalid)
	require.ErrorContains(t, err, "If-Match does not match the current tag digest")
	var httpErr oci.HTTPError
	require.ErrorAs(t, err, &httpErr)
	require.Equal(t, 400, httpErr.StatusCode())
	current, err := client.ResolveTag(t.Context(), "example", "latest")
	require.NoError(t, err)
	require.Equal(t, next.Digest, current.Digest)
	entries, err := oci.All(backend.TagHistory(t.Context(), "example", "latest", nil))
	require.NoError(t, err)
	require.Len(t, entries, 2)
}
