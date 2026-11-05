package ocimiddleware_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/ocimiddleware"
	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/stretchr/testify/require"
)

func TestImmutablePushManifestPreservesConfirmedTags(t *testing.T) {
	data := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	want := oci.TaggedDescriptor{
		Descriptor: oci.Descriptor{Digest: ocidigest.FromBytes(data), Size: int64(len(data)), MediaType: oci.MediaTypeImageIndex},
		Tags:       []string{"created"},
	}
	for _, tc := range []struct {
		name string
		err  error
	}{{"partial success", nil}, {"partial failure", errors.New("later tag failed")}} {
		t.Run(tc.name, func(t *testing.T) {
			pushed := false
			backend := &oci.Funcs{
				ResolveTag_: func(_ context.Context, _, tag string) (oci.Descriptor, error) {
					if pushed && tag == "created" {
						return want.Descriptor, nil
					}
					return oci.Descriptor{}, oci.ErrManifestUnknown
				},
				PushManifest_: func(context.Context, string, []byte, string, *oci.PushManifestParameters) (oci.TaggedDescriptor, error) {
					pushed = true
					return want, tc.err
				},
			}
			got, err := ocimiddleware.Immutable(backend).PushManifest(t.Context(), "app", data, want.MediaType, &oci.PushManifestParameters{Tags: []string{"created", "missing"}})
			require.ErrorIs(t, err, tc.err)
			require.Equal(t, want, got)
		})
	}
}
