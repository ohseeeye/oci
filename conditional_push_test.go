package oci_test

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/ocilayout"
	"github.com/ohseeeye/oci/ocimem"
	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/stretchr/testify/require"
)

func conditionalIndex(version string) []byte {
	return []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[],"annotations":{"version":%q}}`, oci.MediaTypeImageIndex, version))
}

func TestConditionalPushBackends(t *testing.T) {
	for _, factory := range []struct {
		name string
		new  func(*testing.T) oci.Registry
	}{
		{"memory", func(*testing.T) oci.Registry { return ocimem.New() }},
		{"layout", func(t *testing.T) oci.Registry {
			r, err := ocilayout.New(t.TempDir(), nil)
			require.NoError(t, err)
			return r
		}},
		{"per repository layout", func(t *testing.T) oci.Registry {
			r, err := ocilayout.NewPerRepository(t.TempDir(), nil)
			require.NoError(t, err)
			return r
		}},
	} {
		t.Run(factory.name, func(t *testing.T) {
			r := factory.new(t)
			ctx := t.Context()
			first := conditionalIndex("first")
			old, err := r.PushManifest(ctx, "example/app", first, oci.MediaTypeImageIndex, &oci.PushManifestParameters{Tags: []string{"latest"}, Digest: ocidigest.SHA512.FromBytes(first)})
			require.NoError(t, err)
			second := conditionalIndex("second")
			for _, tc := range []struct {
				condition string
				tags      []string
				want      error
			}{
				{`"stale"`, []string{"latest"}, oci.ErrPreconditionFailed},
				{"W/" + strconv.Quote(old.Digest.String()), []string{"latest"}, oci.ErrPreconditionFailed},
				{"*", []string{"missing"}, oci.ErrPreconditionFailed},
				{old.Digest.String(), []string{"latest"}, oci.ErrManifestInvalid},
				{"*", nil, oci.ErrManifestInvalid},
				{"*", []string{"latest", "extra"}, oci.ErrManifestInvalid},
			} {
				_, err := r.PushManifest(ctx, "example/app", second, oci.MediaTypeImageIndex, &oci.PushManifestParameters{Tags: tc.tags, IfMatch: tc.condition})
				require.ErrorIs(t, err, tc.want)
			}
			current, err := r.ResolveTag(ctx, "example/app", "latest")
			require.NoError(t, err)
			require.Equal(t, old.Digest, current.Digest)
			_, err = r.ResolveManifest(ctx, "example/app", ocidigest.FromBytes(second))
			require.ErrorIs(t, err, oci.ErrManifestUnknown, "failed conditions must not publish the new manifest")
			history, ok := r.(oci.TagHistory)
			require.True(t, ok)
			entries, err := oci.All(history.TagHistory(ctx, "example/app", "latest", nil))
			require.NoError(t, err)
			require.Len(t, entries, 1, "failed conditions must not append history")
			_, err = r.PushManifest(ctx, "absent/app", second, oci.MediaTypeImageIndex, &oci.PushManifestParameters{Tags: []string{"latest"}, IfMatch: "*"})
			require.ErrorIs(t, err, oci.ErrPreconditionFailed)
			next, err := r.PushManifest(ctx, "example/app", second, oci.MediaTypeImageIndex, &oci.PushManifestParameters{Tags: []string{"latest"}, IfMatch: `"other", ` + strconv.Quote(old.Digest.String())})
			require.NoError(t, err)
			entries, err = oci.All(history.TagHistory(ctx, "example/app", "latest", nil))
			require.NoError(t, err)
			require.Len(t, entries, 2)
			require.Equal(t, next.Digest, entries[0].Digest)
			_, err = r.PushManifest(ctx, "example/app", conditionalIndex("third"), oci.MediaTypeImageIndex, &oci.PushManifestParameters{Tags: []string{"latest"}, IfMatch: "*"})
			require.NoError(t, err)

			t.Run("concurrent writers", func(t *testing.T) {
				initial, err := r.ResolveTag(ctx, "example/app", "latest")
				require.NoError(t, err)
				start := make(chan struct{})
				errs := make(chan error, 2)
				var wg sync.WaitGroup
				for i := range 2 {
					wg.Go(func() {
						<-start
						_, err := r.PushManifest(ctx, "example/app", conditionalIndex(fmt.Sprint(i)), oci.MediaTypeImageIndex, &oci.PushManifestParameters{Tags: []string{"latest"}, IfMatch: strconv.Quote(initial.Digest.String())})
						errs <- err
					})
				}
				close(start)
				wg.Wait()
				close(errs)
				succeeded, rejected := 0, 0
				for err := range errs {
					if err == nil {
						succeeded++
					} else if errors.Is(err, oci.ErrPreconditionFailed) {
						rejected++
					} else {
						t.Fatalf("unexpected push error: %v", err)
					}
				}
				require.Equal(t, 1, succeeded)
				require.Equal(t, 1, rejected)
				entries, err := oci.All(history.TagHistory(ctx, "example/app", "latest", nil))
				require.NoError(t, err)
				require.Len(t, entries, 4, "only the winning writer adds history")
			})
		})
	}
}
