package ociclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/ocimem"
	"github.com/ohseeeye/oci/ocimiddleware"
	"github.com/ohseeeye/oci/ociserver"
	"github.com/ohseeeye/oci/pkg/ociauth"
	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/stretchr/testify/require"
)

func TestCopyImage(t *testing.T) {
	c, err := New("registry.example", &Options{
		Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
			require.Equal(t, http.MethodPost, req.Method)
			require.Equal(t, "https", req.URL.Scheme)
			require.Equal(t, "registry.example", req.URL.Host)
			require.Equal(t, "/v2/dst/repo/_docker/copy/image", req.URL.Path)
			require.Equal(t, "src/repo", req.URL.Query().Get("from"))
			require.Equal(t, "stable", req.URL.Query().Get("reference"))
			require.Nil(t, req.Body)
			require.Empty(t, req.Header.Get("Content-Type"))

			scope := ociauth.RequestInfoFromContext(req.Context()).RequiredScope
			require.Equal(t, "repository:dst/repo:pull,push repository:src/repo:pull", scope.Canonical().String())

			return &http.Response{
				StatusCode: http.StatusCreated,
				Status:     "201 Created",
				Header: http.Header{
					"Content-Type":          {oci.MediaTypeImageManifest},
					"Docker-Content-Digest": {testDigest.String()},
				},
				Body:    http.NoBody,
				Request: req,
			}, nil
		}),
	})
	require.NoError(t, err)

	desc, err := c.CopyImage(context.Background(), "src/repo", "dst/repo", "stable")
	require.NoError(t, err)
	require.Equal(t, testDigest, desc.Digest)
	require.Equal(t, oci.MediaTypeImageManifest, desc.MediaType)
	require.Zero(t, desc.Size)
}

func newManifestTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	client, err := New(srv.Listener.Addr().String(), &Options{Insecure: true})
	require.NoError(t, err)
	return client
}

func TestManifestIfMatchDiscovery(t *testing.T) {
	data := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	digest := ocidigest.FromBytes(data)
	etag := strconv.Quote(digest.String())
	for _, read := range []string{"GET tag", "HEAD tag", "GET digest", "HEAD digest", "GET blob", "HEAD blob"} {
		t.Run(read, func(t *testing.T) {
			for _, test := range []struct {
				name   string
				etags  []string
				status int
			}{
				{"strong", []string{etag}, http.StatusOK},
				{"absent", nil, http.StatusOK},
				{"weak", []string{"W/" + etag}, http.StatusOK},
				{"unquoted", []string{digest.String()}, http.StatusOK},
				{"other", []string{`"other"`}, http.StatusOK},
				{"multiple", []string{etag, etag}, http.StatusOK},
				{"failed", []string{etag}, http.StatusNotFound},
			} {
				t.Run(test.name, func(t *testing.T) {
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", oci.MediaTypeImageIndex)
						w.Header().Set("Content-Length", strconv.Itoa(len(data)))
						w.Header().Set("Docker-Content-Digest", digest.String())
						for _, value := range test.etags {
							w.Header().Add("ETag", value)
						}
						w.WriteHeader(test.status)
						if r.Method == http.MethodGet {
							_, _ = w.Write(data)
						}
					}))
					t.Cleanup(srv.Close)
					client := newManifestTestClient(t, srv)
					require.False(t, client.SupportsIfMatch())
					var err error
					var body oci.BlobReader
					switch read {
					case "GET tag":
						body, err = client.GetTag(t.Context(), "example/app", "latest")
					case "HEAD tag":
						_, err = client.ResolveTag(t.Context(), "example/app", "latest")
					case "GET digest":
						body, err = client.GetManifest(t.Context(), "example/app", digest)
					case "HEAD digest":
						_, err = client.ResolveManifest(t.Context(), "example/app", digest)
					case "GET blob":
						body, err = client.GetBlob(t.Context(), "example/app", digest)
					case "HEAD blob":
						_, err = client.ResolveBlob(t.Context(), "example/app", digest)
					}
					if test.status == http.StatusOK {
						require.NoError(t, err)
						if body != nil {
							_, err = io.Copy(io.Discard, body)
							require.NoError(t, err)
							require.NoError(t, body.Close())
						}
					} else {
						require.Error(t, err)
					}
					want := test.name == "strong" && (read == "GET tag" || read == "HEAD tag")
					require.Equal(t, want, client.SupportsIfMatch())
					other := newManifestTestClient(t, srv)
					require.False(t, other.SupportsIfMatch(), "discovery belongs to the client instance")
				})
			}
		})
	}
}

func TestConditionalManifestPush(t *testing.T) {
	backend := ocimem.New()
	handler, err := ociserver.New(ocimiddleware.Sub(backend, "prefix"), nil)
	require.NoError(t, err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := newManifestTestClient(t, srv)
	require.False(t, client.SupportsIfMatch())
	first := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	old, err := client.PushManifest(t.Context(), "example/app", first, oci.MediaTypeImageIndex, &oci.PushManifestParameters{Tags: []string{"latest"}})
	require.NoError(t, err)
	require.False(t, client.SupportsIfMatch(), "PUT responses do not enable discovery")
	observed, err := client.ResolveTag(t.Context(), "example/app", "latest")
	require.NoError(t, err)
	require.Equal(t, old.Digest, observed.Digest)
	require.True(t, client.SupportsIfMatch())
	second := []byte(strings.Replace(string(first), `"manifests":[]`, `"manifests":[],"annotations":{"version":"two"}`, 1))
	condition := strconv.Quote(old.Digest.String())
	next, err := client.PushManifest(t.Context(), "example/app", second, oci.MediaTypeImageIndex, &oci.PushManifestParameters{Tags: []string{"latest"}, IfMatch: condition})
	require.NoError(t, err)
	_, err = client.PushManifest(t.Context(), "example/app", first, oci.MediaTypeImageIndex, &oci.PushManifestParameters{Tags: []string{"latest"}, IfMatch: condition})
	require.ErrorIs(t, err, oci.ErrManifestInvalid)
	require.ErrorContains(t, err, "If-Match does not match the current tag digest")
	current, err := client.ResolveTag(t.Context(), "example/app", "latest")
	require.NoError(t, err)
	require.Equal(t, next.Digest, current.Digest)
	entries, err := oci.All(backend.TagHistory(t.Context(), "prefix/example/app", "latest", nil))
	require.NoError(t, err)
	require.Len(t, entries, 2, "middleware and client must preserve the condition on failed pushes")
}

func TestConditionalManifestPushNoFallback(t *testing.T) {
	var calls atomic.Int32
	type request struct{ method, path, query, condition string }
	requests := make(chan request, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			digest := ocidigest.FromBytes([]byte("previous"))
			w.Header().Set("Content-Length", "8")
			w.Header().Set("Docker-Content-Digest", digest.String())
			w.Header().Set("ETag", strconv.Quote(digest.String()))
			return
		}
		calls.Add(1)
		requests <- request{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("If-Match")}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errors":[{"code":"MANIFEST_INVALID","message":"If-Match does not match the current tag digest"}]}`))
	}))
	t.Cleanup(srv.Close)
	client := newManifestTestClient(t, srv)
	_, err := client.ResolveTag(t.Context(), "example/app", "latest")
	require.NoError(t, err)
	_, err = client.PushManifest(t.Context(), "example/app", []byte("data"), oci.MediaTypeImageIndex, &oci.PushManifestParameters{Tags: []string{"latest"}, IfMatch: `"previous"`})
	require.ErrorIs(t, err, oci.ErrManifestInvalid)
	require.EqualValues(t, 1, calls.Load(), "a conditional failure must not retry without the condition")
	got := <-requests
	require.Equal(t, request{http.MethodPut, "/v2/example/app/manifests/latest", "", `"previous"`}, got)
}

func TestManifestIfMatchDiscoveryControlsPush(t *testing.T) {
	data := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	digest := ocidigest.FromBytes(data)
	condition := strconv.Quote(digest.String())
	conditions := make(chan string, 4)
	var advertise atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", oci.MediaTypeImageIndex)
		w.Header().Set("Docker-Content-Digest", digest.String())
		if r.Method == http.MethodPut {
			conditions <- r.Header.Get("If-Match")
			w.Header().Set("OCI-Tag", "latest")
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if advertise.Load() {
			w.Header().Set("ETag", condition)
		}
	}))
	t.Cleanup(srv.Close)
	client := newManifestTestClient(t, srv)
	params := &oci.PushManifestParameters{Tags: []string{"latest"}, IfMatch: condition}
	push := func(want string) {
		t.Helper()
		_, err := client.PushManifest(t.Context(), "example/app", data, oci.MediaTypeImageIndex, params)
		require.NoError(t, err)
		require.Equal(t, want, <-conditions)
	}
	push("")
	_, err := client.ResolveTag(t.Context(), "example/app", "latest")
	require.NoError(t, err)
	require.False(t, client.SupportsIfMatch())
	push("")
	advertise.Store(true)
	_, err = client.ResolveTag(t.Context(), "example/app", "latest")
	require.NoError(t, err)
	require.True(t, client.SupportsIfMatch())
	push(condition)
	// Discovery is registry-wide: a later response without the header does
	// not disable conditions, and ordinary pushes still omit them.
	advertise.Store(false)
	_, err = client.ResolveTag(t.Context(), "another/repo", "stable")
	require.NoError(t, err)
	require.True(t, client.SupportsIfMatch())
	params.IfMatch = ""
	push("")
}
