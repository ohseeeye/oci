package ociserver

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestAcceptsMediaType(t *testing.T) {
	t.Parallel()

	const manifestMediaType = "application/vnd.oci.image.manifest.v1+json"

	tests := []struct {
		name         string
		acceptHeader []string
		want         bool
	}{
		{
			name: "empty accept allows stored type",
			want: true,
		},
		{
			name:         "exact match",
			acceptHeader: []string{manifestMediaType},
			want:         true,
		},
		{
			name:         "exact match with parameters",
			acceptHeader: []string{manifestMediaType + "; q=0.8"},
			want:         true,
		},
		{
			name:         "multiple values with match",
			acceptHeader: []string{"application/vnd.oci.image.index.v1+json, " + manifestMediaType},
			want:         true,
		},
		{
			name: "multiple header lines with match",
			acceptHeader: []string{
				"application/vnd.oci.image.index.v1+json",
				manifestMediaType,
			},
			want: true,
		},
		{
			name:         "any type wildcard",
			acceptHeader: []string{"*/*"},
			want:         true,
		},
		{
			name:         "subtype wildcard",
			acceptHeader: []string{"application/*"},
			want:         true,
		},
		{
			name:         "quality zero rejects otherwise matching type",
			acceptHeader: []string{manifestMediaType + "; q=0"},
			want:         false,
		},
		{
			name:         "different type",
			acceptHeader: []string{"application/vnd.oci.image.index.v1+json"},
			want:         false,
		},
		{
			name:         "different top level wildcard",
			acceptHeader: []string{"text/*"},
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := acceptsMediaType(tt.acceptHeader, manifestMediaType); got != tt.want {
				t.Fatalf("acceptsMediaType(%q, %q) = %v, want %v", tt.acceptHeader, manifestMediaType, got, tt.want)
			}
		})
	}
}

func TestManifestHandlersValidateTags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		method  string
		target  string
		handler func(*Server) http.HandlerFunc
		body    []byte
	}{
		{name: "get reference", method: http.MethodGet, target: "/v2/repo/manifests/-bad", handler: (*Server).manifestHeadGet},
		{name: "delete reference", method: http.MethodDelete, target: "/v2/repo/manifests/-bad", handler: (*Server).manifestDelete},
		{
			name:    "put tag query",
			method:  http.MethodPut,
			target:  "/v2/repo/manifests/latest?tag=-bad",
			handler: (*Server).manifestPut,
			body:    []byte(`{}`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := &Server{db: (*oci.Funcs)(nil)}
			req := httptest.NewRequest(tt.method, tt.target, bytes.NewReader(tt.body))
			rec := httptest.NewRecorder()

			serveTestRoute(t, `/v2/*name/manifests/:reference`, tt.handler(s), rec, req)

			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Contains(t, rec.Body.String(), `"code":"MANIFEST_INVALID"`)
		})
	}
}

func TestManifestPutValidatesContentType(t *testing.T) {
	t.Parallel()

	s := &Server{db: (*oci.Funcs)(nil)}
	req := httptest.NewRequest(http.MethodPut, "/v2/repo/manifests/latest", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", `application/vnd.oci.image.manifest.v1+json; broken`)
	rec := httptest.NewRecorder()

	serveTestRoute(t, `/v2/*name/manifests/:reference`, s.manifestPut(), rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), `"code":"MANIFEST_INVALID"`)
}

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
	stale := conditionalPut(t, srv, target, second, strconv.Quote(ocidigest.FromBytes([]byte(second)).String()))
	require.Equal(t, http.StatusBadRequest, stale.Code)
	var response oci.WireErrors
	require.NoError(t, json.Unmarshal(stale.Body.Bytes(), &response))
	require.Len(t, response.Errors, 1)
	require.Equal(t, "MANIFEST_INVALID", response.Errors[0].Code())
	require.Contains(t, response.Errors[0].Message, "If-Match does not match the current tag digest")
	current, err := r.ResolveTag(t.Context(), "example/app", "latest")
	require.NoError(t, err)
	require.Equal(t, ocidigest.FromBytes([]byte(conditionIndex)), current.Digest)
	for _, condition := range []string{"", " ", current.Digest.String(), "*", "W/" + etag, `"other", ` + etag} {
		require.Equal(t, http.StatusBadRequest, conditionalPut(t, srv, target, second, condition).Code)
	}
	digestURL := "/v2/example/app/manifests/" + ocidigest.FromBytes([]byte(second)).String()
	require.Equal(t, http.StatusBadRequest, conditionalPut(t, srv, digestURL, second, etag).Code)
	require.Equal(t, http.StatusBadRequest, conditionalPut(t, srv, target+"?tag=extra", second, etag).Code)
	require.Equal(t, http.StatusBadRequest, conditionalPut(t, srv, "/v2/missing/app/manifests/latest", second, etag).Code)
	require.Equal(t, http.StatusBadRequest, conditionalPut(t, srv, target, second, etag, etag).Code, "multiple header values are not supported")
	require.Equal(t, http.StatusCreated, conditionalPut(t, srv, target, second, etag).Code)
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
