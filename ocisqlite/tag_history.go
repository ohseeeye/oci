package ocisqlite

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ociref"
	"zombiezen.com/go/sqlite"
)

func recordTagEvent(conn *sqlite.Conn, repo int64, tag string, desc oci.Descriptor, event string) error {
	last, err := integer(conn, `SELECT coalesce(max(created_at),0) FROM tag_history WHERE repository_id=? AND tag=?`, repo, tag)
	if err != nil {
		return err
	}
	now := time.Now().UnixNano()
	if now <= last {
		now = last + 1
	}
	return execute(conn, `INSERT INTO tag_history(repository_id,tag,digest,media_type,artifact_type,size,event_type,created_at) VALUES (?,?,?,?,?,?,?,?)`, repo, tag, desc.Digest.String(), desc.MediaType, desc.ArtifactType, desc.Size, event, now)
}

// TagHistory returns descriptor snapshots newest first, including deleted tags
// and manifests. Missing history yields an empty iterator.
func (r *Registry) TagHistory(ctx context.Context, repo, tag string, params *oci.TagHistoryParameters) iter.Seq2[oci.Descriptor, error] {
	if !ociref.IsValidTag(tag) {
		return oci.ErrorSeq[oci.Descriptor](oci.ErrNameInvalid)
	}
	var p oci.TagHistoryParameters
	if params != nil {
		p = *params
	}
	limit := -1
	if p.Limit != nil {
		if *p.Limit < 0 {
			return oci.ErrorSeq[oci.Descriptor](fmt.Errorf("tag history limit must not be negative"))
		}
		limit = *p.Limit
	}
	if p.Digest != "" {
		if err := p.Digest.Validate(); err != nil {
			return oci.ErrorSeq[oci.Descriptor](oci.ErrDigestInvalid)
		}
	}
	var result []oci.Descriptor
	err := r.withConn(ctx, false, func(conn *sqlite.Conn) error {
		id, err := repository(conn, repo, false)
		if errors.Is(err, oci.ErrNameUnknown) {
			return nil
		}
		if err != nil {
			return err
		}
		query := `SELECT digest,media_type,size,artifact_type,event_type,created_at FROM tag_history WHERE repository_id=? AND tag=?`
		args := []any{id, tag}
		if !p.Before.IsZero() {
			query += " AND created_at<?"
			args = append(args, p.Before.UnixNano())
		}
		if !p.Since.IsZero() {
			query += " AND created_at>?"
			args = append(args, p.Since.UnixNano())
		}
		if p.Digest != "" {
			query += " AND digest=?"
			args = append(args, p.Digest.String())
		}
		query += " ORDER BY created_at DESC LIMIT ?"
		args = append(args, limit)
		return rows(conn, query, func(s *sqlite.Stmt) error {
			result = append(result, oci.Descriptor{
				Digest: oci.Digest(s.ColumnText(0)), MediaType: s.ColumnText(1), Size: s.ColumnInt64(2), ArtifactType: s.ColumnText(3),
				Annotations: map[string]string{
					oci.TagHistoryEventAnnotation:     s.ColumnText(4),
					oci.TagHistoryTimestampAnnotation: time.Unix(0, s.ColumnInt64(5)).UTC().Format(time.RFC3339Nano),
				},
			})
			return nil
		}, args...)
	})
	return snapshot(ctx, result, err)
}
