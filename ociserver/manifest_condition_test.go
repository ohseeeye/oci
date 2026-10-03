package ociserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/ocimem"
	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/stretchr/testify/require"
)

const conditionIndex = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`

func conditionalPut(t *testing.T, srv *Server, target, body string, conditions ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, target, strings.NewReader(body))
	req.Header.Set("Content-Type", oci.MediaTypeImageIndex)
	for _, condition := range conditions {
		req.Header.Add("If-Match", condition)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestManifestIfMatch(t *testing.T) {
	r := ocimem.New()
	srv, err := New(r, nil)
	require.NoError(t, err)
	target := "/v2/example/app/manifests/latest"
	first := conditionalPut(t, srv, target, conditionIndex)
	require.Equal(t, http.StatusCreated, first.Code)
	etag := strconv.Quote(first.Header().Get("Docker-Content-Digest"))
	require.Equal(t, etag, first.Header().Get("ETag"))
	for _, reference := range []string{"latest", first.Header().Get("Docker-Content-Digest")} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(method+"/"+reference, func(t *testing.T) {
				rec := httptest.NewRecorder()
				srv.ServeHTTP(rec, httptest.NewRequest(method, "/v2/example/app/manifests/"+reference, nil))
				require.Equal(t, http.StatusOK, rec.Code)
				require.Equal(t, etag, rec.Header().Get("ETag"))
				require.Equal(t, first.Header().Get("Docker-Content-Digest"), rec.Header().Get("Docker-Content-Digest"))
			})
		}
	}
	second := strings.Replace(conditionIndex, `"manifests":[]`, `"manifests":[],"annotations":{"version":"two"}`, 1)
	stale := conditionalPut(t, srv, target, second, `"stale"`)
	require.Equal(t, http.StatusPreconditionFailed, stale.Code)
	require.Contains(t, stale.Body.String(), "PRECONDITION_FAILED")
	current, err := r.ResolveTag(t.Context(), "example/app", "latest")
	require.NoError(t, err)
	require.Equal(t, ocidigest.FromBytes([]byte(conditionIndex)), current.Digest)
	for _, condition := range []string{"", " ", current.Digest.String()} {
		require.Equal(t, http.StatusBadRequest, conditionalPut(t, srv, target, second, condition).Code)
	}
	digestURL := "/v2/example/app/manifests/" + ocidigest.FromBytes([]byte(second)).String()
	require.Equal(t, http.StatusBadRequest, conditionalPut(t, srv, digestURL, second, etag).Code)
	require.Equal(t, http.StatusBadRequest, conditionalPut(t, srv, target+"?tag=extra", second, etag).Code)
	require.Equal(t, http.StatusPreconditionFailed, conditionalPut(t, srv, "/v2/missing/app/manifests/latest", second, "*").Code)
	require.Equal(t, http.StatusCreated, conditionalPut(t, srv, target, second, `"other"`, etag).Code, "multiple header lines form one list")
	// Normal pushes remain unconditional after a successful conditional update.
	require.Equal(t, http.StatusCreated, conditionalPut(t, srv, target, conditionIndex).Code)
}

func TestManifestIfMatchPassesThrough(t *testing.T) {
	var got *oci.PushManifestParameters
	srv, err := New(&oci.Funcs{PushManifest_: func(_ context.Context, _ string, _ []byte, _ string, p *oci.PushManifestParameters) (oci.Descriptor, error) {
		captured := *p
		got = &captured
		return oci.Descriptor{}, nil
	}}, nil)
	require.NoError(t, err)
	condition := `"sha256:previous"`
	rec := conditionalPut(t, srv, "/v2/example/manifests/latest", conditionIndex, condition)
	require.Equal(t, http.StatusCreated, rec.Code, "a backend may ignore the experimental parameter")
	require.NotNil(t, got)
	require.Equal(t, condition, got.IfMatch)
	require.Equal(t, []string{"latest"}, got.Tags)
}
