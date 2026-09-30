package ocisqlite

import (
	"context"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ociref"
	"zombiezen.com/go/sqlite"
)

func resolveBlob(conn *sqlite.Conn, repo int64, digest oci.Digest) (oci.Descriptor, error) {
	if err := digest.Validate(); err != nil {
		return oci.Descriptor{}, oci.ErrDigestInvalid
	}
	var desc oci.Descriptor
	err := rows(conn, `SELECT b.size FROM blobs b JOIN repository_blob rb USING(digest) WHERE rb.repository_id=? AND rb.digest=?`, func(s *sqlite.Stmt) error {
		desc = oci.Descriptor{Digest: digest, Size: s.ColumnInt64(0), MediaType: "application/octet-stream"}
		return nil
	}, repo, digest.String())
	if err != nil {
		return oci.Descriptor{}, err
	}
	if desc.Digest == "" {
		return desc, oci.ErrBlobUnknown
	}
	return desc, nil
}

func resolveManifest(conn *sqlite.Conn, repo int64, digest oci.Digest, tag string) (oci.Descriptor, error) {
	query := `SELECT m.digest,m.media_type,b.size FROM manifests m JOIN blobs b USING(digest) WHERE m.repository_id=? AND m.digest=?`
	value := digest.String()
	if tag != "" {
		query = `SELECT m.digest,m.media_type,b.size FROM tag t JOIN manifests m ON m.repository_id=t.repository_id AND m.digest=t.digest JOIN blobs b ON b.digest=m.digest WHERE t.repository_id=? AND t.name=?`
		value = tag
	} else if err := digest.Validate(); err != nil {
		return oci.Descriptor{}, oci.ErrDigestInvalid
	}
	var desc oci.Descriptor
	err := rows(conn, query, func(s *sqlite.Stmt) error {
		desc = oci.Descriptor{Digest: oci.Digest(s.ColumnText(0)), MediaType: s.ColumnText(1), Size: s.ColumnInt64(2)}
		return nil
	}, repo, value)
	if err != nil {
		return oci.Descriptor{}, err
	}
	if desc.Digest == "" {
		return desc, oci.ErrManifestUnknown
	}
	return desc, nil
}

// ResolveBlob returns a descriptor for content present in repo.
func (r *Registry) ResolveBlob(ctx context.Context, repo string, digest oci.Digest) (desc oci.Descriptor, err error) {
	err = r.withConn(ctx, false, func(conn *sqlite.Conn) error {
		id, err := repository(conn, repo, false)
		if err != nil {
			return err
		}
		desc, err = resolveBlob(conn, id, digest)
		return err
	})
	return desc, err
}

// ResolveManifest returns the descriptor for a manifest in repo.
func (r *Registry) ResolveManifest(ctx context.Context, repo string, digest oci.Digest) (desc oci.Descriptor, err error) {
	err = r.withConn(ctx, false, func(conn *sqlite.Conn) error {
		id, err := repository(conn, repo, false)
		if err != nil {
			return err
		}
		desc, err = resolveManifest(conn, id, digest, "")
		return err
	})
	return desc, err
}

// ResolveTag returns the descriptor for the manifest assigned to tag.
func (r *Registry) ResolveTag(ctx context.Context, repo, tag string) (desc oci.Descriptor, err error) {
	if !ociref.IsValidTag(tag) {
		return desc, oci.ErrNameInvalid
	}
	err = r.withConn(ctx, false, func(conn *sqlite.Conn) error {
		id, err := repository(conn, repo, false)
		if err != nil {
			return err
		}
		desc, err = resolveManifest(conn, id, "", tag)
		return err
	})
	return desc, err
}

// GetBlob opens a blob's content. Closing the reader releases its file handle.
func (r *Registry) GetBlob(ctx context.Context, repo string, digest oci.Digest) (oci.BlobReader, error) {
	return r.GetBlobRange(ctx, repo, digest, 0, -1)
}

// GetBlobRange opens the half-open byte range [start,end). A negative end or an
// end beyond the content size selects all remaining bytes.
func (r *Registry) GetBlobRange(ctx context.Context, repo string, digest oci.Digest, start, end int64) (oci.BlobReader, error) {
	desc, err := r.ResolveBlob(ctx, repo, digest)
	if err != nil {
		return nil, err
	}
	return r.openBlob(ctx, desc, start, end)
}

// GetManifest opens a manifest's content.
func (r *Registry) GetManifest(ctx context.Context, repo string, digest oci.Digest) (oci.BlobReader, error) {
	desc, err := r.ResolveManifest(ctx, repo, digest)
	if err != nil {
		return nil, err
	}
	return r.openBlob(ctx, desc, 0, -1)
}

// GetTag opens the manifest assigned to tag.
func (r *Registry) GetTag(ctx context.Context, repo, tag string) (oci.BlobReader, error) {
	desc, err := r.ResolveTag(ctx, repo, tag)
	if err != nil {
		return nil, err
	}
	return r.openBlob(ctx, desc, 0, -1)
}
