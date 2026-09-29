package ociserver

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/mux"
	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/ohseeeye/oci/pkg/ociref"
)

const defaultTagHistoryPageSize = 100

func parseTagHistoryParameters(query url.Values) (oci.TagHistoryParameters, int, error) {
	params := oci.TagHistoryParameters{}
	limit := defaultTagHistoryPageSize
	for _, key := range []string{"n", "before", "since", "digest"} {
		if values, ok := query[key]; ok && len(values) != 1 {
			return params, 0, fmt.Errorf("invalid %s", key)
		}
	}
	if values, ok := query["n"]; ok {
		n, err := strconv.Atoi(values[0])
		if err != nil || n < 0 {
			return params, 0, fmt.Errorf("invalid n")
		}
		limit = n
	}
	for _, bound := range []struct {
		name string
		out  *time.Time
	}{{"before", &params.Before}, {"since", &params.Since}} {
		if values, ok := query[bound.name]; ok {
			ts, err := time.Parse(time.RFC3339Nano, values[0])
			if err != nil {
				return params, 0, fmt.Errorf("invalid %s", bound.name)
			}
			*bound.out = ts
		}
	}
	if values, ok := query["digest"]; ok {
		digest, err := ocidigest.Parse(values[0])
		if err != nil {
			return params, 0, fmt.Errorf("invalid digest")
		}
		params.Digest = digest
	}
	fetchLimit := limit
	if limit > 0 && limit < math.MaxInt {
		fetchLimit++ // Look ahead without emitting an empty final page.
	}
	params.Limit = &fetchLimit
	return params, limit, nil
}

func (s *Server) tagHistoryGet(history oci.TagHistory) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo, tag := mux.URLParam(r, "name"), mux.URLParam(r, "tag")
		if !ociref.IsValidTag(tag) {
			returnError(w, ErrBadRequest("invalid tag"))
			return
		}
		params, limit, err := parseTagHistoryParameters(r.URL.Query())
		if err != nil {
			returnError(w, ErrBadRequest(err.Error()))
			return
		}
		entries, err := oci.All(history.TagHistory(r.Context(), repo, tag, &params))
		if err != nil {
			switch {
			case errors.Is(err, oci.ErrNameUnknown), errors.Is(err, oci.ErrManifestUnknown):
				entries = nil // Unknown repository or tag has an empty history.
			case errors.Is(err, oci.ErrUnsupported):
				http.NotFound(w, r)
				return
			default:
				s.logError(r.Context(), "listing tag history", err, "repository", repo, "tag", tag)
				returnError(w, ErrServerError())
				return
			}
		}
		if entries == nil {
			entries = []oci.Descriptor{}
		}
		var next string
		if limit == 0 {
			entries = entries[:0]
		} else if len(entries) > limit {
			entries = entries[:limit]
			next = entries[len(entries)-1].Annotations[oci.TagHistoryTimestampAnnotation]
			if _, err := time.Parse(time.RFC3339Nano, next); err != nil {
				s.logError(r.Context(), "tag history page has invalid cursor", err, "repository", repo, "tag", tag)
				returnError(w, ErrServerError())
				return
			}
		}
		body, err := marshalIndexResponse(entries)
		if err != nil {
			s.logError(r.Context(), "encoding tag history", err, "repository", repo, "tag", tag)
			returnError(w, ErrServerError())
			return
		}
		if next != "" {
			query := r.URL.Query()
			query.Set("n", strconv.Itoa(limit))
			query.Set("before", next)
			w.Header().Set("Link", fmt.Sprintf(`<%s?%s>; rel="next"`, r.URL.EscapedPath(), query.Encode()))
		}
		w.Header().Set("Content-Type", oci.MediaTypeImageIndex)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if _, err := w.Write(body); err != nil {
			s.logError(r.Context(), "writing tag history response", err, "repository", repo, "tag", tag)
		}
	}
}
