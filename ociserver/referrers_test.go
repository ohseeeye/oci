package ociserver

import (
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/stretchr/testify/require"
)

func TestMarshalIndexResponseFieldOrder(t *testing.T) {
	for _, descriptors := range [][]oci.Descriptor{
		nil,
		{},
		{{MediaType: oci.MediaTypeImageManifest, Digest: ocidigest.FromBytes([]byte("manifest")), Size: 8}},
	} {
		data, err := marshalIndexResponse(descriptors)
		require.NoError(t, err)
		children, err := json.Marshal(descriptors)
		require.NoError(t, err)
		require.Equal(t, `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":`+string(children)+`}`, string(data))
	}
}

func TestGeneratedIndexResponseFieldOrder(t *testing.T) {
	descriptors := []oci.Descriptor{{
		MediaType: oci.MediaTypeImageManifest,
		Digest:    ocidigest.FromBytes([]byte("manifest")),
		Size:      8,
	}}
	backend := &tagHistoryBackend{
		Funcs: &oci.Funcs{
			Referrers_: func(context.Context, string, oci.Digest, *oci.ReferrersParameters) iter.Seq2[oci.Descriptor, error] {
				return oci.SliceSeq(descriptors)
			},
		},
		list: func(context.Context, string, string, *oci.TagHistoryParameters) iter.Seq2[oci.Descriptor, error] {
			return oci.SliceSeq(descriptors)
		},
	}
	srv, err := New(backend, nil)
	require.NoError(t, err)
	want, err := marshalIndexResponse(descriptors)
	require.NoError(t, err)
	for _, target := range []string{
		"/v2/example/referrers/" + descriptors[0].Digest.String(),
		"/v2/example/_oci/tag-history/latest",
	} {
		t.Run(target, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
			require.Equal(t, http.StatusOK, rec.Code)
			require.Equal(t, oci.MediaTypeImageIndex, rec.Header().Get("Content-Type"))
			require.Equal(t, string(want), rec.Body.String())
		})
	}
}
