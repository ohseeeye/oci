package ocis3

import (
	"context"
	"errors"
	"fmt"
	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ociref"
	"io"
	"strings"
)

// referenced scans repository manifests. The scan is not a transaction with
// concurrent pushes; callers must serialize destructive maintenance with writes.
func (r *Registry) referenced(ctx context.Context, repo string, digest oci.Digest, manifest bool) error {
	return r.list(ctx, r.repoKey(repo, "_manifests/"), "", func(key string) (bool, error) {
		suffix := strings.TrimPrefix(key, r.repoKey(repo, "_manifests/"))
		alg, hash, ok := strings.Cut(suffix, "/")
		if !ok {
			return false, fmt.Errorf("invalid manifest key")
		}
		br, err := r.GetManifest(ctx, repo, oci.Digest(alg+":"+hash))
		if errors.Is(err, oci.ErrManifestUnknown) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		data, err := io.ReadAll(io.LimitReader(br, 4*1024*1024+1))
		_ = br.Close()
		if err != nil {
			return false, err
		}
		info, err := manifestInfoFromBytes(br.Descriptor().MediaType, data)
		if err != nil {
			return false, err
		}
		for _, child := range info.children {
			if child.manifest == manifest && child.desc.Digest == digest {
				return false, fmt.Errorf("%w: content is referenced by a manifest", oci.ErrDenied)
			}
		}
		return true, nil
	})
}

// DeleteBlob removes membership, retaining shared content for other repositories.
func (r *Registry) DeleteBlob(ctx context.Context, repo string, digest oci.Digest) error {
	if _, err := r.ResolveBlob(ctx, repo, digest); err != nil {
		return err
	}
	if err := r.referenced(ctx, repo, digest, false); err != nil {
		return err
	}
	key, err := r.membershipKey(repo, digest)
	if err != nil {
		return err
	}
	return r.remove(ctx, key)
}

// DeleteManifest removes a manifest, its referrer entry, and matching tags.
// Each object change commits separately; rerunning cleanup is safe.
func (r *Registry) DeleteManifest(ctx context.Context, repo string, digest oci.Digest) error {
	br, err := r.GetManifest(ctx, repo, digest)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(br, 4*1024*1024+1))
	_ = br.Close()
	if err != nil {
		return err
	}
	info, err := manifestInfoFromBytes(br.Descriptor().MediaType, data)
	if err != nil {
		return err
	}
	if err := r.referenced(ctx, repo, digest, true); err != nil {
		return err
	}
	// Clear pointers before removing bytes so failure leaves a retryable manifest.
	prefix := r.repoKey(repo, "_tags/")
	err = r.list(ctx, prefix, "", func(key string) (bool, error) {
		return true, r.changeTag(ctx, repo, strings.TrimPrefix(key, prefix), oci.Descriptor{}, oci.TagHistoryEventDeleted, digest)
	})
	if err != nil {
		return err
	}
	key, err := r.manifestKey(repo, digest)
	if err != nil {
		return err
	}
	if info.subject != "" {
		ref, err := r.referrerKey(repo, info.subject, digest)
		if err != nil {
			return err
		}
		if err := r.remove(ctx, ref); err != nil {
			return err
		}
	}
	return r.remove(ctx, key)
}

// DeleteTag publishes a tombstone while retaining the full history chain.
func (r *Registry) DeleteTag(ctx context.Context, repo, tag string) error {
	if !ociref.IsValidTag(tag) {
		return oci.ErrNameInvalid
	}
	if err := r.checkRepo(ctx, repo); err != nil {
		return err
	}
	return r.changeTag(ctx, repo, tag, oci.Descriptor{}, oci.TagHistoryEventDeleted, "")
}
