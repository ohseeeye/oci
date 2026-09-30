package ociserver

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/ocimem"
	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/stretchr/testify/require"
)

type tagHistoryBackend struct {
	*oci.Funcs
	list func(context.Context, string, string, *oci.TagHistoryParameters) iter.Seq2[oci.Descriptor, error]
}

func (b *tagHistoryBackend) TagHistory(ctx context.Context, repo, tag string, params *oci.TagHistoryParameters) iter.Seq2[oci.Descriptor, error] {
	return b.list(ctx, repo, tag, params)
}

func getTagHistory(t *testing.T, srv *Server, target string) (*httptest.ResponseRecorder, oci.IndexOrManifest) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	var index oci.IndexOrManifest
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &index))
	}
	return rec, index
}

func TestTagHistoryRouteRequiresCapability(t *testing.T) {
	plain, err := New(&oci.Funcs{}, nil)
	require.NoError(t, err)
	rec, _ := getTagHistory(t, plain, "/v2/example/_oci/tag-history/latest?n=0")
	require.Equal(t, http.StatusNotFound, rec.Code)

	withHistory, err := New(ocimem.New(), nil)
	require.NoError(t, err)
	rec, index := getTagHistory(t, withHistory, "/v2/example/_oci/tag-history/latest?n=0")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, index.Manifests)
	require.Equal(t, oci.MediaTypeImageIndex, rec.Header().Get("Content-Type"))
	require.Empty(t, rec.Header().Get("Link"))
}

func TestTagHistoryResponseAndPagination(t *testing.T) {
	registry := ocimem.New()
	for _, version := range []string{"one", "two"} {
		manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[],"annotations":{"version":"` + version + `"}}`)
		_, err := registry.PushManifest(t.Context(), "example", manifest, oci.MediaTypeImageIndex, &oci.PushManifestParameters{Tags: []string{"latest"}})
		require.NoError(t, err)
	}
	require.NoError(t, registry.DeleteTag(t.Context(), "example", "latest"))
	srv, err := New(registry, nil)
	require.NoError(t, err)

	target := "/v2/example/_oci/tag-history/latest?n=1"
	var got []oci.Descriptor
	for range 3 {
		rec, index := getTagHistory(t, srv, target)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, oci.MediaTypeImageIndex, index.MediaType)
		require.Equal(t, 2, index.SchemaVersion)
		require.Len(t, index.Manifests, 1)
		require.Equal(t, rec.Body.Len(), int(rec.Result().ContentLength))
		got = append(got, index.Manifests[0])
		link := rec.Header().Get("Link")
		if len(got) == 3 {
			require.Empty(t, link)
			break
		}
		require.True(t, strings.HasSuffix(link, `; rel="next"`))
		next, found := strings.CutPrefix(link, "<")
		require.True(t, found)
		next, _, found = strings.Cut(next, ">")
		require.True(t, found)
		u, err := url.Parse(next)
		require.NoError(t, err)
		require.Equal(t, "1", u.Query().Get("n"))
		require.Equal(t, index.Manifests[0].Annotations[oci.TagHistoryTimestampAnnotation], u.Query().Get("before"))
		target = next
	}
	require.Equal(t, []string{oci.TagHistoryEventDeleted, oci.TagHistoryEventCreated, oci.TagHistoryEventCreated}, []string{
		got[0].Annotations[oci.TagHistoryEventAnnotation],
		got[1].Annotations[oci.TagHistoryEventAnnotation],
		got[2].Annotations[oci.TagHistoryEventAnnotation],
	})
	for _, desc := range got {
		_, err := time.Parse(time.RFC3339Nano, desc.Annotations[oci.TagHistoryTimestampAnnotation])
		require.NoError(t, err)
	}
	zero, index := getTagHistory(t, srv, "/v2/example/_oci/tag-history/latest?n=0")
	require.Equal(t, http.StatusOK, zero.Code)
	require.Empty(t, index.Manifests)
	require.Empty(t, zero.Header().Get("Link"))
	unknown, index := getTagHistory(t, srv, "/v2/missing/_oci/tag-history/latest")
	require.Equal(t, http.StatusOK, unknown.Code)
	require.Empty(t, index.Manifests)
}

func TestTagHistoryQueryValidation(t *testing.T) {
	backend := &tagHistoryBackend{Funcs: &oci.Funcs{}, list: func(context.Context, string, string, *oci.TagHistoryParameters) iter.Seq2[oci.Descriptor, error] {
		t.Fatal("invalid request reached backend")
		return nil
	}}
	srv, err := New(backend, nil)
	require.NoError(t, err)
	for _, target := range []string{
		"/v2/example/_oci/tag-history/-bad",
		"/v2/example/_oci/tag-history/latest?n=-1",
		"/v2/example/_oci/tag-history/latest?n=no",
		"/v2/example/_oci/tag-history/latest?n=",
		"/v2/example/_oci/tag-history/latest?n=1&n=2",
		"/v2/example/_oci/tag-history/latest?before=bad",
		"/v2/example/_oci/tag-history/latest?since=",
		"/v2/example/_oci/tag-history/latest?digest=bad",
	} {
		rec, _ := getTagHistory(t, srv, target)
		require.Equal(t, http.StatusBadRequest, rec.Code, target)
	}
}

func TestTagHistoryPassesFiltersAndPreservesLinkQuery(t *testing.T) {
	digest := ocidigest.FromBytes([]byte("manifest"))
	before := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	since := before.Add(-24 * time.Hour)
	var calls int
	backend := &tagHistoryBackend{Funcs: &oci.Funcs{}, list: func(_ context.Context, repo, tag string, params *oci.TagHistoryParameters) iter.Seq2[oci.Descriptor, error] {
		calls++
		require.Equal(t, "example", repo)
		require.Equal(t, "latest", tag)
		require.Equal(t, 2, *params.Limit) // One result plus lookahead.
		require.Equal(t, before, params.Before)
		require.Equal(t, since, params.Since)
		require.Equal(t, digest, params.Digest)
		return oci.SliceSeq([]oci.Descriptor{
			{Digest: digest, MediaType: oci.MediaTypeImageIndex, Size: 10, Annotations: map[string]string{oci.TagHistoryTimestampAnnotation: before.Add(-time.Hour).Format(time.RFC3339Nano)}},
			{Digest: digest, MediaType: oci.MediaTypeImageIndex, Size: 10, Annotations: map[string]string{oci.TagHistoryTimestampAnnotation: before.Add(-2 * time.Hour).Format(time.RFC3339Nano)}},
		})
	}}
	srv, err := New(backend, nil)
	require.NoError(t, err)
	query := url.Values{"n": {"1"}, "before": {before.Format(time.RFC3339Nano)}, "since": {since.Format(time.RFC3339Nano)}, "digest": {digest.String()}}
	rec, _ := getTagHistory(t, srv, "/v2/example/_oci/tag-history/latest?"+query.Encode())
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, calls)
	link := rec.Header().Get("Link")
	require.Contains(t, link, "since="+url.QueryEscape(since.Format(time.RFC3339Nano)))
	require.Contains(t, link, "digest="+url.QueryEscape(digest.String()))
	require.Contains(t, link, "before="+url.QueryEscape(before.Add(-time.Hour).Format(time.RFC3339Nano)))
}

func TestTagHistoryBackendErrors(t *testing.T) {
	backend := &tagHistoryBackend{Funcs: &oci.Funcs{}}
	srv, err := New(backend, nil)
	require.NoError(t, err)
	for _, tc := range []struct {
		err  error
		want int
	}{
		{oci.ErrNameUnknown, http.StatusOK},
		{oci.ErrManifestUnknown, http.StatusOK},
		{oci.ErrUnsupported, http.StatusNotFound},
		{errors.New("failure"), http.StatusInternalServerError},
	} {
		backend.list = func(context.Context, string, string, *oci.TagHistoryParameters) iter.Seq2[oci.Descriptor, error] {
			return oci.ErrorSeq[oci.Descriptor](tc.err)
		}
		rec, index := getTagHistory(t, srv, "/v2/example/_oci/tag-history/latest")
		require.Equal(t, tc.want, rec.Code)
		if tc.want == http.StatusOK {
			require.Empty(t, index.Manifests)
		}
	}
}
