package ociserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/ociclient"
	"github.com/ohseeeye/oci/ocilayout"
	"github.com/ohseeeye/oci/ocimem"
	"github.com/ohseeeye/oci/ocimiddleware"
	"github.com/stretchr/testify/require"
)

// names must be in lexical order, as required by Registry.Repositories.
func catalogTestRegistry(names []string) *oci.Funcs {
	return &oci.Funcs{
		Repositories_: func(_ context.Context, last string) iter.Seq2[string, error] {
			return func(yield func(string, error) bool) {
				for _, name := range names {
					if name > last && !yield(name, nil) {
						return
					}
				}
			}
		},
	}
}

func TestCatalogGet(t *testing.T) {
	names := []string{"alpha", "library/alpine", "team/app", "team/app/tools", "zulu"}
	for _, test := range []struct {
		name, query string
		names       []string
		want        []string
		next        string
	}{
		{name: "empty", want: []string{}},
		{name: "all", names: names, want: names},
		{name: "zero is unlimited", query: "n=0", names: names, want: names},
		{name: "first page", query: "n=2", names: names, want: names[:2], next: "library/alpine"},
		{name: "middle page", query: "n=2&last=library%2Falpine", names: names, want: names[2:4], next: "team/app/tools"},
		{name: "last page", query: "n=2&last=team%2Fapp%2Ftools", names: names, want: names[4:]},
		{name: "exact page", query: "n=2", names: names[:2], want: names[:2]},
		{name: "after final name", query: "n=2&last=zulu", names: names, want: []string{}},
		{name: "nonexistent cursor", query: "n=2&last=beta", names: names, want: names[1:3], next: "team/app"},
		{name: "cursor without limit", query: "last=library%2Falpine", names: names, want: names[2:]},
		{name: "maximum limit", query: "n=" + strconv.Itoa(math.MaxInt), names: names, want: names},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv, err := New(catalogTestRegistry(test.names), nil)
			require.NoError(t, err)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/_catalog?"+test.query, nil))
			require.Equal(t, http.StatusOK, rec.Code)
			require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			var body struct {
				Repositories []string `json:"repositories"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Equal(t, test.want, body.Repositories)
			if test.next == "" {
				require.Empty(t, rec.Header().Get("Link"))
			} else {
				query, err := url.ParseQuery(test.query)
				require.NoError(t, err)
				query.Set("last", test.next)
				require.Equal(t, `</v2/_catalog?`+query.Encode()+`>; rel="next"`, rec.Header().Get("Link"))
			}
		})
	}
}

func TestCatalogRejectsInvalidParameters(t *testing.T) {
	for _, query := range []string{
		"n=-1", "n=invalid", "n=1.5", "n=99999999999999999999999999",
		"last=-invalid", "last=Uppercase", "last=team//app", "last=" + strings.Repeat("a", 256),
	} {
		t.Run(query, func(t *testing.T) {
			srv, err := New(&oci.Funcs{
				Repositories_: func(context.Context, string) iter.Seq2[string, error] {
					t.Fatal("invalid request reached registry")
					return nil
				},
			}, nil)
			require.NoError(t, err)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/_catalog?"+query, nil))
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Contains(t, rec.Body.String(), `"code":"BAD_REQUEST"`)
		})
	}
}

func TestCatalogStopsAfterLookahead(t *testing.T) {
	var consumed int
	srv, err := New(&oci.Funcs{
		Repositories_: func(context.Context, string) iter.Seq2[string, error] {
			return func(yield func(string, error) bool) {
				for _, name := range []string{"a", "b", "c", "d"} {
					consumed++
					if !yield(name, nil) {
						return
					}
				}
			}
		},
	}, nil)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/_catalog?n=2&extra=keep", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 3, consumed)
	require.JSONEq(t, `{"repositories":["a","b"]}`, rec.Body.String())
	require.Equal(t, `</v2/_catalog?extra=keep&last=b&n=2>; rel="next"`, rec.Header().Get("Link"))
}

func TestCatalogBackendErrors(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"unsupported", oci.ErrUnsupported, http.StatusNotImplemented, "UNSUPPORTED"},
		{"unauthorized", oci.ErrUnauthorized, http.StatusUnauthorized, "UNAUTHORIZED"},
		{"denied", oci.ErrDenied, http.StatusForbidden, "DENIED"},
		{"unexpected", errors.New("private backend details"), http.StatusInternalServerError, "SERVER_ERROR"},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv, err := New(&oci.Funcs{
				Repositories_: func(context.Context, string) iter.Seq2[string, error] {
					return func(yield func(string, error) bool) {
						if yield("a", nil) {
							yield("", fmt.Errorf("listing failed: %w", test.err))
						}
					}
				},
			}, nil)
			require.NoError(t, err)
			rec := httptest.NewRecorder()
			// Errors during lookahead must not publish a partial successful page.
			srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/_catalog?n=1", nil))
			require.Equal(t, test.status, rec.Code)
			require.Contains(t, rec.Body.String(), `"code":"`+test.code+`"`)
			require.NotContains(t, rec.Body.String(), "repositories")
			require.NotContains(t, rec.Body.String(), "private backend details")
			require.Empty(t, rec.Header().Get("Link"))
		})
	}
}

func TestCatalogAuthorizer(t *testing.T) {
	type identityKey struct{}
	var lookups int
	backend := &authorizingRegistry{
		Registry: &oci.Funcs{
			Repositories_: func(ctx context.Context, _ string) iter.Seq2[string, error] {
				lookups++
				require.Equal(t, "reader", ctx.Value(identityKey{}))
				return oci.SliceSeq([]string{"team/app"})
			},
		},
		authorize: func(w http.ResponseWriter, r *http.Request, repo, reference string, next http.Handler) {
			require.Empty(t, repo)
			require.Empty(t, reference)
			if r.Header.Get("Authorization") != "Bearer valid" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="https://auth.example.test/token"`)
				returnError(w, ErrUnauthorized())
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, "reader")))
		},
	}
	srv, err := New(backend, nil)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	// Authorization precedes even query validation.
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/_catalog?n=bad", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.NotEmpty(t, rec.Header().Get("WWW-Authenticate"))
	require.Zero(t, lookups)

	req := httptest.NewRequest(http.MethodGet, "/v2/_catalog", nil)
	req.Header.Set("Authorization", "Bearer valid")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, lookups)
}

func TestCatalogFiltersBeforePagination(t *testing.T) {
	backend := ocimiddleware.AccessChecker(catalogTestRegistry([]string{"a", "b", "c", "d"}), func(repo string, access ocimiddleware.AccessKind) error {
		if access == ocimiddleware.AccessRead && (repo == "a" || repo == "c") {
			return oci.ErrDenied
		}
		return nil
	})
	srv, err := New(backend, nil)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/_catalog?n=1", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"repositories":["b"]}`, rec.Body.String())
	require.Equal(t, `</v2/_catalog?last=b&n=1>; rel="next"`, rec.Header().Get("Link"))

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/_catalog?n=1&last=b", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"repositories":["d"]}`, rec.Body.String())
	require.Empty(t, rec.Header().Get("Link"))
}

func TestCatalogClientRoundTrip(t *testing.T) {
	for _, backendName := range []string{"memory", "layout"} {
		t.Run(backendName, func(t *testing.T) {
			var backend oci.Registry = ocimem.New()
			if backendName == "layout" {
				var err error
				backend, err = ocilayout.New(t.TempDir(), nil)
				require.NoError(t, err)
			}
			ctx := context.Background()
			names := []string{"a", "library/alpine", "team/app"}
			const index = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`
			for _, name := range names {
				_, err := backend.PushManifest(ctx, name, []byte(index), oci.MediaTypeImageIndex, &oci.PushManifestParameters{Tags: []string{"latest"}})
				require.NoError(t, err)
			}
			srv, err := New(backend, nil)
			require.NoError(t, err)
			// Force pagination even though the client doesn't request a page size,
			// exercising its next-Link handling against the actual catalog route.
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				query := r.URL.Query()
				query.Set("n", "1")
				r.URL.RawQuery = query.Encode()
				srv.ServeHTTP(w, r)
			}))
			t.Cleanup(httpServer.Close)
			client, err := ociclient.New(strings.TrimPrefix(httpServer.URL, "http://"), &ociclient.Options{Insecure: true})
			require.NoError(t, err)
			got, err := oci.All(client.Repositories(ctx, ""))
			require.NoError(t, err)
			require.Equal(t, names, got)
			got, err = oci.All(client.Repositories(ctx, "library/alpine"))
			require.NoError(t, err)
			require.Equal(t, names[2:], got)
		})
	}
}
