package oci

import (
	"net/http"
	"testing"

	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/stretchr/testify/require"
)

func TestCheckManifestIfMatch(t *testing.T) {
	digest := ocidigest.FromBytes([]byte("current"))
	quoted := `"` + digest.String() + `"`
	for _, tc := range []struct {
		name, value string
		current     Digest
		want        error
	}{
		{"unconditional", "", "", nil},
		{"match", quoted, digest, nil},
		{"missing", quoted, "", ErrPreconditionFailed},
		{"stale", `"stale"`, digest, ErrPreconditionFailed},
		{"wildcard", "*", digest, nil},
		{"wildcard missing", "*", "", ErrPreconditionFailed},
		{"whitespace", "\t" + quoted + " ", digest, nil},
		{"list", `"other", ` + quoted, digest, nil},
		{"empty list members", ", ," + quoted + ",", digest, nil},
		{"weak", "W/" + quoted, digest, ErrPreconditionFailed},
		{"weak then strong", "W/" + quoted + ", " + quoted, digest, nil},
		{"quoted wildcard", `"*"`, digest, ErrPreconditionFailed},
		{"opaque commas", `"opaque,tag", ` + quoted, digest, nil},
		{"bare digest", digest.String(), digest, ErrManifestInvalid},
		{"blank", " ", digest, ErrManifestInvalid},
		{"only commas", ",,", digest, ErrManifestInvalid},
		{"unterminated", `"unfinished`, digest, ErrManifestInvalid},
		{"invalid tag character", `"bad tag"`, digest, ErrManifestInvalid},
		{"wildcard list", "*, " + quoted, digest, ErrManifestInvalid},
		{"invalid suffix after match", quoted + ", invalid", digest, ErrManifestInvalid},
		{"missing comma", quoted + " " + quoted, digest, ErrManifestInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckManifestIfMatch(tc.value, tc.current)
			if tc.want == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.want)
			}
		})
	}
}

func TestPreconditionFailedError(t *testing.T) {
	_, status := MarshalError(ErrPreconditionFailed)
	require.Equal(t, http.StatusPreconditionFailed, status)
	require.ErrorIs(t, NewHTTPError(nil, http.StatusPreconditionFailed, nil, nil), ErrPreconditionFailed)
}
