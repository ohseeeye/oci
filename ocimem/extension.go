package ocimem

import (
	"context"
	"fmt"
	"iter"
	"maps"
	"time"

	"github.com/ohseeeye/oci"
)

type tagHistoryEntry struct {
	desc      oci.Descriptor
	timestamp time.Time
}

// recordTagEvent records an event while the registry mutex is held.
func (repo *repository) recordTagEvent(tag string, desc oci.Descriptor, event string) {
	now := time.Now().UTC()
	history := repo.tagHistory[tag]
	if len(history) > 0 && !now.After(history[len(history)-1].timestamp) {
		// A strict order keeps time-based pagination reliable even when the
		// clock has insufficient precision or moves backwards.
		now = history[len(history)-1].timestamp.Add(time.Nanosecond)
	}
	desc.Annotations = map[string]string{
		oci.TagHistoryTimestampAnnotation: now.Format(time.RFC3339Nano),
		oci.TagHistoryEventAnnotation:     event,
	}
	repo.tagHistory[tag] = append(history, tagHistoryEntry{desc: desc, timestamp: now})
}

// TagHistory returns a snapshot of the named tag's history, newest first.
// Unknown repositories and tags yield an empty iterator, as proposed by the
// tag-history extension. The history survives tag and manifest deletion.
func (r *Registry) TagHistory(ctx context.Context, repoName, tag string, params *oci.TagHistoryParameters) iter.Seq2[oci.Descriptor, error] {
	if err := ctx.Err(); err != nil {
		return oci.ErrorSeq[oci.Descriptor](err)
	}
	var limit *int
	var before, since time.Time
	var digest oci.Digest
	if params != nil {
		limit, before, since, digest = params.Limit, params.Before, params.Since, params.Digest
	}
	if limit != nil && *limit < 0 {
		return oci.ErrorSeq[oci.Descriptor](fmt.Errorf("tag history limit must not be negative"))
	}
	if digest != "" {
		if err := digest.Validate(); err != nil {
			return oci.ErrorSeq[oci.Descriptor](fmt.Errorf("invalid tag history digest: %w", err))
		}
	}
	if limit != nil && *limit == 0 {
		return oci.SliceSeq([]oci.Descriptor{})
	}

	r.mu.Lock()
	var history []tagHistoryEntry
	if repo := r.repos[repoName]; repo != nil {
		history = repo.tagHistory[tag]
	}
	result := make([]oci.Descriptor, 0, len(history))
	for i := len(history) - 1; i >= 0; i-- {
		entry := history[i]
		if !before.IsZero() && !entry.timestamp.Before(before) {
			continue
		}
		if !since.IsZero() && !entry.timestamp.After(since) {
			continue
		}
		if digest != "" && entry.desc.Digest != digest {
			continue
		}
		desc := entry.desc
		desc.Annotations = maps.Clone(desc.Annotations)
		result = append(result, desc)
		if limit != nil && len(result) >= *limit {
			break
		}
	}
	r.mu.Unlock()

	return func(yield func(oci.Descriptor, error) bool) {
		for _, desc := range result {
			if err := ctx.Err(); err != nil {
				yield(oci.Descriptor{}, err)
				return
			}
			if !yield(desc, nil) {
				return
			}
		}
	}
}
