package ociserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/mux"
	"github.com/ohseeeye/oci/pkg/ociref"
)

// ServerConfig configures a [Server].
type ServerConfig struct {
	// Logger receives diagnostics for unexpected internal failures. A nil
	// Logger disables logging.
	Logger *slog.Logger

	// Middlewares are applied to all routes on the server's root mux, including
	// routes added through [Server.Mux]. They are applied in the order provided;
	// the first middleware is the outermost middleware.
	Middlewares []func(http.Handler) http.Handler
}

// Server contains everything necessary to run the API portion of the service
type Server struct {
	cfg      ServerConfig
	mux      *mux.Mux
	db       oci.Registry
	redirect Redirecter
}

var _ http.Handler = (*Server)(nil)

// New returns a new server backed by pers. A nil cfg is equivalent to a
// pointer to a zero [ServerConfig]. The configuration is copied before New
// returns and may be modified by the caller afterward. When pers implements
// [Authorizer], its middleware is applied to the built-in OCI routes.
func New(pers oci.Registry, cfg0 *ServerConfig) (*Server, error) {
	var cfg ServerConfig
	if cfg0 != nil {
		cfg = *cfg0
		cfg.Middlewares = slices.Clone(cfg0.Middlewares)
	}
	s := &Server{
		cfg: cfg,
		db:  pers,
	}
	if redirecter, ok := pers.(Redirecter); ok {
		s.redirect = redirecter
	}
	s.addRoutes()
	return s, nil
}

// ServeHTTP implements [http.Handler].
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// Mux returns the server's root router. Callers may use it to add routes outside
// the built-in /v2 namespace, which is owned by a mounted child router. Root
// middleware must be configured through [ServerConfig.Middlewares] because the
// router does not allow middleware to be added after routes are registered.
func (s *Server) Mux() *mux.Mux {
	return s.mux
}

func (s *Server) logError(ctx context.Context, message string, err error, args ...any) {
	if s.cfg.Logger == nil {
		return
	}
	safeArgs := make([]any, len(args))
	copy(safeArgs, args)
	for i, arg := range safeArgs {
		if str, ok := arg.(string); ok {
			safeArgs[i] = sanitizeLogInput(str)
		}
	}
	s.cfg.Logger.ErrorContext(ctx, message, append(safeArgs, "error", err)...)
}

// sanitizeLogInput replaces newline characters to prevent log injection
func sanitizeLogInput(input string) string {
	escaped := strings.NewReplacer("\n", "\\n", "\r", "\\r")
	return escaped.Replace(input)
}

func (s *Server) addRoutes() {
	r := mux.New()
	if len(s.cfg.Middlewares) > 0 {
		r.Use(s.cfg.Middlewares...)
	}

	r.Route("/v2", func(r mux.Router) {
		if authorizer, ok := s.db.(Authorizer); ok {
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					reference := mux.URLParam(r, "reference")
					if reference == "" {
						reference = mux.URLParam(r, "digest")
					}
					if reference == "" {
						reference = mux.URLParam(r, "tag")
					}
					authorizer.Authorize(w, r, mux.URLParam(r, "name"), reference, next)
				})
			})
		}
		r.Use(validateRepositoryName)

		r.Handle("/", s.root())
		r.Get("/_catalog", s.catalogGet())

		// blob uploads
		r.Post("/*name/blobs/uploads/", s.blobUploadPost())
		r.Get("/*name/blobs/uploads/:session", s.blobUploadGet())
		r.Patch("/*name/blobs/uploads/:session", s.blobUploadPatch())
		r.Put("/*name/blobs/uploads/:session", s.blobUploadPut())
		r.Delete("/*name/blobs/uploads/:session", s.blobUploadDelete())

		// blobs
		r.Head("/*name/blobs/:digest", s.blobHeadGet())
		r.Get("/*name/blobs/:digest", s.blobHeadGet())
		r.Delete("/*name/blobs/:digest", s.blobDelete())

		// manifests
		r.Head("/*name/manifests/:reference", s.manifestHeadGet())
		r.Get("/*name/manifests/:reference", s.manifestHeadGet())
		r.Put("/*name/manifests/:reference", s.manifestPut())
		r.Delete("/*name/manifests/:reference", s.manifestDelete())

		// tags
		r.Get("/*name/tags/list", s.tagsGet())

		// referrers
		r.Get("/*name/referrers/:digest", s.referrersGet())

		// Experimental endpoints are registered only when the backend supports them.
		if history, ok := s.db.(oci.TagHistory); ok {
			r.Get("/*name/_oci/tag-history/:tag", s.tagHistoryGet(history))
		}
	})
	s.mux = r
}

func validateRepositoryName(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := mux.URLParam(r, "name")
		if name != "" && !ociref.IsValidRepository(name) {
			returnError(w, ErrNameInvalid(name))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) root() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}
}

func returnError(w http.ResponseWriter, oe *OCIError) {
	b, err := json.Marshal(map[string]any{
		"errors": []*OCIError{oe},
	})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if oe.status == http.StatusRequestedRangeNotSatisfiable {
		w.Header().Set("Content-Range", "bytes */*")
	}
	w.WriteHeader(oe.status)
	_, _ = w.Write(b)
}

// Authorizer provides registry-owned HTTP authentication and authorization.
// New detects this optional interface on the supplied registry and applies it
// to every built-in OCI route, including /v2/ and optional extension routes.
// ServerConfig.Middlewares run outside it; custom routes added through Server.Mux
// do not receive it.
type Authorizer interface {
	// Authorize handles authentication and authorization for a matched request.
	// repo is the repository name extracted from the path, or empty for /v2/
	// discovery and /v2/_catalog. Catalog requests also have an empty reference.
	// reference is the manifest tag/digest, blob/referrer digest, or history tag
	// extracted from the path. Listings and upload sessions have no reference;
	// upload IDs and query parameters are not treated as references.
	//
	// These are routed path values, not resolved descriptors. Name, reference,
	// and content validation happen afterward in the OCI handlers. Authorize may
	// attach an identity using r.WithContext and call next.ServeHTTP to continue.
	// Otherwise it must write the challenge, denial, or login redirect itself.
	// It runs once per matched request and must support concurrent requests.
	Authorize(w http.ResponseWriter, r *http.Request, repo, reference string, next http.Handler)
}

// Redirecter provides storage-backed URL generation for blob redirects.
type Redirecter interface {
	// Redirect returns whether the request should redirect, the redirect URL, and
	// any error encountered while deciding.
	Redirect(r *http.Request, desc oci.Descriptor, repo string) (bool, string, error)
}
