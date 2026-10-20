package ociserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/stretchr/testify/require"
)

func TestServerDefaultLoggingIsSilent(t *testing.T) {
	var globalLog bytes.Buffer
	oldDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&globalLog, nil)))
	t.Cleanup(func() {
		slog.SetDefault(oldDefault)
	})

	srv, err := New(&oci.Funcs{
		ResolveBlob_: func(context.Context, string, oci.Digest) (oci.Descriptor, error) {
			return oci.Descriptor{}, errors.New("backend failed")
		},
	}, nil)
	require.NoError(t, err)

	dgst := ocidigest.FromBytes([]byte("blob"))
	req := httptest.NewRequest(http.MethodHead, "/v2/example/blobs/"+dgst.String(), nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Empty(t, globalLog.String())
}

func TestServerLogsUnexpectedFailures(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	srv, err := New(&oci.Funcs{
		ResolveBlob_: func(context.Context, string, oci.Digest) (oci.Descriptor, error) {
			return oci.Descriptor{}, errors.New("backend failed")
		},
	}, &ServerConfig{Logger: logger})
	require.NoError(t, err)

	dgst := ocidigest.FromBytes([]byte("blob"))
	req := httptest.NewRequest(http.MethodHead, "/v2/example/blobs/"+dgst.String(), nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Contains(t, logs.String(), "msg=\"resolving blob\"")
	require.Contains(t, logs.String(), "repository=example")
	require.Contains(t, logs.String(), "error=\"backend failed\"")
}

func TestServerMiddlewares(t *testing.T) {
	var calls []string
	middleware := func(name string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, name+" before")
				next.ServeHTTP(w, r)
				calls = append(calls, name+" after")
			})
		}
	}
	cfg := &ServerConfig{
		Middlewares: []func(http.Handler) http.Handler{
			middleware("first"),
			middleware("second"),
		},
	}
	srv, err := New((*oci.Funcs)(nil), cfg)
	require.NoError(t, err)

	// New must retain its own middleware slice rather than the caller's backing
	// array.
	cfg.Middlewares[0] = func(http.Handler) http.Handler {
		panic("server used the caller's mutated middleware slice")
	}

	req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, []string{
		"first before",
		"second before",
		"second after",
		"first after",
	}, calls)
	require.True(t, strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json"))
}

type authorizingRegistry struct {
	oci.Registry
	authorize func(http.ResponseWriter, *http.Request, string, string, http.Handler)
}

var _ Authorizer = (*authorizingRegistry)(nil)

func (r *authorizingRegistry) Authorize(w http.ResponseWriter, req *http.Request, repo, reference string, next http.Handler) {
	r.authorize(w, req, repo, reference, next)
}

func TestServerAuthorizerOnlyAppliesToOCIRoutes(t *testing.T) {
	var calls []string
	middleware := func(name string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	backend := &authorizingRegistry{
		Registry: (*oci.Funcs)(nil),
		authorize: func(w http.ResponseWriter, r *http.Request, _, _ string, next http.Handler) {
			calls = append(calls, "auth")
			next.ServeHTTP(w, r)
		},
	}
	srv, err := New(backend, &ServerConfig{
		Middlewares: []func(http.Handler) http.Handler{middleware("root")},
	})
	require.NoError(t, err)
	srv.Mux().Get(`/health`, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, []string{"root", "auth"}, calls)

	calls = nil
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, []string{"root"}, calls)
}

func TestServerAuthorizerPassesContextToRegistry(t *testing.T) {
	type identityKey struct{}
	const challenge = `Bearer realm="https://auth.example.test/token"`
	digest := ocidigest.FromBytes([]byte("blob"))
	var lookups int
	backend := &authorizingRegistry{
		Registry: &oci.Funcs{ResolveBlob_: func(ctx context.Context, repo string, got oci.Digest) (oci.Descriptor, error) {
			lookups++
			require.Equal(t, "alice", ctx.Value(identityKey{}))
			require.Equal(t, "example/app", repo)
			require.Equal(t, digest, got)
			return oci.Descriptor{Digest: digest, Size: 4}, nil
		}},
		authorize: func(w http.ResponseWriter, r *http.Request, repo, reference string, next http.Handler) {
			require.Equal(t, "example/app", repo)
			require.Equal(t, digest.String(), reference)
			if r.Header.Get("Authorization") != "Bearer valid" {
				w.Header().Set("WWW-Authenticate", challenge)
				returnError(w, ErrUnauthorized())
				return
			}
			ctx := context.WithValue(r.Context(), identityKey{}, "alice")
			next.ServeHTTP(w, r.WithContext(ctx))
		},
	}
	srv, err := New(backend, nil)
	require.NoError(t, err)
	target := "/v2/example/app/blobs/" + digest.String()

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, target, nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, challenge, rec.Header().Get("WWW-Authenticate"))
	require.Zero(t, lookups, "authentication must happen before registry calls")

	req := httptest.NewRequest(http.MethodHead, target, nil)
	req.Header.Set("Authorization", "Bearer valid")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, lookups)
	require.Equal(t, digest.String(), rec.Header().Get("Docker-Content-Digest"))
}

func TestServerAuthorizerCanRedirect(t *testing.T) {
	backend := &authorizingRegistry{
		Registry: (*oci.Funcs)(nil),
		authorize: func(w http.ResponseWriter, r *http.Request, _, _ string, _ http.Handler) {
			http.Redirect(w, r, "/login", http.StatusFound)
		},
	}
	srv, err := New(backend, nil)
	require.NoError(t, err)
	for _, request := range []struct{ method, target string }{
		{http.MethodGet, "/v2/"},
		{http.MethodGet, "/v2/_catalog"},
		{http.MethodGet, "/v2/example/manifests/latest"},
		{http.MethodPost, "/v2/example/blobs/uploads/"},
		{http.MethodPatch, "/v2/example/blobs/uploads/session"},
		{http.MethodPut, "/v2/example/blobs/uploads/session"},
		{http.MethodDelete, "/v2/example/blobs/uploads/session"},
	} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(request.method, request.target, nil))
		require.Equal(t, http.StatusFound, rec.Code)
		require.Equal(t, "/login", rec.Header().Get("Location"))
	}
}

func TestServerAuthorizerReceivesRepositoryAndReference(t *testing.T) {
	digest := ocidigest.FromBytes([]byte("blob")).String()
	for _, request := range []struct {
		method, target, repo, reference string
	}{
		{http.MethodGet, "/v2/", "", ""},
		{http.MethodGet, "/v2/_catalog?n=2&last=team/app", "", ""},
		{http.MethodGet, "/v2/team/example/app/manifests/latest", "team/example/app", "latest"},
		{http.MethodHead, "/v2/example/app/manifests/" + digest, "example/app", digest},
		{http.MethodPut, "/v2/example/app/manifests/release", "example/app", "release"},
		{http.MethodDelete, "/v2/example/app/manifests/" + digest, "example/app", digest},
		{http.MethodGet, "/v2/example/app/blobs/" + digest, "example/app", digest},
		{http.MethodHead, "/v2/example/app/blobs/" + digest, "example/app", digest},
		{http.MethodDelete, "/v2/example/app/blobs/" + digest, "example/app", digest},
		{http.MethodGet, "/v2/example/app/referrers/" + digest, "example/app", digest},
		{http.MethodGet, "/v2/example/app/tags/list?last=release", "example/app", ""},
		{http.MethodPost, "/v2/example/app/blobs/uploads/?mount=" + digest + "&from=source", "example/app", ""},
		{http.MethodPost, "/v2/example/app/blobs/uploads/?digest=" + digest, "example/app", ""},
		{http.MethodGet, "/v2/example/app/blobs/uploads/session", "example/app", ""},
		{http.MethodPatch, "/v2/example/app/blobs/uploads/session", "example/app", ""},
		{http.MethodPut, "/v2/example/app/blobs/uploads/session?digest=" + digest, "example/app", ""},
		{http.MethodDelete, "/v2/example/app/blobs/uploads/session", "example/app", ""},
	} {
		t.Run(request.method+" "+request.target, func(t *testing.T) {
			var calls int
			backend := &authorizingRegistry{
				Registry: (*oci.Funcs)(nil),
				authorize: func(w http.ResponseWriter, _ *http.Request, repo, reference string, _ http.Handler) {
					calls++
					require.Equal(t, request.repo, repo)
					require.Equal(t, request.reference, reference)
					w.WriteHeader(http.StatusNoContent)
				},
			}
			srv, err := New(backend, nil)
			require.NoError(t, err)
			require.Zero(t, calls, "authorization is a per-request hook")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, httptest.NewRequest(request.method, request.target, nil))
			require.Equal(t, http.StatusNoContent, rec.Code)
			require.Equal(t, 1, calls)
		})
	}
}

type testAuthError struct {
	cause   error
	respond http.HandlerFunc
}

var _ AuthError = (*testAuthError)(nil)

func (e *testAuthError) Error() string { return e.cause.Error() }
func (e *testAuthError) Unwrap() error { return e.cause }
func (e *testAuthError) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.respond(w, r)
}

func TestServerMethodAuthResponses(t *testing.T) {
	for _, response := range []string{"redirect", "challenge", "denied"} {
		for _, wrapping := range []string{"direct", "wrapped", "joined"} {
			t.Run(response+"/"+wrapping, func(t *testing.T) {
				var responses int
				var responsePath string
				authErr := &testAuthError{cause: oci.ErrManifestUnknown, respond: func(w http.ResponseWriter, r *http.Request) {
					responses++
					responsePath = r.URL.Path
					switch response {
					case "redirect":
						http.Redirect(w, r, "/login", http.StatusSeeOther)
					case "challenge":
						w.Header().Set("WWW-Authenticate", `Bearer realm="https://auth.example.test/token"`)
						returnError(w, ErrUnauthorized())
					case "denied":
						returnError(w, ErrDenied("repository is private"))
					}
				}}
				var backendErr error = authErr
				switch wrapping {
				case "wrapped":
					backendErr = fmt.Errorf("secured registry: %w", authErr)
				case "joined":
					backendErr = errors.Join(errors.New("additional detail"), authErr)
				}
				backend := &oci.Funcs{NewError: func(context.Context, string, string) error { return backendErr }}
				srv, err := New(backend, nil)
				require.NoError(t, err)
				rec := httptest.NewRecorder()
				srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/example/manifests/latest", nil))
				require.Equal(t, 1, responses)
				require.Equal(t, "/v2/example/manifests/latest", responsePath)
				switch response {
				case "redirect":
					require.Equal(t, http.StatusSeeOther, rec.Code)
					require.Equal(t, "/login", rec.Header().Get("Location"))
				case "challenge":
					require.Equal(t, http.StatusUnauthorized, rec.Code)
					require.Contains(t, rec.Header().Get("WWW-Authenticate"), "Bearer")
					require.Contains(t, rec.Body.String(), `"code":"UNAUTHORIZED"`)
				case "denied":
					require.Equal(t, http.StatusForbidden, rec.Code)
					require.Contains(t, rec.Body.String(), `"code":"DENIED"`)
				}
			})
		}
	}
}

func TestServerRegistryMethodsCanReturnAuthErrors(t *testing.T) {
	digest := ocidigest.FromBytes([]byte("blob"))
	const index = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`
	for _, test := range []struct {
		method, target, operation, body, byteRange string
	}{
		{http.MethodGet, "/v2/example/manifests/latest", "ResolveTag", "", ""},
		{http.MethodHead, "/v2/example/manifests/latest", "ResolveTag", "", ""},
		{http.MethodGet, "/v2/example/manifests/" + digest.String(), "ResolveManifest", "", ""},
		{http.MethodHead, "/v2/example/manifests/" + digest.String(), "ResolveManifest", "", ""},
		{http.MethodGet, "/v2/example/manifests/latest", "GetManifest", "", ""},
		{http.MethodGet, "/v2/example/manifests/" + digest.String(), "GetManifest", "", ""},
		{http.MethodPut, "/v2/example/manifests/latest", "PushManifest", index, ""},
		{http.MethodDelete, "/v2/example/manifests/latest", "DeleteTag", "", ""},
		{http.MethodDelete, "/v2/example/manifests/" + digest.String(), "DeleteManifest", "", ""},
		{http.MethodHead, "/v2/example/blobs/" + digest.String(), "ResolveBlob", "", ""},
		{http.MethodGet, "/v2/example/blobs/" + digest.String(), "ResolveBlob", "", ""},
		{http.MethodGet, "/v2/example/blobs/" + digest.String(), "GetBlob", "", ""},
		{http.MethodGet, "/v2/example/blobs/" + digest.String(), "GetBlobRange", "", "bytes=0-1"},
		{http.MethodDelete, "/v2/example/blobs/" + digest.String(), "DeleteBlob", "", ""},
		{http.MethodGet, "/v2/example/tags/list", "Tags", "", ""},
		{http.MethodGet, "/v2/_catalog", "Repositories", "", ""},
		{http.MethodGet, "/v2/example/referrers/" + digest.String(), "Referrers", "", ""},
		{http.MethodPost, "/v2/example/blobs/uploads/", "PushBlobChunked", "", ""},
		{http.MethodPost, "/v2/example/blobs/uploads/?digest=" + digest.String(), "PushBlobChunked", "blob", ""},
		{http.MethodGet, "/v2/example/blobs/uploads/session", "PushBlobChunkedResume", "", ""},
		{http.MethodDelete, "/v2/example/blobs/uploads/session", "PushBlobChunkedResume", "", ""},
		{http.MethodPatch, "/v2/example/blobs/uploads/session", "PushBlobChunkedResume", "blob", ""},
		{http.MethodPut, "/v2/example/blobs/uploads/session?digest=" + digest.String(), "PushBlobChunkedResume", "blob", ""},
		{http.MethodPost, "/v2/example/blobs/uploads/?mount=" + digest.String() + "&from=source", "MountBlob", "", ""},
	} {
		t.Run(test.method+" "+test.operation+" "+test.target, func(t *testing.T) {
			var calls, responses int
			authErr := &testAuthError{cause: oci.ErrNameUnknown, respond: func(w http.ResponseWriter, r *http.Request) {
				responses++
				http.Redirect(w, r, "/login", http.StatusSeeOther)
			}}
			backend := &oci.Funcs{NewError: func(_ context.Context, operation, repo string) error {
				calls++
				require.Equal(t, test.operation, operation)
				if test.operation == "Repositories" {
					require.Empty(t, repo)
				} else {
					require.Equal(t, "example", repo)
				}
				return fmt.Errorf("operation failed: %w", authErr)
			}}
			if test.operation == "PushBlobChunkedResume" {
				backend.PushBlobChunkedResume_ = func(ctx context.Context, repo, _ string, _ int64, _ int) (oci.BlobWriter, error) {
					return nil, backend.NewError(ctx, "PushBlobChunkedResume", repo)
				}
			}
			resolve := func(context.Context, string, oci.Digest) (oci.Descriptor, error) {
				return oci.Descriptor{Digest: digest, Size: 4, MediaType: oci.MediaTypeImageIndex}, nil
			}
			if test.operation == "GetBlob" || test.operation == "GetBlobRange" {
				backend.ResolveBlob_ = resolve
			}
			if test.operation == "GetManifest" {
				backend.ResolveManifest_ = resolve
				backend.ResolveTag_ = func(ctx context.Context, repo, _ string) (oci.Descriptor, error) {
					return resolve(ctx, repo, digest)
				}
			}
			srv, err := New(backend, nil)
			require.NoError(t, err)
			req := httptest.NewRequest(test.method, test.target, strings.NewReader(test.body))
			if test.byteRange != "" {
				req.Header.Set("Range", test.byteRange)
			}
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			require.Equal(t, http.StatusSeeOther, rec.Code, rec.Body.String())
			require.Equal(t, "/login", rec.Header().Get("Location"))
			require.Equal(t, 1, calls, "auth failures must not cause fallback operations")
			require.Equal(t, 1, responses)
			require.Empty(t, rec.Header().Get("Docker-Content-Digest"))
			require.Empty(t, rec.Header().Get("ETag"))
			require.Empty(t, rec.Header().Get("Content-Range"))
		})
	}
}

type authRedirectRegistry struct {
	oci.Registry
	err error
}

func (r *authRedirectRegistry) Redirect(*http.Request, oci.Descriptor, string) (bool, string, error) {
	return false, "", r.err
}

func TestServerBlobRedirectAuthError(t *testing.T) {
	digest := ocidigest.FromBytes([]byte("blob"))
	backend := &authRedirectRegistry{
		Registry: &oci.Funcs{ResolveBlob_: func(context.Context, string, oci.Digest) (oci.Descriptor, error) {
			return oci.Descriptor{Digest: digest, Size: 4}, nil
		}},
		err: &testAuthError{cause: oci.ErrDenied, respond: func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		}},
	}
	srv, err := New(backend, nil)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/example/blobs/"+digest.String(), nil))
	require.Equal(t, http.StatusSeeOther, rec.Code)
	require.Equal(t, "/login", rec.Header().Get("Location"))
}

func TestServerStreamingAuthErrorDoesNotRedirect(t *testing.T) {
	var responses int
	authErr := &testAuthError{cause: oci.ErrDenied, respond: func(w http.ResponseWriter, r *http.Request) {
		responses++
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}}
	digest := ocidigest.FromBytes([]byte("blobblob"))
	desc := oci.Descriptor{Digest: digest, Size: 8}
	backend := &oci.Funcs{
		ResolveBlob_: func(context.Context, string, oci.Digest) (oci.Descriptor, error) { return desc, nil },
		GetBlob_: func(context.Context, string, oci.Digest) (oci.BlobReader, error) {
			return &testBlobReader{desc: desc, ReadCloser: io.NopCloser(io.MultiReader(strings.NewReader("blob"), iotest.ErrReader(authErr)))}, nil
		},
	}
	srv, err := New(backend, nil)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/example/blobs/"+digest.String(), nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "blob", rec.Body.String())
	require.Zero(t, responses)
	require.Empty(t, rec.Header().Get("Location"))
}

func TestServerInvalidRepositoryNameReturnsOCIError(t *testing.T) {
	srv, err := New((*oci.Funcs)(nil), nil)
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/INVALID/manifests/latest", nil))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.JSONEq(t, `{
		"errors": [{
			"code": "NAME_INVALID",
			"message": "repository name is invalid: INVALID"
		}]
	}`, rec.Body.String())
}

func TestServerRejectsOverlongRepositoryName(t *testing.T) {
	srv, err := New((*oci.Funcs)(nil), nil)
	require.NoError(t, err)

	name := strings.Repeat("a", 256)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/"+name+"/tags/list", nil))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), `"code":"NAME_INVALID"`)
}
