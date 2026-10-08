package ocisqlite

import (
	"context"
	"fmt"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ociref"
	"zombiezen.com/go/sqlite"
)

// DeleteBlob removes repository membership. Content files are retained for
// readers and other repositories. Referenced blobs are protected unless sparse
// manifests are enabled. Manifest backing blobs are always protected.
func (r *Registry) DeleteBlob(ctx context.Context, repo string, digest oci.Digest) error {
	return r.withConn(ctx, true, func(conn *sqlite.Conn) error {
		id, err := repository(conn, repo, false)
		if err != nil {
			return err
		}
		if _, err := resolveBlob(conn, id, digest); err != nil {
			return err
		}
		n, err := integer(conn, `SELECT (SELECT count(*) FROM manifest_blob WHERE repository_id=? AND blob_digest=?) + (SELECT count(*) FROM manifests WHERE repository_id=? AND digest=?)`, id, digest.String(), id, digest.String())
		if err != nil {
			return err
		}
		if n > 0 && !r.allowSparseManifests {
			return fmt.Errorf("%w: blob is referenced by a manifest", oci.ErrDenied)
		}
		// A manifest's own backing blob cannot be removed independently.
		own, err := integer(conn, "SELECT count(*) FROM manifests WHERE repository_id=? AND digest=?", id, digest.String())
		if err != nil {
			return err
		}
		if own > 0 {
			return fmt.Errorf("%w: blob stores a manifest", oci.ErrDenied)
		}
		return execute(conn, "DELETE FROM repository_blob WHERE repository_id=? AND digest=?", id, digest.String())
	})
}

// DeleteManifest removes a manifest and its tags, preserving tag history.
// A manifest referenced by an index is protected unless sparse manifests are enabled.
func (r *Registry) DeleteManifest(ctx context.Context, repo string, digest oci.Digest) error {
	return r.withConn(ctx, true, func(conn *sqlite.Conn) error {
		id, err := repository(conn, repo, false)
		if err != nil {
			return err
		}
		desc, err := resolveManifest(conn, id, digest, "")
		if err != nil {
			return err
		}
		n, err := integer(conn, "SELECT count(*) FROM manifest_manifest WHERE repository_id=? AND child_digest=?", id, digest.String())
		if err != nil {
			return err
		}
		if n > 0 && !r.allowSparseManifests {
			return fmt.Errorf("%w: manifest is referenced by an index", oci.ErrDenied)
		}
		if err := rows(conn, "SELECT artifact_type FROM manifests WHERE repository_id=? AND digest=?", func(s *sqlite.Stmt) error { desc.ArtifactType = s.ColumnText(0); return nil }, id, digest.String()); err != nil {
			return err
		}
		var tags []string
		if err := rows(conn, "SELECT name FROM tag WHERE repository_id=? AND digest=? ORDER BY name", func(s *sqlite.Stmt) error { tags = append(tags, s.ColumnText(0)); return nil }, id, digest.String()); err != nil {
			return err
		}
		for _, tag := range tags {
			if err := recordTagEvent(conn, id, tag, desc, oci.TagHistoryEventDeleted); err != nil {
				return err
			}
		}
		return execute(conn, "DELETE FROM manifests WHERE repository_id=? AND digest=?", id, digest.String())
	})
}

// DeleteTag removes a tag assignment and records a deletion event atomically.
func (r *Registry) DeleteTag(ctx context.Context, repo, tag string) error {
	if !ociref.IsValidTag(tag) {
		return oci.ErrNameInvalid
	}
	return r.withConn(ctx, true, func(conn *sqlite.Conn) error {
		id, err := repository(conn, repo, false)
		if err != nil {
			return err
		}
		desc, err := resolveManifest(conn, id, "", tag)
		if err != nil {
			return err
		}
		if err := rows(conn, "SELECT artifact_type FROM manifests WHERE repository_id=? AND digest=?", func(s *sqlite.Stmt) error { desc.ArtifactType = s.ColumnText(0); return nil }, id, desc.Digest.String()); err != nil {
			return err
		}
		if err := recordTagEvent(conn, id, tag, desc, oci.TagHistoryEventDeleted); err != nil {
			return err
		}
		return execute(conn, "DELETE FROM tag WHERE repository_id=? AND name=?", id, tag)
	})
}
