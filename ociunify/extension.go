package ociunify

import (
	"context"
	"iter"

	"github.com/ohseeeye/oci"
)

// TagHistory returns ErrUnsupported because history merging is undefined.
func (unifier) TagHistory(ctx context.Context, repo, tag string, params *oci.TagHistoryParameters) iter.Seq2[oci.Descriptor, error] {
	return oci.ErrorSeq[oci.Descriptor](oci.ErrUnsupported)
}
