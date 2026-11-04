package ociunify_test

import (
	"testing"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/ocimem"
	"github.com/ohseeeye/oci/ociunify"
	"github.com/stretchr/testify/require"
)

func TestTagHistoryUnsupportedWithCapableBackends(t *testing.T) {
	r := ociunify.New(ocimem.New(), ocimem.New(), nil)
	history, ok := r.(oci.TagHistory)
	require.True(t, ok, "unified registry must expose TagHistory")
	zero := 0
	for _, params := range []*oci.TagHistoryParameters{nil, {Limit: &zero}} {
		_, err := oci.All(history.TagHistory(t.Context(), "app", "release", params))
		require.ErrorIs(t, err, oci.ErrUnsupported)
	}
}
