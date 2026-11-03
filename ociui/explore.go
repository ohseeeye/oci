// Modified for dreg: local imports; see UPSTREAM.md.

package ociui

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitorus/timestamp"
	httpserve "github.com/ohseeeye/dreg/internal/explore/internal/forks/http"
	"github.com/ohseeeye/dreg/internal/explore/internal/gguf"
	rendering "github.com/ohseeeye/dreg/internal/explore/internal/render"
	"github.com/ohseeeye/dreg/internal/explore/internal/soci"
	"github.com/ohseeeye/dreg/internal/explore/internal/xxd"
	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ociref"
	"golang.org/x/sync/errgroup"
)

// We should not buffer blobs greater than 4MB
const (
	tooBig     = 1 << 22
	respTooBig = 1 << 25
)

//go:embed favicon.svg
var faviconSVG []byte

//go:embed robots.txt
var robotsTXT []byte

type handler struct {
	mux      http.Handler
	registry oci.Registry

	tocCache   cache
	indexCache cache

	sync.Mutex
	sawTags  map[string][]string
	inflight map[string]*soci.Indexer
}

// New serves one registry. All references are repository-relative; the caller
// owns backend construction, authentication, transport settings, and lifetime.
// New panics if registry is nil.
func New(registry oci.Registry) http.Handler {
	if registry == nil {
		panic("explore: registry is required")
	}
	h := handler{
		registry:   registry,
		sawTags:    map[string][]string{},
		inflight:   map[string]*soci.Indexer{},
		tocCache:   buildTocCache(),
		indexCache: buildIndexCache(),
	}

	h.mux = gzipHandler(h.routes())

	return &h
}

func splitFsURL(p string) (string, string, error) {
	for _, prefix := range []string{"/fs/", "/layers/", "/cache/", "/size/", "/sizes/", "/zurl/"} {
		if strings.HasPrefix(p, prefix) {
			return strings.TrimPrefix(p, prefix), prefix, nil
		}
	}

	return "", "", fmt.Errorf("unexpected path: %v", p)
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("%v", r.URL)
	start := time.Now()
	defer func() { log.Printf("%v (%s)", r.URL, time.Since(start)) }()

	if r.Header.Get("User-Agent") == "Go-http-client/1.1" {
		http.Error(w, "set a user-agent you coward", http.StatusForbidden)
		return
	}

	if r.URL.Path == "/favicon.svg" || r.URL.Path == "/favicon.ico" {
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("Content-Type", "image/svg+xml")
		http.ServeContent(w, r, "favicon.svg", time.Time{}, bytes.NewReader(faviconSVG))
		return
	}
	if r.URL.Path == "/robots.txt" {
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		http.ServeContent(w, r, "robots.txt", time.Time{}, bytes.NewReader(robotsTXT))
		return
	}

	h.mux.ServeHTTP(w, r)
}

type HandleFuncE func(http.ResponseWriter, *http.Request) error

func (h *handler) errHandler(hfe HandleFuncE) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := hfe(w, r); err != nil {
			log.Printf("%s: %v", r.URL.Path, err)
			fmt.Fprintf(w, "failed: %s", html.EscapeString(err.Error()))
			var serr *ErrorSuggestion
			if errors.As(err, &serr) {
				fmt.Fprintf(w, "<p><i>suggestion: %s</i></p>", serr.Suggestion())
			}
		}
	}
}

// ErrorSuggestion wraps an upstream error with a server-provided suggestion for the user.
type ErrorSuggestion struct {
	err     error
	suggest string
}

func NewErrorSuggestion(err error, suggest string) *ErrorSuggestion {
	return &ErrorSuggestion{
		err:     err,
		suggest: suggest,
	}
}

func (e *ErrorSuggestion) Error() string {
	return e.err.Error()
}

func (e *ErrorSuggestion) Unwrap() error {
	return e.err
}

func (e *ErrorSuggestion) Suggestion() string {
	return e.suggest
}

// Render repo with tags linking to images.
func (h *handler) renderRepo(w http.ResponseWriter, r *http.Request, repo string) error {
	if repo == "" {
		return h.renderCatalog(w, r, repo)
	}
	if !ociref.IsValidRepository(repo) {
		return fmt.Errorf("invalid repository %q", repo)
	}

	if err := headerTmpl.Execute(w, TitleData{repo}); err != nil {
		return err
	}
	header := HeaderData{
		Repo:      repo,
		Reference: repo,
		JQ:        crane("ls") + " " + repo,
	}
	if strings.Contains(repo, "/") {
		header.Up = &RepoParent{Parent: path.Dir(repo), Child: path.Base(repo), Separator: "/"}
	}
	if err := bodyTmpl.Execute(w, header); err != nil {
		return err
	}

	output := &jsonOutputter{
		w:     w,
		u:     r.URL,
		fresh: []bool{},
		repo:  repo,
	}

	tags, err := h.listTags(w, r, repo)
	if err != nil {
		if tags == nil {
			return NewErrorSuggestion(err, `try the <a href="/">catalog</a>?`)
		}

		// Sometimes we time out (or other issues), render whatever we got.
		fmt.Fprintf(w, "<p>returning partial response, saw error: %s</p>\n<hr>\n", err)
	}

	b, err := json.Marshal(tags)
	if err != nil {
		return err
	}
	if err := renderJSON(output, b); err != nil {
		return err
	}

	fmt.Fprintf(w, footer)
	return nil
}

func (h *handler) renderCatalog(w http.ResponseWriter, r *http.Request, repo string) error {
	if err := headerTmpl.Execute(w, TitleData{"Repositories"}); err != nil {
		return err
	}
	header := HeaderData{
		Repo:      repo,
		Reference: "Repositories",
		JQ:        "registry repositories",
	}
	if err := bodyTmpl.Execute(w, header); err != nil {
		return err
	}

	output := &jsonOutputter{
		w:     w,
		u:     r.URL,
		fresh: []bool{},
		repo:  repo,
	}

	w.Write([]byte(registryForms))
	v, err := h.listCatalog(w, r)
	if err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := renderJSON(output, b); err != nil {
		return err
	}

	fmt.Fprintf(w, footer)
	return nil
}

// Render manifests with links to blobs, manifests, etc.
func (h *handler) renderManifest(w http.ResponseWriter, r *http.Request, image string) error {
	ref, err := ociref.ParseRelative(image)
	if err != nil {
		return err
	}

	desc, err := h.fetchManifest(w, r, ref)
	if err != nil {
		return fmt.Errorf("fetchManifest: %w", err)
	}

	header := h.manifestHeader(ref, desc.Descriptor)

	u := *r.URL
	if ref.Digest != "" {
		// Allow this to be cached for an hour.
		w.Header().Set("Cache-Control", "max-age=3600, immutable")
	}
	// Keep tag-based view links on the tag URL. Links to configs and layers
	// still refer to this immutable manifest snapshot.
	immutableImage := ref.Repository + "@" + desc.Digest.String()

	if err := headerTmpl.Execute(w, TitleData{image}); err != nil {
		return fmt.Errorf("headerTmpl: %w", err)
	}

	output := &jsonOutputter{
		w:     w,
		u:     &u,
		fresh: []bool{},
		repo:  ref.Repository,
		mt:    desc.MediaType,
		image: immutableImage,
	}

	// Mutates header for bodyTmpl.
	b, err := h.jq(output, desc.Manifest, r, header)
	if err != nil {
		return fmt.Errorf("h.jq: %w", err)
	}

	if r.URL.Query().Get("render") == "x509" {
		if bytes.Count(b, []byte("-----BEGIN CERTIFICATE-----")) > 1 {
			header.JQ += " | while openssl x509 -text -noout 2>/dev/null; do :; done"
		} else {
			header.JQ += " | openssl x509 -text -noout"
		}
	} else if r.URL.Query().Get("render") == "history" {
		header.JQ = strings.TrimSuffix(header.JQ, " | jq .")
		header.JQ += ` | jq '.history[] | .v1Compatibility' -r | jq '.container_config.Cmd | join(" ")' -r | tac`
	}

	header.SizeLink = fmt.Sprintf("/sizes/%s?mt=%s&size=%d", ref.Repository+"@"+desc.Digest.String(), desc.MediaType, desc.Size)

	if err := bodyTmpl.Execute(w, header); err != nil {
		return fmt.Errorf("bodyTmpl: %w", err)
	}

	if err := h.renderContent(w, r, ref, b, output, u); err != nil {
		return err
	}

	fmt.Fprintf(w, footer)

	return nil
}

func (h *handler) tagHistory(r *http.Request, ref ociref.Reference) ([]byte, error) {
	capability, ok := h.registry.(oci.TagHistory)
	if !ok {
		return nil, oci.ErrUnsupported
	}
	if ref.Digest != "" {
		return nil, fmt.Errorf("history requires a tag reference")
	}
	tag := ref.Tag
	if tag == "" {
		tag = "latest"
	}
	params := &oci.TagHistoryParameters{}
	if n, err := listingLimit(r); err != nil {
		return nil, err
	} else if n > 0 {
		params.Limit = &n
	}
	for key, dest := range map[string]*time.Time{"before": &params.Before, "since": &params.Since} {
		if value := r.URL.Query().Get(key); value != "" {
			parsed, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return nil, err
			}
			*dest = parsed
		}
	}
	entries := []oci.Descriptor{}
	for desc, err := range capability.TagHistory(r.Context(), ref.Repository, tag, params) {
		if err != nil {
			return nil, err
		}
		entries = append(entries, desc)
	}
	b, err := json.Marshal(entries)
	if err != nil {
		return nil, err
	}
	if len(b) > tooBig {
		return nil, fmt.Errorf("tag history exceeds maximum size %d", tooBig)
	}
	return b, nil
}

// Render optional OCI tag history without assuming an HTTP host or vendor.
func (h *handler) renderHistory(w http.ResponseWriter, r *http.Request, image string) error {
	ref, err := ociref.ParseRelative(image)
	if err != nil {
		return err
	}

	th, err := h.tagHistory(r, ref)
	if err != nil {
		return err
	}

	header := HeaderData{
		Repo:      ref.Repository,
		Reference: ref.String(),
		JQ:        "tag history " + ref.String(),
	}

	if err := headerTmpl.Execute(w, TitleData{image}); err != nil {
		return fmt.Errorf("headerTmpl: %w", err)
	}

	copied := *r.URL
	output := &jsonOutputter{
		w:          w,
		u:          &copied,
		fresh:      []bool{},
		repo:       ref.Repository,
		mt:         r.URL.Query().Get("mt"),
		tagHistory: true,
	}

	// Mutates header for bodyTmpl.
	b, err := h.jq(output, th, r, &header)
	if err != nil {
		return fmt.Errorf("h.jq: %w", err)
	}

	if err := bodyTmpl.Execute(w, header); err != nil {
		return fmt.Errorf("bodyTmpl: %w", err)
	}

	if err := h.renderContent(w, r, ref, b, output, copied); err != nil {
		return err
	}

	fmt.Fprintf(w, footer)

	return nil
}

func (h *handler) renderReferrers(w http.ResponseWriter, r *http.Request, src string) error {
	ref, err := parseDigestReference(src)
	if err != nil {
		return err
	}

	desc, err := h.fetchReferrers(w, r, ref)
	if err != nil {
		return err
	}

	header := h.manifestHeader(ociref.Reference{Repository: ref.Repository, Digest: desc.Digest}, desc.Descriptor)
	header.JQ = fmt.Sprintf("referrers %s", ref.String())
	header.Referrers = false
	header.Subject = ref.Digest.String()

	if err := headerTmpl.Execute(w, TitleData{src}); err != nil {
		return fmt.Errorf("headerTmpl: %w", err)
	}

	u := *r.URL

	output := &jsonOutputter{
		w:     w,
		u:     &u,
		fresh: []bool{},
		repo:  ref.Repository,
		mt:    oci.MediaTypeImageIndex,
	}

	b := desc.Manifest

	b, err = h.jq(output, b, r, header)
	if err != nil {
		return fmt.Errorf("h.jq: %w", err)
	}

	if err := bodyTmpl.Execute(w, header); err != nil {
		return fmt.Errorf("bodyTmpl: %w", err)
	}

	if err := h.renderContent(w, r, ref, b, output, u); err != nil {
		return err
	}

	fmt.Fprintf(w, footer)

	return nil
}

// Render blob as JSON, possibly containing refs to images.
func (h *handler) renderBlobJSON(w http.ResponseWriter, r *http.Request, blobRef string) error {
	ref, err := parseDigestReference(blobRef)
	if err != nil {
		return fmt.Errorf("digest reference: %w", err)
	}

	blob, err := h.openBlob(w, r, ref)
	if err != nil {
		return fmt.Errorf("GetBlob: %w", err)
	}
	return h.renderBlobJSONContent(w, r, ref, blob)
}

// renderBlobJSONContent takes ownership of blob, including buffered sniff bytes.
func (h *handler) renderBlobJSONContent(w http.ResponseWriter, r *http.Request, ref ociref.Reference, blob oci.BlobReader) error {
	defer blob.Close()
	blobRef := ref.String()
	size := blob.Descriptor().Size
	// Allow this to be cached for an hour.
	w.Header().Set("Cache-Control", "max-age=3600, immutable")

	if err := headerTmpl.Execute(w, TitleData{blobRef}); err != nil {
		return fmt.Errorf("headerTmpl: %w", err)
	}

	hash := ref.Digest

	output := &jsonOutputter{
		w:     w,
		u:     r.URL,
		fresh: []bool{},
		repo:  ref.Repository,
		mt:    r.URL.Query().Get("mt"),
	}

	mediaType := "application/octet-stream"
	if output.mt != "" {
		mediaType = output.mt
	}

	desc := oci.Descriptor{
		Size:      size,
		Digest:    hash,
		MediaType: mediaType,
	}
	header := headerData(ref, desc)
	header.Up = &RepoParent{
		Parent:    ref.Repository,
		Separator: "@",
		Child:     ref.Digest.String(),
	}
	header.JQ = crane("blob") + " " + ref.String()

	if size > tooBig {
		header.JQ += fmt.Sprintf(" | head -c %d", httpserve.TooBig)
		if err := bodyTmpl.Execute(w, header); err != nil {
			return fmt.Errorf("bodyTmpl: %w", err)
		}
		dumb := rendering.NewEscapeWriter(w)
		if _, err := io.CopyN(dumb, blob, httpserve.TooBig); err != nil {
			return err
		}
		fmt.Fprintf(w, footer)

		return nil
	}

	// TODO: Can we do this in a streaming way?
	input, err := io.ReadAll(io.LimitReader(blob, tooBig))
	if err != nil {
		return err
	}

	// Mutates header for bodyTmpl.
	b, err := h.jq(output, input, r, header)
	if err != nil {
		return fmt.Errorf("h.jq: %w", err)
	}

	if r.URL.Query().Get("render") == "history" {
		header.JQ = strings.TrimSuffix(header.JQ, " | jq .")
		header.JQ += " | jq '.history[] | .created_by' -r"
	} else if r.URL.Query().Get("render") == "der" {
		header.JQ += " | openssl x509 -inform der -text -noout"
	} else if r.URL.Query().Get("render") == "xxd" {
		header.JQ += " | xxd"
	}

	if err := bodyTmpl.Execute(w, header); err != nil {
		return fmt.Errorf("bodyTmpl: %w", err)
	}

	if err := h.renderContent(w, r, ref, b, output, *r.URL); err != nil {
		if r.URL.Query().Get("render") == "xxd" {
			return fmt.Errorf("renderContent: %w", err)
		}

		r.URL.Query().Set("render", "xxd")
		fmt.Fprintf(w, "NOTE: failed to render: %v\n", err)
		if err := renderOctets(w, r, b); err != nil {
			return fmt.Errorf("renderContent fallback: %w", err)
		}
	}

	fmt.Fprintf(w, footer)

	return nil
}

func renderOctets(w http.ResponseWriter, r *http.Request, b []byte) error {
	fmt.Fprintf(w, "<pre>")
	if _, err := io.Copy(xxd.NewWriter(w, int64(len(b))), bytes.NewReader(b)); err != nil {
		return err
	}
	fmt.Fprintf(w, "</pre>")

	return nil
}

func (h *handler) renderContent(w http.ResponseWriter, r *http.Request, ref ociref.Reference, b []byte, output *jsonOutputter, u url.URL) error {
	switch r.URL.Query().Get("render") {
	case "raw":
		fmt.Fprintf(w, "<pre>")
		if _, err := w.Write(b); err != nil {
			return err
		}
		fmt.Fprintf(w, "</pre>")
	case "x509":
		return renderx509(w, b)
	case "cert":
		return renderCert(w, b, u)
	case "der":
		return renderDer(w, b)
	case "history":
		if isSchema1(r.URL.Query().Get("mt")) {
			return h.renderDockerfileSchema1(w, r, b, ref.Repository)
		} else {
			return h.renderDockerfile(w, r, ref, b)
		}
	case "created_by":
		fmt.Fprintf(w, "<pre>")
		if err := renderCreatedBy(w, b); err != nil {
			return err
		}
		fmt.Fprintf(w, "</pre>")
	case "xxd":
		return renderOctets(w, r, b)
	case "timestamp":
		ts, err := timestamp.Parse(b)
		if err != nil {
			return err
		}
		j, err := json.Marshal(ts)
		if err != nil {
			return err
		}
		return renderJSON(output, j)
	default:
		return renderJSON(output, b)
	}

	return nil
}

func (h *handler) renderGGUF(w http.ResponseWriter, r io.Reader) error {
	gr := gguf.NewGGUFFileReader(r)
	gf, err := gr.ReadGGUFFile()
	if err != nil {
		return err
	}

	fmt.Fprintf(w, "GGUF Version: %d\n", gf.Header.Version)
	for _, kv := range gf.MetadataKV {
		fmt.Fprintf(w, "<p>%s: %s</p>\n", kv.Key, kv.Value)
	}

	return nil
}

func (h *handler) renderDockerfile(w http.ResponseWriter, r *http.Request, ref ociref.Reference, b []byte) error {
	manifest := r.URL.Query().Get("manifest")
	if manifest == "" {
		return renderDockerfile(w, b, nil, ref.Repository)
	}

	dig, err := ociref.ParseRelative(manifest)
	if err != nil {
		return err
	}
	desc, err := h.fetchManifest(w, r, dig)
	if err != nil {
		return err
	}
	m := oci.IndexOrManifest{}
	if err := json.Unmarshal(desc.Manifest, &m); err != nil {
		return err
	}
	return renderDockerfile(w, b, &m, ref.Repository)
}

func (h *handler) renderFile(w http.ResponseWriter, r *http.Request, ref ociref.Reference, kind string, blob *sizeSeeker) error {
	mt := r.URL.Query().Get("mt")

	// Allow this to be cached for an hour.
	w.Header().Set("Cache-Control", "max-age=3600, immutable")

	httpserve.ServeContent(w, r, "", time.Time{}, blob, func(w http.ResponseWriter, r *http.Request, ctype string) error {
		// Kind at this point can be "gzip", "zstd" or ""
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := headerTmpl.Execute(w, TitleData{ref.String()}); err != nil {
			return err
		}
		hash := ref.Digest
		desc := oci.Descriptor{
			Digest:    hash,
			MediaType: mt,
		}
		if size := r.URL.Query().Get("size"); size != "" {
			if parsed, err := strconv.ParseInt(size, 10, 64); err == nil {
				desc.Size = parsed
			}
		}
		header := headerData(ref, desc)
		header.Up = &RepoParent{
			Parent:    ref.Repository,
			Separator: "@",
			Child:     ref.Digest.String(),
		}
		header.JQ = crane("blob") + " " + ref.String()
		if kind == "zstd" {
			header.JQ += " | zstd -d"
		} else if kind == "gzip" {
			header.JQ += " | gunzip"
		}

		if blob.size < 0 || blob.size > httpserve.TooBig {
			header.JQ += fmt.Sprintf(" | head -c %d", httpserve.TooBig)
		}
		log.Printf("ctype=%q", ctype)
		if !strings.HasPrefix(ctype, "text/") && !strings.Contains(ctype, "json") {
			header.JQ += " | xxd"
		}

		return bodyTmpl.Execute(w, header)
	})

	return nil
}

func (h *handler) renderFS(w http.ResponseWriter, r *http.Request) error {
	return h.renderFSWithBlob(w, r, nil)
}

// renderFSWithBlob takes ownership of an optional, already-sniffed blob.
func (h *handler) renderFSWithBlob(w http.ResponseWriter, r *http.Request, blob *sizeBlob) error {
	if blob != nil {
		defer blob.Close()
	}
	mt := r.URL.Query().Get("mt")

	dig, ref, err := h.getDigest(w, r)
	if err != nil {
		return fmt.Errorf("getDigest: %w", err)
	}

	isImage := strings.HasPrefix(mt, "image/")
	if isImage {
		return h.renderImage(w, r, dig, mt)
	}

	index, err := h.getIndex(r.Context(), dig.Digest.String())
	if err != nil {
		return fmt.Errorf("indexCache.Index(%q) = %w", dig.Digest.String(), err)
	}
	if index != nil {
		fs, err := h.indexedFS(w, r, dig, ref, index)
		if err != nil {
			return err
		}
		httpserve.FileServer(httpserve.FS(fs)).ServeHTTP(w, r)
		return nil
	}

	// Determine if this is actually a filesystem thing.
	if blob == nil {
		blob, ref, err = h.fetchBlob(w, r)
		if err != nil {
			return fmt.Errorf("fetchBlob: %w", err)
		}
		defer blob.Close()
	}

	if mt == "application/vnd.ollama.image.model" || mt == "application/vnd.docker.ai.gguf.v3" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := headerTmpl.Execute(w, TitleData{dig.String()}); err != nil {
			return err
		}
		hash := dig.Digest
		desc := oci.Descriptor{
			Digest:    hash,
			MediaType: mt,
		}
		if size := r.URL.Query().Get("size"); size != "" {
			if parsed, err := strconv.ParseInt(size, 10, 64); err == nil {
				desc.Size = parsed
			}
		}
		header := headerData(dig, desc)
		header.Up = &RepoParent{
			Parent:    dig.Repository,
			Separator: "@",
			Child:     dig.Digest.String(),
		}
		header.JQ = crane("blob") + " " + dig.String()
		if err := bodyTmpl.Execute(w, header); err != nil {
			return fmt.Errorf("bodyTmpl: %w", err)
		}
		if err := h.renderGGUF(w, blob); err != nil {
			return err
		}
		fmt.Fprintf(w, footer)
		return nil
	}

	kind, original, unwrapped, err := h.tryNewIndex(w, r, dig, ref, blob)
	if err != nil {
		return fmt.Errorf("failed to index blob %q: %w", dig.String(), err)
	}
	if unwrapped != nil {
		slog.Debug("unwrapped blob", "kind", kind)
		seek := &sizeSeeker{unwrapped, -1}
		return h.renderFile(w, r, dig, kind, seek)
	}
	if original != nil {
		slog.Debug("using original blob")
		seek := &sizeSeeker{original, blob.size}
		return h.renderFile(w, r, dig, kind, seek)
	}

	return nil
}

func (h *handler) renderFat(w http.ResponseWriter, r *http.Request) error {
	mt := r.URL.Query().Get("mt")
	dig, ref, err := h.getDigest(w, r)
	if err != nil {
		return fmt.Errorf("getDigest: %w", err)
	}

	index, err := h.getIndex(r.Context(), dig.Digest.String())
	if err != nil {
		return fmt.Errorf("indexCache.Index(%q) = %w", dig.Digest.String(), err)
	}

	if index == nil {
		// Determine if this is actually a filesystem thing.
		blob, _, err := h.fetchBlob(w, r)
		if err != nil {
			return fmt.Errorf("fetchBlob: %w", err)
		}

		index, err = h.createIndex(r.Context(), blob, blob.size, dig.Digest.String(), 0, mt)
		if err != nil {
			return fmt.Errorf("createIndex: %w", err)
		}
		if index == nil {
			// Non-indexable blobs are filtered later.
			return fmt.Errorf("not a filesystem")
		}
	}

	fs, err := h.indexedFS(w, r, dig, ref, index)
	if err != nil {
		return err
	}

	des, err := fs.Everything()
	if err != nil {
		return err
	}

	des = dropZeroSize(des)

	f := renderDirSize(w, r, index.TOC().Csize, dig, index.TOC().Type, mt, len(des))
	return httpserve.DirList(w, r, httpserve.FS(fs), ref, des, f)
}

func (h *handler) renderFats(w http.ResponseWriter, r *http.Request) error {
	dig, ref, err := h.getDigest(w, r)
	if err != nil {
		return err
	}

	desc, err := h.fetchManifest(w, r, dig)
	if err != nil {
		return err
	}

	var manifest oci.IndexOrManifest
	if err := json.Unmarshal(desc.Manifest, &manifest); err != nil {
		return err
	}
	if isIndex(desc.MediaType) || manifest.Manifests != nil {
		return h.renderSizeIndex(w, r, dig, desc, manifest.Manifests)
	}

	mfs, err := h.multiFS(w, r, dig, desc, ref)
	if err != nil {
		return err
	}

	des, err := mfs.Everything()
	if err != nil {
		return err
	}

	des = dropZeroSize(des)

	f := renderDirSize(w, r, desc.Size, dig, "tar", desc.MediaType, len(des))
	return httpserve.DirList(w, r, httpserve.FS(mfs), ref, des, f)
}

// dropZeroSize removes zero-byte entries from a directory listing. Used by
// the size-sorted views (/size/, /sizes/) where empty files, symlinks,
// hardlinks, and directories would just cluster at the bottom of the list
// without contributing useful information.
func dropZeroSize(des []fs.DirEntry) []fs.DirEntry {
	out := des[:0]
	for _, de := range des {
		fi, err := de.Info()
		if err == nil && fi.Size() == 0 {
			continue
		}
		out = append(out, de)
	}
	return out
}

func (h *handler) renderImage(w http.ResponseWriter, r *http.Request, ref ociref.Reference, mt string) error {
	imageURL := "/raw/" + ref.String() + "?mt=" + url.QueryEscape(mt)
	if err := headerTmpl.Execute(w, TitleData{ref.String()}); err != nil {
		return err
	}
	hash := ref.Digest
	desc := oci.Descriptor{
		Digest:    hash,
		MediaType: mt,
	}
	if size := r.URL.Query().Get("size"); size != "" {
		if parsed, err := strconv.ParseInt(size, 10, 64); err == nil {
			desc.Size = parsed
		}
	}
	header := headerData(ref, desc)
	header.Up = &RepoParent{
		Parent:    ref.Repository,
		Separator: "@",
		Child:     ref.Digest.String(),
	}
	header.JQ = "curl " + imageURL

	if err := bodyTmpl.Execute(w, header); err != nil {
		return err
	}

	fmt.Fprintf(w, "<img src=%q></img>", imageURL)
	fmt.Fprintf(w, "</body></html>")

	return nil
}

func (h *handler) indexedFS(w http.ResponseWriter, r *http.Request, dig ociref.Reference, ref string, index soci.Index) (*soci.SociFS, error) {
	toc := index.TOC()
	if toc == nil {
		return nil, fmt.Errorf("this should not happen")
	}
	mt := r.URL.Query().Get("mt")
	if mt == "" {
		mt = toc.MediaType
	}

	blob, err := h.registryBlob(w, r, dig)
	if err != nil {
		return nil, err
	}
	return soci.FS(index, blob, strings.TrimPrefix(ref, "/"), dig.String(), respTooBig, mt, renderHeader), nil
}

func (h *handler) multiFS(w http.ResponseWriter, r *http.Request, dig ociref.Reference, desc *manifestData, ref string) (*soci.MultiFS, error) {
	var m oci.IndexOrManifest
	if err := json.Unmarshal(desc.Manifest, &m); err != nil {
		return nil, err
	}

	fss := make([]*soci.SociFS, len(m.Layers))
	var g errgroup.Group
	for i, layer := range m.Layers {
		i := i
		size := layer.Size
		digest := layer.Digest
		layerRef := ociref.Reference{Repository: dig.Repository, Digest: layer.Digest}
		mediaType := layer.MediaType

		if digest.String() == emptyDigest {
			continue
		}

		g.Go(func() error {
			index, err := h.getIndex(r.Context(), digest.String())
			if err != nil {
				return fmt.Errorf("indexCache.Index(%q) = %w", dig.Digest.String(), err)
			}
			if index == nil {
				rc, err := h.openBlob(w, r, layerRef)
				if err != nil {
					return err
				}

				index, err = h.createIndex(r.Context(), rc, size, digest.String(), 0, mediaType)
				if err != nil {
					return fmt.Errorf("createIndex: %w", err)
				}
				if index == nil {
					// Non-indexable blobs are filtered later.
					return nil
				}
			}

			fs, err := h.createFs(w, r, ref, layerRef, index, mediaType)
			if err != nil {
				return err
			}
			// NOTE: reverses order
			fss[(len(m.Layers)-1)-i] = fs
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	prefix := strings.TrimPrefix(ref, "/")
	return soci.NewMultiFS(fss, prefix, dig.String(), desc.Size, desc.MediaType, renderDir), nil
}

// Flatten layers of an image and serve as a filesystem.
func (h *handler) renderLayers(w http.ResponseWriter, r *http.Request) error {
	dig, ref, err := h.getDigest(w, r)
	if err != nil {
		return err
	}

	desc, err := h.fetchManifest(w, r, dig)
	if err != nil {
		return err
	}

	mfs, err := h.multiFS(w, r, dig, desc, ref)
	if err != nil {
		return err
	}

	// Allow this to be cached for an hour.
	w.Header().Set("Cache-Control", "max-age=3600, immutable")

	httpserve.FileServer(httpserve.FS(mfs)).ServeHTTP(w, r)

	return nil
}

func (h *handler) renderIndex(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	idx := 0
	dig, idxs, err := h.getDigest(w, r)
	if err != nil {
		return fmt.Errorf("getDigest: %w", err)
	}
	prefix := fmt.Sprintf("/cache/%s/%s", idxs, dig.String())
	if parsed, err := strconv.ParseInt(idxs, 10, 64); err != nil {
		slog.Debug("parse index", "index", idxs)
	} else {
		idx = int(parsed)
	}
	key := indexKey(dig.Digest.String(), idx)
	size, err := h.indexCache.Size(ctx, key)
	if err != nil {
		return fmt.Errorf("indexCache.Size: %w", err)
	}
	rc, err := h.indexCache.Reader(ctx, key)
	if err != nil {
		return fmt.Errorf("indexCache.Reader: %w", err)
	}
	defer rc.Close()
	zr, err := gzip.NewReader(rc)
	if err != nil {
		return fmt.Errorf("gzip.NewReader: %w", err)
	}
	tr := tar.NewReader(zr)
	fs := h.newLayerFS(tr, size, prefix, dig.String(), "tar+gzip", "application/tar+gzip")

	// Allow this to be cached for an hour.
	w.Header().Set("Cache-Control", "max-age=3600, immutable")

	httpserve.FileServer(fs).ServeHTTP(w, r)
	return nil
}

func (h *handler) jq(output *jsonOutputter, b []byte, r *http.Request, header *HeaderData) ([]byte, error) {
	jq, ok := r.URL.Query()["jq"]
	if !ok {
		header.JQ += " | jq ."
		return b, nil
	}

	var (
		err error
		exp string
	)

	exps := []string{header.JQ}

	for _, j := range jq {
		if debug {
			log.Printf("j = %s", j)
		}
		b, exp, err = evalBytes(j, b)
		if err != nil {
			return nil, err
		}
		exps = append(exps, exp)
	}

	header.JQ = strings.Join(exps, " | ")
	return b, nil
}

func (h *handler) getTags(repo string) ([]string, bool) {
	h.Lock()
	tags, ok := h.sawTags[repo]
	h.Unlock()
	return tags, ok
}

func (h *handler) manifestHeader(ref ociref.Reference, desc oci.Descriptor) *HeaderData {
	header := headerData(ref, desc)
	header.JQ = crane("manifest") + " " + ref.String()
	header.Referrers = true
	if ref.Digest == "" {
		if _, supported := h.registry.(oci.TagHistory); supported {
			header.ReferenceURL = withQuery(historyURL(ref), url.Values{"mt": {desc.MediaType}})
		}
	}

	identifiers := strings.TrimPrefix(ref.String(), ref.Repository)

	// Handle clicking repo to list tags and such.
	if strings.Contains(identifiers, "@") && strings.Index(identifiers, "@") < strings.Index(identifiers, ":") {
		chunks := strings.SplitN(ref.String(), "@", 2)
		header.Up = &RepoParent{
			Parent:    ref.Repository,
			Child:     chunks[1],
			Separator: "@",
		}
	} else if strings.Contains(identifiers, ":") {
		chunks := strings.SplitN(identifiers, ":", 2)
		header.Up = &RepoParent{
			Parent:    ref.Repository,
			Child:     chunks[1],
			Separator: ":",
		}
	} else {
		header.Up = &RepoParent{
			Parent: ref.String(),
		}
	}

	// Opportunistically show referrers based on cosign scheme if we
	// have a cached tags list response.
	prefix := strings.Replace(desc.Digest.String(), ":", "-", 1)
	tags, ok := h.getTags(ref.Repository)
	if ok {
		for _, tag := range tags {
			if tag == prefix {
				// Referrers tag schema
				header.CosignTags = append(header.CosignTags, CosignTag{
					Tag:   tag,
					Short: "fallback",
				})
			} else if strings.HasPrefix(tag, prefix) {
				// Cosign tag schema
				chunks := strings.SplitN(tag, ".", 2)
				if len(chunks) == 2 && len(chunks[1]) != 0 {
					header.CosignTags = append(header.CosignTags, CosignTag{
						Tag:   tag,
						Short: chunks[1],
					})
				}
			}
		}
	}

	return header
}

func headerData(ref ociref.Reference, desc oci.Descriptor) *HeaderData {
	query := url.Values{"mt": {desc.MediaType}}
	digestQuery := url.Values{"mt": {desc.MediaType}, "size": {strconv.FormatInt(desc.Size, 10)}}
	hdr := &HeaderData{
		Repo:          ref.Repository,
		Reference:     ref.String(),
		CosignTags:    []CosignTag{},
		Descriptor:    &desc,
		ReferenceURL:  withQuery(contentURL(desc.MediaType, ref.String()), query),
		DigestURL:     withQuery(contentURL(desc.MediaType, ref.Repository+"@"+desc.Digest.String()), digestQuery),
		MediaTypeLink: getLink(desc.MediaType),
		HumanSize:     renderSize(desc.Size),
	}

	return hdr
}

func tarExtractFlags(kind string) string {
	if kind == "tar+gzip" {
		return "tar -Oxz "
	}
	if kind == "tar+zstd" {
		return "tar --zstd -Ox "
	}
	return "tar -Ox "
}

func tarListFlags(kind string) string {
	if kind == "tar+gzip" {
		return "tar -tvz"
	}
	if kind == "tar+zstd" {
		return "tar --zstd -tv"
	}
	return "tar -tv"
}

func tarFileLink(prefix, filename string) string {
	filelink := filename

	// Compute links for JQ
	fprefix := ""
	if strings.HasPrefix(filename, "./") {
		fprefix = "./"
	}
	filename = strings.TrimSuffix(filename, "/")
	dir := path.Dir(filename)
	if dir != "." {
		base := path.Base(filename)
		sep := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(filename, fprefix), dir), base)

		href := path.Join(prefix, dir)
		htext := fprefix + dir + sep

		slog.Debug("render path", "dir", dir, "separator", sep, "base", base, "href", href, "text", htext)
		dirlink := fmt.Sprintf(`<a class="mt" href="/%s">%s</a>`, href, htext)
		filelink = dirlink + base
	}
	return filelink
}

func fileCommand(r *http.Request, fname string, prefix string, refs string, kind string, f httpserve.File, ctype string) (string, error) {
	ref, err := ociref.ParseRelative(refs)
	if err != nil {
		return "", err
	}
	filename := strings.TrimPrefix(fname, "/"+prefix)
	filename = strings.TrimPrefix(filename, "/")

	var stat fs.FileInfo
	if f != nil {
		stat, err = f.Stat()
		if err != nil {
			return "", err
		}
		if tarh, ok := stat.Sys().(*tar.Header); ok {
			filename = tarh.Name
		} else if !stat.IsDir() {
			slog.Debug("not a tar header or directory")
		}
	}

	filelink := tarFileLink(prefix, filename)
	cmd := crane("blob") + " " + ref.String() + " | " + tarExtractFlags(kind) + " " + filelink

	if r.URL.Query().Get("render") == "elf" {
		cmd += " | objdump -x -"
	} else if stat != nil {
		if stat.Size() > httpserve.TooBig {
			cmd += fmt.Sprintf(" | head -c %d", httpserve.TooBig)
		}
		if ctype == "application/octet-stream" || ctype == "elf" {
			cmd += " | xxd"
		}
	}

	if stat != nil && stat.IsDir() {
		q := r.URL.Query()
		if q.Get("all") == "true" {
			q.Del("all")
		} else {
			q.Set("all", "true")
		}
		tarhref := r.URL.Path
		if enc := q.Encode(); enc != "" {
			tarhref += "?" + enc
		}
		tarlink := fmt.Sprintf("<a class=%q href=%q>%s</a>", "mt", tarhref, tarListFlags(kind))

		tarSuffix := filelink
		if filter := tarFilterArgs(r, ref.String()); filter != "" {
			tarSuffix = filter
		}

		cmd = crane("blob") + " " + ref.String() + " | " + tarlink + " " + tarSuffix
	}
	return cmd, nil
}

func renderBeforeOpen(w http.ResponseWriter, r *http.Request, fname string, prefix string, refs string, kind string, mediaType string, size int64) error {
	ref, header, err := layerHeader(refs, mediaType, size)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := headerTmpl.Execute(w, TitleData{ref.String()}); err != nil {
		return err
	}

	cmd, err := fileCommand(r, fname, prefix, refs, kind, nil, "")
	if err != nil {
		return err
	}
	if err := bodyTmpl.Execute(w, header); err != nil {
		return err
	}
	writeLoadingShell(w, cmd)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func layerHeader(refs string, mediaType string, size int64) (ociref.Reference, *HeaderData, error) {
	ref, err := parseDigestReference(refs)
	if err != nil {
		return ociref.Reference{}, nil, err
	}
	desc := oci.Descriptor{Size: size, Digest: ref.Digest, MediaType: mediaType}
	header := headerData(ref, desc)
	header.Up = &RepoParent{Parent: ref.Repository, Separator: "@", Child: ref.Digest.String()}
	header.SizeLink = fmt.Sprintf("/size/%s?mt=%s&size=%d", ref.String(), mediaType, size)
	return ref, header, nil
}

func writeLoadingShell(w io.Writer, cmd string) {
	fmt.Fprintf(w, `<div><template shadowrootmode="open"><style>
@keyframes spin {
  to {
    transform: rotate(360deg);
  }
}

@keyframes twist-up {
  to {
    transform: rotateX(360deg);
  }
}
#file {
  display: block;
  font-family: monospace;
  white-space: pre;
}
#file::slotted(*) {
  font-family: monospace;
  white-space: pre;
}
#file::slotted(pre) {
  margin: 0;
}</style><h4><span style="padding:0;" class="noselect">$ </span><slot name="cmd">%s</slot></h4><p id="loading"><slot name="message"><span style="line-height: .707em; width: .707em; display: inline-block; animation: spin 1.0s infinite linear;">🤐</span> Loading<span><slot name="progress"></slot></span></slot></p><slot id="file" name="file"></slot></template>`, cmd)
}

func renderHeader(w http.ResponseWriter, r *http.Request, fname string, prefix string, refs string, kind string, mediaType string, size int64, f httpserve.File, ctype string) error {
	log.Printf("ctype=%q", ctype)
	ref, header, err := layerHeader(refs, mediaType, size)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := headerTmpl.Execute(w, TitleData{ref.String()}); err != nil {
		return err
	}

	cmd, err := fileCommand(r, fname, prefix, refs, kind, f, ctype)
	if err != nil {
		return err
	}
	loading, _ := f.(interface {
		Loading() bool
		StartLoading()
	})
	if loading != nil && loading.Loading() {
		header.JQ = ""
	} else {
		header.JQ = cmd
	}

	if err := bodyTmpl.Execute(w, header); err != nil {
		return err
	}

	if loading != nil && loading.Loading() {
		writeLoadingShell(w, cmd)
		loading.StartLoading()
	}

	return nil
}

// tarFilterArgs returns the suffix to append after `tar -tv` so the displayed
// shell command actually filters to what the page is showing. Two flavors:
//
//   - ?env=PATH (or another env name) returns a $(crane config ... | jq ...)
//     subshell that extracts the first matching env var, splits on ":", strips
//     leading slashes, and joins with spaces — matching the manual incantation
//     in https://github.com/jonjohnsonjr/dagdotdev/issues/112.
//   - ?path=... returns the literal paths space-joined.
//
// Returns "" when no filter is in play.
func tarFilterArgs(r *http.Request, ref string) string {
	if env := r.URL.Query().Get("env"); env != "" && isEnvName(env) {
		return fmt.Sprintf(`$(crane config %s | jq -r 'first(.config.Env[] | select(startswith("%s=")) | ltrimstr("%s=") | split(":") | map(ltrimstr("/")) | join(" "))')`, ref, env, env)
	}
	var parts []string
	for _, p := range r.URL.Query()["path"] {
		for _, sub := range strings.Split(p, ":") {
			sub = strings.Trim(sub, "/")
			if sub != "" {
				parts = append(parts, sub)
			}
		}
	}
	return strings.Join(parts, " ")
}

func isEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		if c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
			continue
		}
		if i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
}

func loadingBarSize(ref string) int {
	loading := "Loading"
	tarflags := "tar -tvz"

	s := "crane blob " + ref + " | " + tarflags
	log.Printf("s: %q", s)
	return len(s) - len(loading)
}

func renderDir(w http.ResponseWriter, r *http.Request, fname string, prefix string, mediaType string, size int64, refs string, f httpserve.File, ctype string) error {
	ref, err := ociref.ParseRelative(refs)
	if err != nil {
		return err
	}

	// This must be a directory because it wasn't part of a filesystem
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	if !stat.IsDir() {
		return fmt.Errorf("file was not a directory")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := headerTmpl.Execute(w, TitleData{ref.String()}); err != nil {
		return err
	}

	filename := strings.TrimPrefix(fname, "/"+prefix)
	filename = strings.TrimPrefix(filename, "/")

	sys := stat.Sys()
	tarh, ok := sys.(*tar.Header)
	if ok {
		filename = tarh.Name
	} else {
		slog.Debug("file metadata", "type", fmt.Sprintf("%T", sys))
	}

	tarflags := "tar -tv"

	hash := ref.Digest

	desc := oci.Descriptor{
		Size:      size,
		Digest:    hash,
		MediaType: mediaType,
	}
	header := headerData(ref, desc)

	header.Up = &RepoParent{
		Parent:    ref.Repository,
		Separator: "@",
		Child:     ref.Digest.String(),
	}

	q := r.URL.Query()
	if q.Get("all") == "true" {
		q.Del("all")
	} else {
		q.Set("all", "true")
	}
	tarhref := r.URL.Path
	if enc := q.Encode(); enc != "" {
		tarhref += "?" + enc
	}
	tarlink := fmt.Sprintf("<a class=%q href=%q>%s</a>", "mt", tarhref, tarflags)

	tarSuffix := filename
	if filter := tarFilterArgs(r, ref.String()); filter != "" {
		tarSuffix = filter
	}

	// TODO: Make filename clickable to go up a directory.
	header.JQ = crane("export") + " " + ref.String() + " | " + tarlink + " " + tarSuffix

	header.SizeLink = fmt.Sprintf("/sizes/%s?mt=%s&size=%d", ref.Repository+"@"+hash.String(), mediaType, size)

	return bodyTmpl.Execute(w, header)
}

func renderDirSize(w http.ResponseWriter, r *http.Request, size int64, ref ociref.Reference, kind string, mediaType string, num int) func() error {
	return func() error {
		// This must be a directory because it wasn't part of a filesystem
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := headerTmpl.Execute(w, TitleData{ref.String()}); err != nil {
			return err
		}

		hash := ref.Digest

		desc := oci.Descriptor{
			Size:      size,
			Digest:    hash,
			MediaType: mediaType,
		}
		header := headerData(ref, desc)

		header.Up = &RepoParent{
			Parent:    ref.Repository,
			Separator: "@",
			Child:     ref.Digest.String(),
		}

		tarflags := "tar -tv"
		if kind == "tar+gzip" {
			tarflags = "tar -tvz"
		} else if kind == "tar+zstd" {
			tarflags = "tar --zstd -tv"
		}

		if filter := tarFilterArgs(r, ref.String()); filter != "" {
			tarflags += " " + filter
		}

		ua := r.UserAgent()
		if strings.Contains(ua, "BSD") || strings.Contains(ua, "Mac") {
			tarflags += " | sort -n -r -k5"
		} else {
			tarflags += " | sort -n -r -k3"
		}

		if isImage(mediaType) {
			header.JQ = crane("export") + " " + ref.String() + " | " + tarflags
		} else {
			header.JQ = crane("blob") + " " + ref.String() + " | " + tarflags
		}

		if num > httpserve.TooBig {
			header.JQ += fmt.Sprintf(" | head -c %d", httpserve.TooBig)
		}

		return bodyTmpl.Execute(w, header)
	}
}

func (h *handler) renderZurl(w http.ResponseWriter, r *http.Request) error {
	dig, ref, err := h.getDigest(w, r)
	if err != nil {
		return err
	}

	filename := strings.TrimPrefix(r.URL.Path, ref)
	filename = strings.TrimPrefix(filename, "/")

	desc, err := h.fetchManifest(w, r, dig)
	if err != nil {
		return err
	}

	var m oci.IndexOrManifest
	if err := json.Unmarshal(desc.Manifest, &m); err != nil {
		return err
	}

	layers := m.Layers
	for i := len(layers) - 1; i >= 0; i-- {
		layer := layers[i]
		digest := layer.Digest
		index, err := h.getIndex(r.Context(), digest.String())
		if err != nil {
			return fmt.Errorf("indexCache.Index(%q) = %w", dig.Digest.String(), err)
		}
		if index == nil {
			layerRef := ociref.Reference{Repository: dig.Repository, Digest: layer.Digest}
			rc, err := h.openBlob(w, r, layerRef)
			if err != nil {
				return err
			}

			index, err = h.createIndex(r.Context(), rc, layer.Size, digest.String(), 0, layer.MediaType)
			if err != nil {
				return fmt.Errorf("createIndex: %w", err)
			}
			if index == nil {
				slog.Debug("index was nil")
				// Non-indexable blobs are filtered later.
				continue
			}
		}

		toc := index.TOC()
		if toc == nil {
			continue
		}

		tf, err := index.Locate(filename)
		if err != nil {
			return err
		}

		cp := toc.Checkpoint(tf)

		dict, err := index.Dict(cp)
		if err != nil {
			return err
		}

		from := cp.Checkpoint
		from.SetHistory(dict)

		b, err := json.Marshal(from)
		if err != nil {
			return err
		}

		fmt.Fprintf(w, "zurl cat %s --start %d --end %d --checkpoint %q", dig.Repository+"@"+digest.String(), cp.File.Offset, cp.File.Offset+cp.File.Size, string(b))
		return nil
	}

	w.WriteHeader(http.StatusNotFound)
	return fmt.Errorf("could not find %s", filename)
}
