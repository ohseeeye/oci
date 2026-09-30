//go:build integration

package ocisqlite_test

import (
	"testing"

	"github.com/ohseeeye/oci/internal/conformance"
	"github.com/ohseeeye/oci/ocisqlite"
)

func TestOCIConformance(t *testing.T) {
	r, err := ocisqlite.New(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	conformance.Run(t, "ocisqlite", r)
}
