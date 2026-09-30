package ociclient_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/ociclient"
	"github.com/ohseeeye/oci/ocimem"
	"github.com/ohseeeye/oci/ociserver"
	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var _ oci.TagHistory = (*ociclient.Client)(nil)

func historyClient(t *testing.T, handler http.Handler) *ociclient.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	client, err := ociclient.New(u.Host, &ociclient.Options{Insecure: true})
	require.NoError(t, err)
	return client
}

func TestTagHistoryRoundTrip(t *testing.T) {
	backend := ocimem.New()
	server, err := ociserver.New(backend, nil)
	require.NoError(t, err)
	client := historyClient(t, server)
	for _, version := range []string{"first", "second"} {
		data := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[],"annotations":{"version":%q}}`, oci.MediaTypeImageIndex, version))
		_, err := client.PushManifest(t.Context(), "example", data, oci.MediaTypeImageIndex, &oci.PushManifestParameters{Tags: []string{"latest"}})
		require.NoError(t, err)
	}
	require.NoError(t, client.DeleteTag(t.Context(), "example", "latest"))

	entries, err := oci.All(client.TagHistory(t.Context(), "example", "latest", nil))
	require.NoError(t, err)
	require.Len(t, entries, 3)
	require.Equal(t, oci.TagHistoryEventDeleted, entries[0].Annotations[oci.TagHistoryEventAnnotation])
	limit := 2
	page, err := oci.All(client.TagHistory(t.Context(), "example", "latest", &oci.TagHistoryParameters{Limit: &limit}))
	require.NoError(t, err)
	require.Equal(t, entries[:2], page)
	cursor, err := time.Parse(time.RFC3339Nano, page[1].Annotations[oci.TagHistoryTimestampAnnotation])
	require.NoError(t, err)
	older, err := oci.All(client.TagHistory(t.Context(), "example", "latest", &oci.TagHistoryParameters{Before: cursor}))
	require.NoError(t, err)
	require.Equal(t, entries[2:], older)
	zero := 0
	empty, err := oci.All(client.TagHistory(t.Context(), "example", "latest", &oci.TagHistoryParameters{Limit: &zero}))
	require.NoError(t, err)
	require.Empty(t, empty)
	empty, err = oci.All(client.TagHistory(t.Context(), "missing", "latest", nil))
	require.NoError(t, err)
	require.Empty(t, empty)
}

func TestTagHistoryUnsupported(t *testing.T) {
	server, err := ociserver.New(&oci.Funcs{}, nil)
	require.NoError(t, err)
	client := historyClient(t, server)
	_, err = oci.All(client.TagHistory(t.Context(), "example", "latest", nil))
	require.ErrorIs(t, err, oci.ErrUnsupported)
	var httpErr oci.HTTPError
	require.ErrorAs(t, err, &httpErr)
	require.Equal(t, http.StatusNotFound, httpErr.StatusCode())
	// An explicit zero-length page is a capability probe and must also report
	// that the endpoint is unsupported.
	zero := 0
	_, err = oci.All(client.TagHistory(t.Context(), "example", "latest", &oci.TagHistoryParameters{Limit: &zero}))
	require.ErrorIs(t, err, oci.ErrUnsupported)
}

func historyDescriptor(i int) oci.Descriptor {
	return oci.Descriptor{
		MediaType: oci.MediaTypeImageIndex,
		Digest:    ocidigest.FromBytes([]byte(fmt.Sprint(i))),
		Size:      1,
		Annotations: map[string]string{
			oci.TagHistoryTimestampAnnotation: time.Date(2026, time.January, i+1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano),
			oci.TagHistoryEventAnnotation:     oci.TagHistoryEventCreated,
		},
	}
}

func TestTagHistoryFollowsLinkAndLimitsTotal(t *testing.T) {
	var requests int
	client := historyClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal(t, "/v2/example/_oci/tag-history/latest", r.URL.Path)
		if requests == 1 {
			assert.Equal(t, "2", r.URL.Query().Get("n"))
			assert.Empty(t, r.URL.Query().Get("cursor"))
		} else {
			assert.Equal(t, fmt.Sprint(requests), r.URL.Query().Get("cursor"))
		}
		w.Header().Set("Content-Type", oci.MediaTypeImageIndex)
		w.Header().Set("Link", fmt.Sprintf(`</v2/example/_oci/tag-history/latest?cursor=%d>; rel="next"`, requests+1))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schemaVersion": 2,
			"mediaType":     oci.MediaTypeImageIndex,
			"manifests":     []oci.Descriptor{historyDescriptor(requests)},
		})
	}))
	limit := 2
	entries, err := oci.All(client.TagHistory(t.Context(), "example", "latest", &oci.TagHistoryParameters{Limit: &limit}))
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Equal(t, 2, requests)
}

func TestTagHistoryQueryAndValidation(t *testing.T) {
	before := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	since := before.Add(-time.Hour)
	digest := ocidigest.FromBytes([]byte("manifest"))
	var requests int
	client := historyClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal(t, "0", r.URL.Query().Get("n"))
		assert.Equal(t, before.Format(time.RFC3339Nano), r.URL.Query().Get("before"))
		assert.Equal(t, since.Format(time.RFC3339Nano), r.URL.Query().Get("since"))
		assert.Equal(t, digest.String(), r.URL.Query().Get("digest"))
		w.Header().Set("Content-Type", oci.MediaTypeImageIndex)
		_, _ = w.Write([]byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`))
	}))
	zero := 0
	entries, err := oci.All(client.TagHistory(t.Context(), "example", "latest", &oci.TagHistoryParameters{Limit: &zero, Before: before, Since: since, Digest: digest}))
	require.NoError(t, err)
	require.Empty(t, entries)
	require.Equal(t, 1, requests)
	negative := -1
	for _, params := range []*oci.TagHistoryParameters{{Limit: &negative}, {Digest: "bad"}} {
		_, err := oci.All(client.TagHistory(t.Context(), "example", "latest", params))
		require.Error(t, err)
	}
	_, err = oci.All(client.TagHistory(t.Context(), "INVALID", "latest", nil))
	require.ErrorIs(t, err, oci.ErrNameInvalid)
	require.Equal(t, 1, requests)
}

func TestTagHistoryRejectsMalformedIndex(t *testing.T) {
	for _, body := range []string{
		`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","manifests":[]}`,
		`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.index.v1+json","digest":"sha256:bad","size":1}]}`,
	} {
		client := historyClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", oci.MediaTypeImageIndex)
			_, _ = w.Write([]byte(body))
		}))
		_, err := oci.All(client.TagHistory(t.Context(), "example", "latest", nil))
		require.Error(t, err)
		require.NotErrorIs(t, err, oci.ErrUnsupported)
	}
}
