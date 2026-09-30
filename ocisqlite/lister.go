package ocisqlite

import (
	"context"
	"iter"

	"github.com/ohseeeye/oci"
	"zombiezen.com/go/sqlite"
)

func snapshot[T any](ctx context.Context, values []T, err error) iter.Seq2[T, error] {
	if err != nil {
		return oci.ErrorSeq[T](err)
	}
	return func(yield func(T, error) bool) {
		for _, value := range values {
			if err := ctx.Err(); err != nil {
				var zero T
				yield(zero, err)
				return
			}
			if !yield(value, nil) {
				return
			}
		}
	}
}

// Repositories returns repository names in lexical order after startAfter.
func (r *Registry) Repositories(ctx context.Context, startAfter string) iter.Seq2[string, error] {
	var result []string
	err := r.withConn(ctx, false, func(conn *sqlite.Conn) error {
		return rows(conn, "SELECT name FROM repository WHERE name>? ORDER BY name", func(s *sqlite.Stmt) error { result = append(result, s.ColumnText(0)); return nil }, startAfter)
	})
	return snapshot(ctx, result, err)
}

// Tags returns tags in lexical order with optional pagination.
func (r *Registry) Tags(ctx context.Context, repo string, params *oci.TagsParameters) iter.Seq2[string, error] {
	var result []string
	start, limit := "", -1
	if params != nil {
		start = params.StartAfter
		if params.Limit > 0 {
			limit = params.Limit
		}
	}
	err := r.withConn(ctx, false, func(conn *sqlite.Conn) error {
		id, err := repository(conn, repo, false)
		if err != nil {
			return err
		}
		return rows(conn, "SELECT name FROM tag WHERE repository_id=? AND name>? ORDER BY name LIMIT ?", func(s *sqlite.Stmt) error { result = append(result, s.ColumnText(0)); return nil }, id, start, limit)
	})
	return snapshot(ctx, result, err)
}

// Referrers returns manifests whose subject is digest, ordered by digest.
func (r *Registry) Referrers(ctx context.Context, repo string, digest oci.Digest, params *oci.ReferrersParameters) iter.Seq2[oci.Descriptor, error] {
	if err := digest.Validate(); err != nil {
		return oci.ErrorSeq[oci.Descriptor](oci.ErrDigestInvalid)
	}
	var result []oci.Descriptor
	artifactType := ""
	if params != nil {
		artifactType = params.ArtifactType
	}
	err := r.withConn(ctx, false, func(conn *sqlite.Conn) error {
		id, err := repository(conn, repo, false)
		if err != nil {
			return err
		}
		if err := rows(conn, `SELECT m.digest,m.media_type,b.size,m.artifact_type FROM manifests m JOIN blobs b USING(digest) WHERE m.repository_id=? AND m.subject=? AND (?='' OR m.artifact_type=?) ORDER BY m.digest`, func(s *sqlite.Stmt) error {
			result = append(result, oci.Descriptor{Digest: oci.Digest(s.ColumnText(0)), MediaType: s.ColumnText(1), Size: s.ColumnInt64(2), ArtifactType: s.ColumnText(3)})
			return nil
		}, id, digest.String(), artifactType, artifactType); err != nil {
			return err
		}
		for i := range result {
			if err := rows(conn, `SELECT annotation_key,annotation_value FROM manifest_annotations WHERE repository_id=? AND manifest=?`, func(s *sqlite.Stmt) error {
				if result[i].Annotations == nil {
					result[i].Annotations = make(map[string]string)
				}
				result[i].Annotations[s.ColumnText(0)] = s.ColumnText(1)
				return nil
			}, id, result[i].Digest.String()); err != nil {
				return err
			}
		}
		return nil
	})
	return snapshot(ctx, result, err)
}
