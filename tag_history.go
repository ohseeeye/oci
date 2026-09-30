package oci

import (
	"context"
	"iter"
	"time"
)

// TagHistory is the proposed tag-history capability. It is independent of
// Registry so callers can check for this operation with a type assertion
// without requiring any other experimental API.
type TagHistory interface {
	// TagHistory returns the history of tag in repo, newest event first.
	// Each Descriptor describes the manifest the tag pointed to and has both
	// TagHistoryTimestampAnnotation and TagHistoryEventAnnotation. Events are
	// either TagHistoryEventCreated or TagHistoryEventDeleted. A deleted tag
	// may still have history; an absent history yields an empty iterator.
	// A nil params value leaves all filters unspecified. Implementations should
	// follow the pagination semantics of the proposed tag-history extension.
	TagHistory(ctx context.Context, repo, tag string, params *TagHistoryParameters) iter.Seq2[Descriptor, error]
}

const (
	// TagHistoryTimestampAnnotation is the RFC 3339 time of a tag event.
	TagHistoryTimestampAnnotation = "org.opencontainers.distribution.tag.timestamp"
	// TagHistoryEventAnnotation identifies whether a tag was assigned or deleted.
	TagHistoryEventAnnotation = "org.opencontainers.distribution.tag.event"
	// TagHistoryEventCreated marks a tag assignment.
	TagHistoryEventCreated = "created"
	// TagHistoryEventDeleted marks a tag deletion.
	TagHistoryEventDeleted = "deleted"
)

// TagHistoryParameters holds optional filters for [TagHistory.TagHistory].
// A nil value means no filters. Before and Since are exclusive bounds on the
// history event timestamp, and may be combined. A zero Digest is not a filter.
type TagHistoryParameters struct {
	// Limit is the maximum number of entries to return. Nil leaves the limit
	// unspecified; a pointer to zero requests an empty result (the proposed
	// extension's capability check). Negative values are invalid.
	Limit *int
	// Before selects entries strictly older than this time. Zero is unset.
	Before time.Time
	// Since selects entries strictly newer than this time. Zero is unset.
	Since time.Time
	// Digest restricts entries to the given manifest digest. Zero is unset.
	Digest Digest
}
