package ocis3

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"time"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ociref"
)

type tagEvent struct {
	ID         string         `json:"id"`
	Previous   string         `json:"previous,omitempty"`
	Timestamp  time.Time      `json:"timestamp"`
	Event      string         `json:"event"`
	Descriptor oci.Descriptor `json:"descriptor"`
}
type tagRecord struct {
	Event tagEvent `json:"event"`
}

func (r *Registry) tagPointer(ctx context.Context, repo, tag string) (tagRecord, string, error) {
	var p tagRecord
	etag, err := r.getJSON(ctx, r.repoKey(repo, "_tags/"+tag), &p)
	if missing(err) {
		err = oci.ErrManifestUnknown
	}
	if err == nil {
		err = validateEvent(p.Event)
	}
	return p, etag, err
}
func validateEvent(e tagEvent) error {
	if !validID(e.ID) || (e.Previous != "" && !validID(e.Previous)) || e.Timestamp.IsZero() || e.Descriptor.Size < 0 || e.Descriptor.Digest.Validate() != nil || (e.Event != oci.TagHistoryEventCreated && e.Event != oci.TagHistoryEventDeleted) {
		return fmt.Errorf("invalid stored tag event")
	}
	return nil
}

// changeTag commits an immutable event by conditionally publishing its pointer.
// Losing writers leave unreachable events, which are never returned as history.
// expected restricts manifest-deletion cleanup to a still-matching assignment.
func (r *Registry) changeTag(ctx context.Context, repo, tag string, desc oci.Descriptor, event string, expected oci.Digest) error {
	for range 64 {
		current, etag, err := r.tagPointer(ctx, repo, tag)
		absent := errors.Is(err, oci.ErrManifestUnknown)
		if err != nil && !absent {
			return err
		}
		if event == oci.TagHistoryEventDeleted {
			if absent || current.Event.Event != oci.TagHistoryEventCreated {
				if expected != "" {
					return nil
				}
				return oci.ErrManifestUnknown
			}
			if expected != "" && current.Event.Descriptor.Digest != expected {
				return nil
			}
			desc = current.Event.Descriptor
		}
		now := time.Now().UTC()
		if !absent && !now.After(current.Event.Timestamp) {
			now = current.Event.Timestamp.Add(time.Nanosecond)
		}
		next := tagRecord{Event: tagEvent{ID: newID(), Timestamp: now, Event: event, Descriptor: desc}}
		if !absent {
			next.Event.Previous = current.Event.ID
		}
		if _, err := r.putJSON(ctx, r.repoKey(repo, "_tag_history/"+tag+"/"+next.Event.ID), next.Event, "", true); err != nil {
			return err
		}
		_, err = r.putJSON(ctx, r.repoKey(repo, "_tags/"+tag), next, etag, absent)
		if err == nil {
			return nil
		}
		// A transport failure can follow a successful PUT. Do not retry blindly:
		// read the committed chain to determine whether this event became reachable.
		// SDK retries can turn a lost successful response into a precondition error.
		checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		committed := r.eventCommitted(checkCtx, repo, tag, next.Event.ID, next.Event.Previous)
		cancel()
		if committed {
			return nil
		}
		if conflict(err) {
			continue
		}
		return err
	}
	return fmt.Errorf("%w: tag changed too frequently", oci.ErrDenied)
}

func (r *Registry) eventCommitted(ctx context.Context, repo, tag, id, stop string) bool {
	p, _, err := r.tagPointer(ctx, repo, tag)
	seen := map[string]bool{}
	for err == nil && !seen[p.Event.ID] {
		if p.Event.ID == id {
			return true
		}
		if p.Event.ID == stop {
			return false
		}
		seen[p.Event.ID] = true
		if p.Event.Previous == "" {
			break
		}
		_, err = r.getJSON(ctx, r.repoKey(repo, "_tag_history/"+tag+"/"+p.Event.Previous), &p.Event)
		if err == nil {
			err = validateEvent(p.Event)
		}
	}
	return false
}

// TagHistory follows a snapshot of the committed event chain, newest first.
func (r *Registry) TagHistory(ctx context.Context, repo, tag string, params *oci.TagHistoryParameters) iter.Seq2[oci.Descriptor, error] {
	var p oci.TagHistoryParameters
	if params != nil {
		p = *params
	}
	if p.Limit != nil {
		n := *p.Limit
		p.Limit = &n
	}
	return func(yield func(oci.Descriptor, error) bool) {
		fail := func(err error) { yield(oci.Descriptor{}, err) }
		if err := ctx.Err(); err != nil {
			fail(err)
			return
		}
		if err := validateRepo(repo); err != nil {
			fail(err)
			return
		}
		if !ociref.IsValidTag(tag) {
			fail(oci.ErrNameInvalid)
			return
		}
		if p.Limit != nil && *p.Limit < 0 {
			fail(oci.ErrRangeInvalid)
			return
		}
		if p.Digest != "" && p.Digest.Validate() != nil {
			fail(oci.ErrDigestInvalid)
			return
		}
		if p.Limit != nil && *p.Limit == 0 {
			return
		}
		ptr, _, err := r.tagPointer(ctx, repo, tag)
		if errors.Is(err, oci.ErrManifestUnknown) {
			return
		}
		if err != nil {
			fail(err)
			return
		}
		seen := map[string]bool{}
		count := 0
		for {
			e := ptr.Event
			if err := ctx.Err(); err != nil {
				fail(err)
				return
			}
			if seen[e.ID] {
				fail(fmt.Errorf("cyclic tag history"))
				return
			}
			seen[e.ID] = true
			if !p.Since.IsZero() && !e.Timestamp.After(p.Since) {
				return
			}
			if (p.Before.IsZero() || e.Timestamp.Before(p.Before)) && (p.Digest == "" || p.Digest == e.Descriptor.Digest) {
				desc := e.Descriptor
				desc.Annotations = maps.Clone(desc.Annotations)
				if desc.Annotations == nil {
					desc.Annotations = map[string]string{}
				}
				desc.Annotations[oci.TagHistoryTimestampAnnotation] = e.Timestamp.Format(time.RFC3339Nano)
				desc.Annotations[oci.TagHistoryEventAnnotation] = e.Event
				if !yield(desc, nil) {
					return
				}
				count++
				if p.Limit != nil && count >= *p.Limit {
					return
				}
			}
			if e.Previous == "" {
				return
			}
			var previous tagEvent
			if _, err := r.getJSON(ctx, r.repoKey(repo, "_tag_history/"+tag+"/"+e.Previous), &previous); err != nil {
				fail(err)
				return
			}
			if err := validateEvent(previous); err != nil {
				fail(err)
				return
			}
			if previous.ID != e.Previous || !previous.Timestamp.Before(e.Timestamp) {
				fail(fmt.Errorf("invalid tag history chain"))
				return
			}
			ptr.Event = previous
		}
	}
}
