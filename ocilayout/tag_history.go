package ocilayout

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"maps"
	"time"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ociref"
)

// This custom index annotation is an experimental on-disk extension, not part
// of the OCI Image Layout specification. Its value is JSON because index
// annotations are strings. Existing layouts without it have no known history.
const tagHistoryAnnotation = "io.github.ohseeeye.oci.tag-history.v1"

type tagHistoryStore map[string]map[string][]oci.Descriptor // repository -> tag -> oldest-first events

func loadTagHistory(index oci.IndexOrManifest) (tagHistoryStore, error) {
	encoded := index.Annotations[tagHistoryAnnotation]
	if encoded == "" {
		return make(tagHistoryStore), nil
	}
	var history tagHistoryStore
	if err := json.Unmarshal([]byte(encoded), &history); err != nil {
		return nil, fmt.Errorf("invalid tag history annotation: %w", err)
	}
	if history == nil {
		return nil, fmt.Errorf("invalid tag history annotation: null store")
	}
	return history, nil
}

// recordTagHistory modifies a copy of the index before saveIndex commits it.
func recordTagHistory(index *oci.IndexOrManifest, repo string, tags []string, desc oci.Descriptor, event string) error {
	if len(tags) == 0 {
		return nil
	}
	history, err := loadTagHistory(*index)
	if err != nil {
		return err
	}
	if history[repo] == nil {
		history[repo] = make(map[string][]oci.Descriptor)
	}
	for _, tag := range tags {
		now := time.Now().UTC()
		previous := history[repo][tag]
		if len(previous) > 0 {
			last, err := time.Parse(time.RFC3339Nano, previous[len(previous)-1].Annotations[oci.TagHistoryTimestampAnnotation])
			if err != nil {
				return fmt.Errorf("invalid previous tag history timestamp: %w", err)
			}
			if !now.After(last) {
				now = last.Add(time.Nanosecond)
			}
		}
		eventDesc := desc
		eventDesc.Annotations = map[string]string{
			oci.TagHistoryTimestampAnnotation: now.Format(time.RFC3339Nano),
			oci.TagHistoryEventAnnotation:     event,
		}
		history[repo][tag] = append(previous, eventDesc)
	}
	data, err := json.Marshal(history)
	if err != nil {
		return fmt.Errorf("encoding tag history: %w", err)
	}
	index.Annotations = maps.Clone(index.Annotations)
	if index.Annotations == nil {
		index.Annotations = make(map[string]string)
	}
	index.Annotations[tagHistoryAnnotation] = string(data)
	return nil
}

// TagHistory returns persisted tag events newest first. It reads the custom
// annotation in index.json; layouts created by other tools have empty history.
func (r *Registry) TagHistory(ctx context.Context, repo, tag string, params *oci.TagHistoryParameters) iter.Seq2[oci.Descriptor, error] {
	if err := contextErr(ctx); err != nil {
		return oci.ErrorSeq[oci.Descriptor](err)
	}
	if !ociref.IsValidTag(tag) {
		return oci.ErrorSeq[oci.Descriptor](oci.ErrNameInvalid)
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
	r.mu.Lock()
	st, err := r.layoutForRepoLocked(repo, false)
	if err != nil {
		r.mu.Unlock()
		return oci.ErrorSeq[oci.Descriptor](err)
	}
	history, err := loadTagHistory(st.index)
	if err != nil {
		r.mu.Unlock()
		return oci.ErrorSeq[oci.Descriptor](err)
	}
	result := make([]oci.Descriptor, 0, len(history[repo][tag]))
	if limit == nil || *limit != 0 {
		entries := history[repo][tag]
		for i := len(entries) - 1; i >= 0; i-- {
			entry := entries[i]
			ts, err := time.Parse(time.RFC3339Nano, entry.Annotations[oci.TagHistoryTimestampAnnotation])
			if err != nil {
				r.mu.Unlock()
				return oci.ErrorSeq[oci.Descriptor](fmt.Errorf("invalid tag history timestamp: %w", err))
			}
			if !before.IsZero() && !ts.Before(before) || !since.IsZero() && !ts.After(since) || digest != "" && entry.Digest != digest {
				continue
			}
			entry.Annotations = maps.Clone(entry.Annotations)
			result = append(result, entry)
			if limit != nil && len(result) >= *limit {
				break
			}
		}
	}
	r.mu.Unlock()
	return func(yield func(oci.Descriptor, error) bool) {
		for _, entry := range result {
			if err := contextErr(ctx); err != nil {
				yield(oci.Descriptor{}, err)
				return
			}
			if !yield(entry, nil) {
				return
			}
		}
	}
}
