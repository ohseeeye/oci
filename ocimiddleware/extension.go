package ocimiddleware

import (
	"context"
	"iter"

	"github.com/ohseeeye/oci"
)

func forwardTagHistory(ctx context.Context, r oci.Registry, repo, tag string, params *oci.TagHistoryParameters) iter.Seq2[oci.Descriptor, error] {
	history, ok := r.(oci.TagHistory)
	if !ok {
		return oci.ErrorSeq[oci.Descriptor](oci.ErrUnsupported)
	}
	return history.TagHistory(ctx, repo, tag, params)
}
