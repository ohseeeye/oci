//go:build integration

package ocimem_test

import (
	"testing"

	"github.com/ohseeeye/oci/internal/conformance"
	"github.com/ohseeeye/oci/ocimem"
)

func TestOCIConformance(t *testing.T) {
	conformance.Run(t, "ocimem", ocimem.New())
}
