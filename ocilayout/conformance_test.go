//go:build integration

package ocilayout_test

import (
	"testing"

	"github.com/ohseeeye/oci/internal/conformance"
	"github.com/ohseeeye/oci/ocilayout"
)

func TestOCIConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping OCI conformance tests in short mode")
	}
	registry, err := ocilayout.New(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	conformance.Run(t, "ocilayout", registry)
}
