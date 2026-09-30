package ocisqlite

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/ohseeeye/oci/pkg/ociref"
	"zombiezen.com/go/sqlite"
)

// PushBlob verifies and publishes content before recording repository membership.
func (r *Registry) PushBlob(ctx context.Context, repo string, desc oci.Descriptor, content io.Reader) (oci.Descriptor, error) {
	if err := ctx.Err(); err != nil {
		return oci.Descriptor{}, err
	}
	if !ociref.IsValidRepository(repo) || len(repo) > 255 {
		return oci.Descriptor{}, oci.ErrNameInvalid
	}
	desc, err := writeBlob(ctx, r.dir, desc, content)
	if err != nil {
		return oci.Descriptor{}, err
	}
	err = r.withConn(ctx, true, func(conn *sqlite.Conn) error {
		id, err := repository(conn, repo, true)
		if err != nil {
			return err
		}
		return addBlob(conn, id, desc)
	})
	if err != nil {
		return oci.Descriptor{}, err
	}
	return desc, nil
}

// MountBlob adds repository membership without copying content.
func (r *Registry) MountBlob(ctx context.Context, fromRepo, toRepo string, digest oci.Digest) (desc oci.Descriptor, err error) {
	err = r.withConn(ctx, true, func(conn *sqlite.Conn) error {
		from, err := repository(conn, fromRepo, false)
		if err != nil {
			return err
		}
		desc, err = resolveBlob(conn, from, digest)
		if err != nil {
			return err
		}
		to, err := repository(conn, toRepo, true)
		if err != nil {
			return err
		}
		return addBlob(conn, to, desc)
	})
	return desc, err
}

// PushManifest stores a manifest and updates its tags and history atomically.
// Required children must already be present in the same repository; subjects
// may be dangling and layers with external URLs need not be stored locally.
func (r *Registry) PushManifest(ctx context.Context, repo string, data []byte, mediaType string, params *oci.PushManifestParameters) (oci.Descriptor, error) {
	if err := ctx.Err(); err != nil {
		return oci.Descriptor{}, err
	}
	if !ociref.IsValidRepository(repo) || len(repo) > 255 {
		return oci.Descriptor{}, oci.ErrNameInvalid
	}
	if mediaType == "" || len(mediaType) > oci.MaxMediaTypeLen {
		return oci.Descriptor{}, oci.ErrManifestInvalid
	}
	// Own the content while validating, publishing, and deriving metadata.
	data = slices.Clone(data)
	digest := ocidigest.FromBytes(data)
	var tags []string
	if params != nil {
		tags = slices.Compact(slices.Sorted(slices.Values(params.Tags)))
		if params.Digest != "" {
			digest = params.Digest
		}
	}
	for _, tag := range tags {
		if !ociref.IsValidTag(tag) {
			return oci.Descriptor{}, oci.ErrNameInvalid
		}
	}
	info, err := manifestInfoFromBytes(mediaType, data)
	if err != nil {
		return oci.Descriptor{}, fmt.Errorf("%w: %v", oci.ErrManifestInvalid, err)
	}
	desc := oci.Descriptor{Digest: digest, Size: int64(len(data)), MediaType: mediaType}
	if _, err := writeBlob(ctx, r.dir, desc, bytes.NewReader(data)); err != nil {
		return oci.Descriptor{}, err
	}
	err = r.withConn(ctx, true, func(conn *sqlite.Conn) error {
		id, err := repository(conn, repo, true)
		if err != nil {
			return err
		}
		for _, child := range info.children {
			if !child.manifest && len(child.desc.URLs) > 0 {
				continue
			}
			var got oci.Descriptor
			if child.manifest {
				got, err = resolveManifest(conn, id, child.desc.Digest, "")
			} else {
				got, err = resolveBlob(conn, id, child.desc.Digest)
			}
			if err != nil {
				return fmt.Errorf("%w: child %s: %v", oci.ErrManifestInvalid, child.desc.Digest, err)
			}
			if got.Size != child.desc.Size || (child.manifest && got.MediaType != child.desc.MediaType) {
				return fmt.Errorf("%w: child descriptor mismatch", oci.ErrManifestInvalid)
			}
		}
		if err := addBlob(conn, id, desc); err != nil {
			return err
		}
		// Content is immutable, including its interpretation as a manifest.
		existing, err := resolveManifest(conn, id, digest, "")
		if err == nil && existing.MediaType != mediaType {
			return fmt.Errorf("%w: mismatched manifest media type", oci.ErrDenied)
		}
		if err != nil && err != oci.ErrManifestUnknown {
			return err
		}
		if err := execute(conn, `INSERT INTO manifests(repository_id,digest,media_type,artifact_type,subject,created_at) VALUES (?,?,?,?,?,?) ON CONFLICT DO NOTHING`, id, digest.String(), mediaType, info.artifactType, info.subject.String(), time.Now().UnixNano()); err != nil {
			return err
		}
		for _, child := range info.children {
			if !child.manifest && len(child.desc.URLs) > 0 {
				continue
			}
			query := `INSERT INTO manifest_blob(repository_id,manifest,blob_digest) VALUES (?,?,?) ON CONFLICT DO NOTHING`
			if child.manifest {
				query = `INSERT INTO manifest_manifest(repository_id,manifest,child_digest) VALUES (?,?,?) ON CONFLICT DO NOTHING`
			}
			if err := execute(conn, query, id, digest.String(), child.desc.Digest.String()); err != nil {
				return err
			}
		}
		for key, value := range info.annotations {
			if err := execute(conn, `INSERT INTO manifest_annotations(repository_id,manifest,annotation_key,annotation_value) VALUES (?,?,?,?) ON CONFLICT DO NOTHING`, id, digest.String(), key, value); err != nil {
				return err
			}
		}
		historyDesc := desc
		historyDesc.ArtifactType = info.artifactType
		for _, tag := range tags {
			if err := execute(conn, `INSERT INTO tag(repository_id,name,digest,created_at) VALUES (?,?,?,?) ON CONFLICT(repository_id,name) DO UPDATE SET digest=excluded.digest,created_at=excluded.created_at`, id, tag, digest.String(), time.Now().UnixNano()); err != nil {
				return err
			}
			if err := recordTagEvent(conn, id, tag, historyDesc, oci.TagHistoryEventCreated); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return oci.Descriptor{}, err
	}
	return desc, nil
}
