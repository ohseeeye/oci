//go:build integration

package conformance

import (
	"testing"

	"github.com/ohseeeye/oci"
	conformancetest "github.com/ohseeeye/oci/internal/conformance"
	"github.com/ohseeeye/oci/ocilayout"
	"github.com/ohseeeye/oci/ocimem"
)

func TestOCIConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping OCI conformance tests in short mode")
	}
	backends := []struct {
		name string
		new  func(*testing.T) oci.Registry
	}{
		{
			name: "ocimem",
			new:  func(*testing.T) oci.Registry { return ocimem.New() },
		},
		{
			name: "ocilayout",
			new: func(t *testing.T) oci.Registry {
				r, err := ocilayout.New(t.TempDir(), nil)
				if err != nil {
					t.Fatalf("creating ocilayout backend: %v", err)
				}
				return r
			},
		},
	}
	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			conformancetest.Run(t, backend.name, backend.new(t))
		})
	}
}
