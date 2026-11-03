package ociui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/ohseeeye/oci/pkg/ociref"
)

// manifestData retains the original manifest bytes alongside the backend's
// descriptor. Rendering must not re-encode signed or content-addressed data.
type manifestData struct {
	oci.Descriptor
	Manifest []byte
}
type tagPage struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
	Next string   `json:"next,omitempty"`
}
type repositoryPage struct {
	Repos []string `json:"repositories"`
	Next  string   `json:"next,omitempty"`
}

func readManifest(br oci.BlobReader) (*manifestData, error) {
	defer br.Close()
	if br.Descriptor().Size > tooBig {
		return nil, fmt.Errorf("manifest exceeds maximum size %d", tooBig)
	}
	b, err := io.ReadAll(io.LimitReader(br, tooBig+1))
	if err != nil {
		return nil, err
	}
	if len(b) > tooBig {
		return nil, fmt.Errorf("manifest exceeds maximum size %d", tooBig)
	}
	return &manifestData{Descriptor: br.Descriptor(), Manifest: b}, nil
}

func (h *handler) openBlob(w http.ResponseWriter, r *http.Request, ref ociref.Reference) (oci.BlobReader, error) {
	return h.registry.GetBlob(r.Context(), ref.Repository, ref.Digest)
}

type registryRangeBlob struct {
	registry oci.Registry
	repo     string
	digest   oci.Digest
}

func (b *registryRangeBlob) Reader(ctx context.Context, off, end int64) (io.ReadCloser, error) {
	return b.registry.GetBlobRange(ctx, b.repo, b.digest, off, end)
}
func (h *handler) registryBlob(w http.ResponseWriter, r *http.Request, ref ociref.Reference) (*registryRangeBlob, error) {
	return &registryRangeBlob{registry: h.registry, repo: ref.Repository, digest: ref.Digest}, nil
}

// Pagination uses a name-based start-after cursor, not an upstream URL. Consume
// one extra item so the UI offers a next page only when another item exists.
func listingLimit(r *http.Request) (int, error) {
	if r.URL.Query().Get("n") == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(r.URL.Query().Get("n"))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("n must be a positive integer")
	}
	return n, nil
}
func (h *handler) fetchReferrers(w http.ResponseWriter, r *http.Request, ref ociref.Reference) (*manifestData, error) {
	index := oci.IndexOrManifest{SchemaVersion: 2, MediaType: oci.MediaTypeImageIndex, Manifests: []oci.Descriptor{}}
	for desc, err := range h.registry.Referrers(r.Context(), ref.Repository, ref.Digest, nil) {
		if err != nil {
			return nil, err
		}
		index.Manifests = append(index.Manifests, desc)
	}
	b, err := json.Marshal(index)
	if err != nil {
		return nil, err
	}
	if len(b) > tooBig {
		return nil, fmt.Errorf("referrers index exceeds maximum size %d", tooBig)
	}
	d := oci.Descriptor{MediaType: oci.MediaTypeImageIndex, Size: int64(len(b)), Digest: ocidigest.FromBytes(b)}
	return &manifestData{Descriptor: d, Manifest: b}, nil
}
