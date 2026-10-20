package ociserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ociref"
)

func (s *Server) catalogGet() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		last := query.Get("last")
		if last != "" && !ociref.IsValidRepository(last) {
			returnError(w, ErrBadRequest("invalid last"))
			return
		}
		limit := 0
		if n := query.Get("n"); n != "" {
			i, err := strconv.Atoi(n)
			if err != nil || i < 0 {
				returnError(w, ErrBadRequest("invalid n"))
				return
			}
			limit = i
		}

		repositories := []string{}
		hasNext := false
		for repository, err := range s.db.Repositories(r.Context(), last) {
			if err != nil {
				if returnAuthError(w, r, err) {
					return
				}
				switch {
				case errors.Is(err, oci.ErrUnsupported):
					returnError(w, ErrNotImplemented())
				case errors.Is(err, oci.ErrUnauthorized):
					returnError(w, ErrUnauthorized())
				case errors.Is(err, oci.ErrDenied):
					returnError(w, ErrDenied("catalog access denied"))
				default:
					s.logError(r.Context(), "listing repositories", err)
					returnError(w, ErrServerError())
				}
				return
			}
			// Look ahead one entry without computing limit+1, which could
			// overflow. Stop the iterator rather than collecting the catalog.
			if limit > 0 && len(repositories) == limit {
				hasNext = true
				break
			}
			repositories = append(repositories, repository)
		}
		if hasNext {
			query.Set("last", repositories[len(repositories)-1])
			query.Set("n", strconv.Itoa(limit))
			w.Header().Set("Link", fmt.Sprintf(`<%s?%s>; rel="next"`, r.URL.EscapedPath(), query.Encode()))
		}
		w.Header().Set("Content-Type", "application/json")
		err := json.NewEncoder(w).Encode(struct {
			Repositories []string `json:"repositories"`
		}{Repositories: repositories})
		if err != nil {
			s.logError(r.Context(), "writing catalog response", err)
		}
	}
}
