//go:build integration

package ocimiddleware_test

import (
	"testing"

	"github.com/ohseeeye/oci/internal/conformance"
	"github.com/ohseeeye/oci/ocimem"
	"github.com/ohseeeye/oci/ocimiddleware"
)

func TestOCIConformance(t *testing.T) {
	r, err := ocimiddleware.Cache(ocimem.New(), sparse(), &ocimiddleware.CacheOptions{CacheTags: true})
	if err != nil {
		t.Fatal(err)
	}
	conformance.Run(t, "ocimiddleware-cache", r)
}
