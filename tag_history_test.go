package oci_test

import (
	"context"
	"iter"
	"testing"
	"time"

	"github.com/ohseeeye/oci"
)

// Embedding Funcs supplies the stable API while TagHistory remains optional.
type tagHistoryRegistry struct{ *oci.Funcs }

func (*tagHistoryRegistry) TagHistory(context.Context, string, string, *oci.TagHistoryParameters) iter.Seq2[oci.Descriptor, error] {
	return oci.SliceSeq([]oci.Descriptor{})
}

var _ oci.TagHistory = (*tagHistoryRegistry)(nil)
var _ oci.Registry = (*oci.Funcs)(nil)

func TestTagHistoryParameters(t *testing.T) {
	zero := 0
	params := oci.TagHistoryParameters{
		Limit:  &zero,
		Before: time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC),
		Since:  time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
	}
	if params.Limit == nil || *params.Limit != 0 || params.Before.IsZero() || params.Since.IsZero() {
		t.Fatal("tag history parameters did not preserve optional filters")
	}
	var registry oci.Registry = &tagHistoryRegistry{Funcs: &oci.Funcs{}}
	if _, ok := registry.(oci.TagHistory); !ok {
		t.Fatal("registry does not expose TagHistory")
	}
}
