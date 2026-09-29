package ociclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"time"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ociref"
)

// TagHistory lists a tag's historical manifest assignments and deletions.
// A 404 from the proposed extension is returned as [oci.ErrUnsupported] while
// retaining the upstream HTTP error and its status code.
func (c *Client) TagHistory(ctx context.Context, repo, tag string, params *oci.TagHistoryParameters) iter.Seq2[oci.Descriptor, error] {
	if !ociref.IsValidRepository(repo) || !ociref.IsValidTag(tag) {
		return oci.ErrorSeq[oci.Descriptor](fmt.Errorf("invalid repository or tag: %w", oci.ErrNameInvalid))
	}
	q := make(url.Values)
	var limit *int
	if params != nil {
		limit = params.Limit
		if !params.Before.IsZero() {
			q.Set("before", params.Before.Format(time.RFC3339Nano))
		}
		if !params.Since.IsZero() {
			q.Set("since", params.Since.Format(time.RFC3339Nano))
		}
		if params.Digest != "" {
			if err := params.Digest.Validate(); err != nil {
				return oci.ErrorSeq[oci.Descriptor](fmt.Errorf("invalid tag history digest: %w", err))
			}
			q.Set("digest", params.Digest.String())
		}
	}
	if limit != nil {
		if *limit < 0 {
			return oci.ErrorSeq[oci.Descriptor](fmt.Errorf("tag history limit must not be negative"))
		}
		q.Set("n", fmt.Sprint(*limit))
	}
	u := "/v2/" + repo + "/_oci/tag-history/" + url.PathEscape(tag)
	if len(q) != 0 {
		u += "?" + q.Encode()
	}
	seq := pager(ctx, c, pageRequest{
		URL:   u,
		Scope: pullScope(repo),
		Limit: -1, // A Link, not a short page, determines whether more history exists.
	}, false, func(resp *http.Response) ([]oci.Descriptor, error) {
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("tag history endpoint unavailable: %w: %w", oci.ErrUnsupported, makeError(resp))
		}
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("reading tag history response: %w", err)
		}
		var index oci.IndexOrManifest
		if err := json.Unmarshal(data, &index); err != nil {
			return nil, fmt.Errorf("decoding tag history response: %w", err)
		}
		if index.SchemaVersion != 2 || index.MediaType != oci.MediaTypeImageIndex {
			return nil, fmt.Errorf("tag history response is not an OCI index")
		}
		if err := index.Validate(); err != nil {
			return nil, fmt.Errorf("invalid tag history response: %w", err)
		}
		for _, desc := range index.Manifests {
			if _, err := time.Parse(time.RFC3339Nano, desc.Annotations[oci.TagHistoryTimestampAnnotation]); err != nil {
				return nil, fmt.Errorf("invalid tag history event timestamp: %w", err)
			}
			switch desc.Annotations[oci.TagHistoryEventAnnotation] {
			case oci.TagHistoryEventCreated, oci.TagHistoryEventDeleted:
			default:
				return nil, fmt.Errorf("invalid tag history event type %q", desc.Annotations[oci.TagHistoryEventAnnotation])
			}
		}
		return index.Manifests, nil
	}, http.StatusOK, http.StatusNotFound)
	if limit != nil && *limit > 0 {
		return oci.LimitIter(seq, *limit)
	}
	return seq
}
