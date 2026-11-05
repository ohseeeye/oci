package ociunify_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/ociunify"
	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/stretchr/testify/require"
)

func manifestPusher(result oci.TaggedDescriptor, err error) oci.Registry {
	return &oci.Funcs{PushManifest_: func(context.Context, string, []byte, string, *oci.PushManifestParameters) (oci.TaggedDescriptor, error) {
		return result, err
	}}
}

func TestPushManifestConfirmedTags(t *testing.T) {
	desc := oci.Descriptor{Digest: ocidigest.FromBytes([]byte("manifest")), Size: 8, MediaType: oci.MediaTypeImageIndex}
	failed := errors.New("second backend failed")
	left := oci.TaggedDescriptor{Descriptor: desc, Tags: []string{"left", "common", "common"}}
	for _, tc := range []struct {
		name     string
		right    oci.TaggedDescriptor
		err      error
		wantTags []string
	}{
		{"success", oci.TaggedDescriptor{Descriptor: desc, Tags: []string{"common", "right"}}, nil, []string{"common"}},
		{"partial failure", oci.TaggedDescriptor{Descriptor: desc, Tags: []string{"common"}}, failed, []string{"common"}},
		{"total failure", oci.TaggedDescriptor{}, failed, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := ociunify.New(manifestPusher(left, nil), manifestPusher(tc.right, tc.err), nil)
			got, err := r.PushManifest(t.Context(), "app", nil, desc.MediaType, nil)
			require.ErrorIs(t, err, tc.err)
			require.Equal(t, desc, got.Descriptor)
			require.Equal(t, tc.wantTags, got.Tags)
		})
	}
}

func TestPushManifestRejectsConflictingDigests(t *testing.T) {
	left := oci.TaggedDescriptor{Descriptor: oci.Descriptor{Digest: ocidigest.FromBytes([]byte("left"))}, Tags: []string{"latest"}}
	right := oci.TaggedDescriptor{Descriptor: oci.Descriptor{Digest: ocidigest.FromBytes([]byte("right"))}, Tags: []string{"latest"}}
	r := ociunify.New(manifestPusher(left, nil), manifestPusher(right, nil), nil)
	got, err := r.PushManifest(t.Context(), "app", nil, oci.MediaTypeImageIndex, nil)
	require.Error(t, err)
	require.Empty(t, got.Tags)
}
