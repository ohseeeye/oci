// Modified for dreg: local imports; see UPSTREAM.md.

package ociui

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/ohseeeye/dreg/internal/explore/internal/forks/github.com/klauspost/compress/zstd"
	httpserve "github.com/ohseeeye/dreg/internal/explore/internal/forks/http"
	"github.com/ohseeeye/dreg/internal/explore/internal/soci"
	"github.com/ohseeeye/oci/pkg/ociref"
)

// 5 MB.
const (
	threshold = (1 << 20) * 5
	spanSize  = 1 << 22
)

func indexKey(prefix string, idx int) string {
	return fmt.Sprintf("%s.%d", prefix, idx)
}

// Attempt to create a new index. If we fail, both readclosers will be nil.
// TODO: Dedupe with createIndex.
func (h *handler) tryNewIndex(w http.ResponseWriter, r *http.Request, dig ociref.Reference, ref string, blob *sizeBlob) (string, io.ReadCloser, io.ReadCloser, error) {
	key := indexKey(dig.Digest.String(), 0)

	mt := r.URL.Query().Get("mt")

	var (
		tr      tarReader
		indexer *soci.Indexer
		kind    string
		cw      io.WriteCloser
	)

	h.Lock()
	idx, inflight := h.inflight[key]
	h.Unlock()

	if inflight {
		slog.Debug("archive index already in progress", "key", key)
		kind = idx.Type()
		switch kind {
		case "tar+gzip":
			zr, err := gzip.NewReader(blob)
			if err != nil {
				return "", nil, nil, fmt.Errorf("gzip.NewReader: %w", err)
			}
			tr = tar.NewReader(zr)
		case "tar+zstd":
			zr, err := zstd.NewReader(blob)
			if err != nil {
				return "", nil, nil, fmt.Errorf("zstd.NewReader: %w", err)
			}
			tr = tar.NewReader(zr)
		case "tar":
			tr = tar.NewReader(blob)
		}
	} else {
		// TODO: Plumb this down into NewIndexer so we don't create it until we need to.
		var err error
		cw, err = h.indexCache.Writer(r.Context(), key)
		if err != nil {
			return "", nil, nil, fmt.Errorf("indexCache.Writer: %w", err)
		}

		idx, _, pr, tpr, err := soci.NewIndexer(blob, cw, spanSize, mt)
		if idx == nil {
			if err := cw.Close(); err != nil {
				slog.Debug("close cache writer failed", "key", key, "error", err)
			}
			slog.Debug("archive indexer unavailable")
			return kind, pr, tpr, err
		}
		indexer = idx
		tr = idx
		kind = idx.Type()

		h.Lock()
		h.inflight[key] = indexer
		h.Unlock()

		defer func() {
			h.Lock()
			delete(h.inflight, key)
			h.Unlock()
		}()
	}

	// Render FS the old way while generating the index.
	fs := h.newLayerFS(tr, blob.size, ref, dig.String(), kind, mt)

	if !inflight {
		blob.w = w
		blob.total = loadingBarSize(dig.String())
		fs.progress = blob
	}

	httpserve.FileServer(fs).ServeHTTP(w, r)

	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	if indexer != nil {
		for {
			// Make sure we hit the end.
			_, err := indexer.Next()
			if errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				if err := cw.Close(); err != nil {
					slog.Debug("close cache writer failed", "key", key, "error", err)
				}
				return "", nil, nil, fmt.Errorf("indexer.Next: %w", err)
			}
		}

		toc, err := indexer.TOC()
		if err != nil {
			if err := cw.Close(); err != nil {
				slog.Debug("close cache writer failed", "key", key, "error", err)
			}
			return kind, nil, nil, fmt.Errorf("TOC(): %w", err)
		}

		if err := cw.Close(); err != nil {
			return kind, nil, nil, fmt.Errorf("cw.Close: %w", err)
		}
		if h.tocCache != nil {
			if err := h.tocCache.Put(r.Context(), key, toc); err != nil {
				slog.Debug("store archive TOC failed", "key", key, "error", err)
			}
		}

		slog.Debug("archive indexed", "size", indexer.Size())
	}

	return kind, nil, nil, nil
}

// Returns nil index if it's incomplete.
func (h *handler) getIndex(ctx context.Context, prefix string) (soci.Index, error) {
	if h.indexCache == nil {
		return nil, nil
	}
	index, err := h.getIndexN(ctx, prefix, 0)
	if errors.Is(err, io.EOF) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// TODO: Remove the need for this.
	if index.TOC() == nil {
		return nil, nil
	}

	return index, nil
}

func (h *handler) getIndexN(ctx context.Context, prefix string, idx int) (index soci.Index, err error) {
	key := indexKey(prefix, idx)
	bs := &cacheSeeker{h.indexCache, key}

	var (
		toc  *soci.TOC
		size int64
	)
	// Avoid calling cache.Size if we can.
	if h.tocCache != nil {
		toc, err = h.tocCache.Get(ctx, key)
		if err != nil {
			slog.Debug("get archive TOC failed", "key", key, "error", err)
			defer func() {
				if err == nil {
					if err := h.tocCache.Put(ctx, key, index.TOC()); err != nil {
						slog.Debug("backfill archive TOC failed", "key", key, "error", err)
					}
				}
			}()
		} else {
			size = toc.Size
			slog.Debug("archive TOC cache hit", "key", key)
		}
	}

	// Handle in-memory index under a certain size.
	if size == 0 {
		size, err = h.indexCache.Size(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("indexCache.Size: %w", err)
		}
	}
	if size <= threshold {
		return soci.NewIndex(bs, toc, nil)
	}

	// Index is too big to hold in memory, fetch or create an index of the index.
	sub, err := h.getIndexN(ctx, prefix, idx+1)
	if err != nil {
		slog.Debug("get nested archive index failed", "prefix", prefix, "index", idx+1, "error", err)
		rc, err := h.indexCache.Reader(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("indexCache.Reader: %w", err)
		}
		sub, err = h.createIndex(ctx, rc, size, prefix, idx+1, "application/tar+gzip")
		if err != nil {
			return nil, fmt.Errorf("createIndex(%q, %d): %w", prefix, idx+1, err)
		}
		if sub == nil {
			return nil, fmt.Errorf("createIndex returned nil, not a tar.gz file")
		}
	}

	return soci.NewIndex(bs, toc, sub)
}

func (h *handler) createIndex(ctx context.Context, rc io.ReadCloser, size int64, prefix string, idx int, mediaType string) (soci.Index, error) {
	key := indexKey(prefix, idx)
	cw, err := h.indexCache.Writer(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("indexCache.Writer: %w", err)
	}

	// TODO: Better?
	indexer, _, _, _, err := soci.NewIndexer(rc, cw, spanSize, mediaType)
	if err != nil {
		if err := cw.Close(); err != nil {
			slog.Debug("close cache writer failed", "key", key, "error", err)
		}
		return nil, fmt.Errorf("TODO: don't return this error: %w", err)
	}
	if indexer == nil {
		if err := cw.Close(); err != nil {
			slog.Debug("close cache writer failed", "key", key, "error", err)
		}
		return nil, nil
	}
	for {
		// Make sure we hit the end.
		_, err := indexer.Next()
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			if err := cw.Close(); err != nil {
				slog.Debug("close cache writer failed", "key", key, "error", err)
			}
			return nil, fmt.Errorf("indexer.Next: %w", err)
		}
	}

	toc, err := indexer.TOC()
	if err != nil {
		if err := cw.Close(); err != nil {
			slog.Debug("close cache writer failed", "key", key, "error", err)
		}
		return nil, fmt.Errorf("TOC: %w", err)
	}
	if err := cw.Close(); err != nil {
		return nil, fmt.Errorf("cw.Close: %w", err)
	}
	if h.tocCache != nil {
		if err := h.tocCache.Put(ctx, key, toc); err != nil {
			slog.Debug("store archive TOC failed", "key", key, "error", err)
		}
	}
	slog.Debug("archive indexed", "size", indexer.Size())

	// TODO: Figure out how to simplify getIndexN?
	return h.getIndexN(ctx, prefix, idx)
}

func (h *handler) createFs(w http.ResponseWriter, r *http.Request, ref string, dig ociref.Reference, index soci.Index, mt string) (*soci.SociFS, error) {
	blob, err := h.registryBlob(w, r, dig)
	if err != nil {
		return nil, err
	}
	return soci.FS(index, blob, strings.TrimPrefix(ref, "/"), dig.String(), respTooBig, mt, renderHeader), nil
}
