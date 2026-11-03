package ociui

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ociref"
)

func parseDigestReference(s string) (ociref.Reference, error) {
	ref, err := ociref.ParseRelative(s)
	if err != nil {
		return ociref.Reference{}, err
	}
	if ref.Digest == "" {
		return ociref.Reference{}, fmt.Errorf("reference %q requires a digest", s)
	}
	return ref, nil
}

func (h *handler) fetchManifest(w http.ResponseWriter, r *http.Request, ref ociref.Reference) (*manifestData, error) {
	var err error
	var br oci.BlobReader
	if ref.Digest != "" {
		br, err = h.registry.GetManifest(r.Context(), ref.Repository, ref.Digest)
	} else {
		tag := ref.Tag
		if tag == "" {
			tag = "latest"
		}
		br, err = h.registry.GetTag(r.Context(), ref.Repository, tag)
	}
	if err != nil {
		return nil, err
	}
	// Registry middleware can provide appropriately scoped caching. Do not
	// reuse manifests globally across repositories or credential identities.
	return readManifest(br)
}

func (h *handler) listTags(w http.ResponseWriter, r *http.Request, repo string) (tags *tagPage, err error) {
	defer func() {
		if tags != nil {
			h.Lock()
			h.sawTags[repo] = tags.Tags
			h.Unlock()
		}
	}()

	qs := r.URL.Query()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	tags = &tagPage{Name: repo, Tags: []string{}}
	limit, err := listingLimit(r)
	if err != nil {
		return nil, err
	}
	params := &oci.TagsParameters{StartAfter: qs.Get("next")}
	for tag, err := range h.registry.Tags(ctx, repo, params) {
		if err != nil {
			if len(tags.Tags) == 0 {
				return nil, err
			}
			return tags, err
		}
		if limit > 0 && len(tags.Tags) == limit {
			tags.Next = tags.Tags[len(tags.Tags)-1]
			break
		}
		tags.Tags = append(tags.Tags, tag)
	}
	return tags, nil
}

func (h *handler) listCatalog(w http.ResponseWriter, r *http.Request) (*repositoryPage, error) {
	limit, err := listingLimit(r)
	if err != nil {
		return nil, err
	}
	out := &repositoryPage{Repos: []string{}}
	after := r.URL.Query().Get("next")
	for repo, err := range h.registry.Repositories(r.Context(), after) {
		if err != nil {
			return nil, err
		}
		if limit > 0 && len(out.Repos) == limit {
			out.Next = out.Repos[len(out.Repos)-1]
			break
		}
		out.Repos = append(out.Repos, repo)
	}
	return out, nil
}

// Fetch blob from the supplied registry.
func (h *handler) fetchBlob(w http.ResponseWriter, r *http.Request) (*sizeBlob, string, error) {
	ref, prefix, err := h.getDigest(w, r)
	if err != nil {
		return nil, "", err
	}
	rc, err := h.openBlob(w, r, ref)
	if err != nil {
		return nil, "", err
	}
	return &sizeBlob{rc: rc, size: blobSize(r, rc.Descriptor().Size)}, prefix, nil
}

func blobSize(r *http.Request, size int64) int64 {
	if qsize := r.URL.Query().Get("size"); qsize != "" {
		if sz, err := strconv.ParseInt(qsize, 10, 64); err != nil {
			log.Printf("invalid blob size %q: %v", qsize, err)
		} else if sz != 0 {
			size = sz
		}
	}
	return size
}

// getDigest separates a registry-relative digest reference from an archive path.
// The entire digest token is validated; its length and encoding belong to oci.
func (h *handler) getDigest(w http.ResponseWriter, r *http.Request) (ociref.Reference, string, error) {
	path, root, err := splitFsURL(r.URL.Path)
	if err != nil {
		return parseBlobPath(r.URL.Path)
	}
	cacheIndex := ""
	if root == "/cache/" {
		var ok bool
		cacheIndex, path, ok = strings.Cut(path, "/")
		if !ok {
			return ociref.Reference{}, "", fmt.Errorf("missing cache reference: %q", path)
		}
	}
	repo, rest, ok := strings.Cut(path, "@")
	if !ok {
		return ociref.Reference{}, "", fmt.Errorf("missing digest: %q", path)
	}
	digest, _, _ := strings.Cut(rest, "/")
	ref, err := parseDigestReference(repo + "@" + digest)
	if err != nil {
		return ociref.Reference{}, "", err
	}
	if root == "/cache/" {
		return ref, cacheIndex, nil
	}
	return ref, root + ref.String(), nil
}
