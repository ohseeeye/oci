package ocisqlite

import (
	"net/http/httptest"
	"net/url"
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
