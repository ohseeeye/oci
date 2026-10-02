package ocis3

import (
	"context"
	"errors"
	"github.com/ohseeeye/oci"
	"iter"
	"strings"
)

// Repositories lists catalog markers in lexical order.
func (r *Registry) Repositories(ctx context.Context, after string) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		prefix := r.prefix + "catalog/"
		err := r.list(ctx, prefix, prefix+after, func(key string) (bool, error) { return yield(strings.TrimPrefix(key, prefix), nil), nil })
		if err != nil {
			yield("", err)
		}
	}
}

// Tags lists current assignments; tombstones do not count toward the limit.
func (r *Registry) Tags(ctx context.Context, repo string, params *oci.TagsParameters) iter.Seq2[string, error] {
	var p oci.TagsParameters
	if params != nil {
		p = *params
	}
	return func(yield func(string, error) bool) {
		if err := r.checkRepo(ctx, repo); err != nil {
			yield("", err)
			return
		}
		prefix := r.repoKey(repo, "_tags/")
		count := 0
		err := r.list(ctx, prefix, prefix+p.StartAfter, func(key string) (bool, error) {
			tag := strings.TrimPrefix(key, prefix)
			_, err := r.ResolveTag(ctx, repo, tag)
			if errors.Is(err, oci.ErrManifestUnknown) {
				return true, nil
			}
			if err != nil {
				return false, err
			}
			count++
			return yield(tag, nil) && (p.Limit <= 0 || count < p.Limit), nil
		})
		if err != nil {
			yield("", err)
		}
	}
}

// Referrers lists descriptor objects for a subject without an aggregate index.
func (r *Registry) Referrers(ctx context.Context, repo string, digest oci.Digest, params *oci.ReferrersParameters) iter.Seq2[oci.Descriptor, error] {
	artifactType := ""
	if params != nil {
		artifactType = params.ArtifactType
	}
	return func(yield func(oci.Descriptor, error) bool) {
		key, err := digestKey(digest)
		if err == nil {
			err = r.checkRepo(ctx, repo)
		}
		if err != nil {
			yield(oci.Descriptor{}, err)
			return
		}
		err = r.list(ctx, r.repoKey(repo, "_referrers/"+key+"/"), "", func(key string) (bool, error) {
			var desc oci.Descriptor
			if _, err := r.getJSON(ctx, key, &desc); err != nil {
				if missing(err) {
					return true, nil
				}
				return false, err
			}
			if artifactType != "" && artifactType != desc.ArtifactType {
				return true, nil
			}
			if _, err := r.ResolveManifest(ctx, repo, desc.Digest); err != nil {
				if errors.Is(err, oci.ErrManifestUnknown) {
					return true, nil
				}
				return false, err
			}
			return yield(desc, nil), nil
		})
		if err != nil {
			yield(oci.Descriptor{}, err)
		}
	}
}
