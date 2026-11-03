package ociui

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/ohseeeye/oci/pkg/mux"
	"github.com/ohseeeye/oci/pkg/ociref"
)

// localURL keeps registry links on this origin and escapes path/query data
// without escaping repository separators or the ':' and '@' in pull references.
func localURL(path string, query url.Values) string {
	return (&url.URL{Path: "/" + strings.TrimLeft(path, "/"), RawQuery: query.Encode()}).String()
}

func repoURL(repo string) string {
	if repo == "" {
		return "/"
	}
	return localURL(repo+"/tags/list", nil)
}
func imageURL(ref string) string {
	// A bare path is a repository landing page, not an implicit latest image.
	if ociref.IsValidRepository(ref) {
		ref += ":latest"
	}
	return localURL(ref, nil)
}
func blobURL(ref string) string {
	repo, digest, _ := strings.Cut(ref, "@")
	repo, _, _ = strings.Cut(repo, ":")
	return localURL(repo+"/blobs/"+digest, nil)
}
func referrersURL(repo, digest string) string {
	return localURL(repo+"/referrers/"+digest, nil)
}
func historyURL(ref ociref.Reference) string {
	tag := ref.Tag
	if tag == "" {
		tag = "latest"
	}
	return localURL(ref.Repository+"/_oci/tag-history/"+tag, nil)
}
func withQuery(target string, query url.Values) string {
	if len(query) == 0 {
		return target
	}
	return target + "?" + query.Encode()
}

// contentURL chooses the UI view for a descriptor, not the registry API URL.
func contentURL(mt, ref string) string {
	switch contentKindForMT(mt) {
	case "manifest":
		return imageURL(ref)
	case "fs":
		return localURL("fs/"+ref, nil)
	default:
		return blobURL(ref)
	}
}

func (h *handler) routes() http.Handler {
	router := mux.New()
	// The complete reference is one capture because template parameters must
	// occupy whole segments. ociref remains responsible for actual validation.
	router.RegisterConstraint("reference", `[^:@]+(?::[^/@:]+(?:@[^/]+)?|@[^/]+)`)
	// A repository cannot contain ':' or '@'. In particular, it must never
	// consume a digest and then reinterpret an archive path as another repo.
	router.RegisterConstraint("blobRepo", blobRepoExpression)
	router.RegisterConstraint("blobDigest", blobDigestExpression)
	page := func(pattern string, f HandleFuncE) {
		router.Get(pattern, h.errHandler(f))
		router.Head(pattern, h.errHandler(f))
	}
	page("/", func(w http.ResponseWriter, r *http.Request) error {
		return h.renderCatalog(w, r, "")
	})
	page("/_open", h.openReference)

	// Mounted namespaces own their paths, including misses.
	for _, route := range []struct {
		prefix string
		f      HandleFuncE
	}{
		{"fs", h.renderFS}, {"size", h.renderFat}, {"sizes", h.renderFats},
		{"layers", h.renderLayers}, {"cache", h.renderIndex},
		{"raw", h.serveRawBlob}, {"zurl", h.renderZurl},
	} {
		router.Route("/"+route.prefix, func(sub mux.Router) {
			sub.HandleFunc("/*path", h.errHandler(route.f))
		})
	}
	// Blob descendants belong to the blob, even if their names look like UI
	// resources (tags/list, blobs/<digest>, referrers/<digest>, etc.).
	page("/*blobRepo/blobs/:blobDigest", h.renderBlob)
	page("/*blobRepo/blobs/:blobDigest/", h.renderBlob)
	page("/*blobRepo/blobs/:blobDigest/*archive", h.renderBlob)

	page("/*repo/tags/list", func(w http.ResponseWriter, r *http.Request) error {
		return h.renderRepositoryPath(w, r, mux.URLParam(r, "repo"))
	})
	page("/*repo/referrers/:digest", func(w http.ResponseWriter, r *http.Request) error {
		ref, ok := digestPath(w, r)
		if !ok {
			return nil
		}
		return h.renderReferrers(w, r, ref.String())
	})
	page("/*repo/_oci/tag-history/:tag", func(w http.ResponseWriter, r *http.Request) error {
		ref, err := ociref.ParseRelative(mux.URLParam(r, "repo") + ":" + mux.URLParam(r, "tag"))
		if err != nil || ref.Digest != "" || ref.Tag != mux.URLParam(r, "tag") {
			http.Error(w, "invalid tag history reference", http.StatusBadRequest)
			return nil
		}
		return h.renderHistory(w, r, ref.String())
	})
	page("/*reference", func(w http.ResponseWriter, r *http.Request) error {
		ref, err := ociref.ParseRelative(mux.URLParam(r, "reference"))
		if err != nil {
			http.Error(w, "invalid image reference", http.StatusBadRequest)
			return nil
		}
		return h.renderManifest(w, r, ref.String())
	})
	// Keep this last: resource suffixes and pull references take precedence.
	page("/*repo", func(w http.ResponseWriter, r *http.Request) error {
		return h.renderRepositoryPath(w, r, mux.URLParam(r, "repo"))
	})
	return router
}

func (h *handler) renderRepositoryPath(w http.ResponseWriter, r *http.Request, repo string) error {
	if !ociref.IsValidRepository(repo) {
		http.Error(w, "invalid repository", http.StatusBadRequest)
		return nil
	}
	return h.renderRepo(w, r, repo)
}

func digestPath(w http.ResponseWriter, r *http.Request) (ociref.Reference, bool) {
	repo, digest := mux.URLParam(r, "repo"), mux.URLParam(r, "digest")
	ref, err := parseDigestReference(repo + "@" + digest)
	if err != nil || ref.Repository != repo || ref.Tag != "" {
		http.Error(w, "invalid digest reference", http.StatusBadRequest)
		return ociref.Reference{}, false
	}
	return ref, true
}

// openReference translates the catalog forms into canonical path navigation.
func (h *handler) openReference(w http.ResponseWriter, r *http.Request) error {
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	target := repoURL(repo)
	if image := strings.TrimSpace(r.URL.Query().Get("image")); image != "" {
		ref, err := ociref.ParseRelative(image)
		if err != nil {
			http.Error(w, "invalid image reference", http.StatusBadRequest)
			return nil
		}
		target = imageURL(ref.String())
	} else if repo != "" && !ociref.IsValidRepository(repo) {
		http.Error(w, "invalid repository", http.StatusBadRequest)
		return nil
	}
	http.Redirect(w, r, target, http.StatusFound)
	return nil
}
