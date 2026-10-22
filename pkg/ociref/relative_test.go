package ociref

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ohseeeye/oci/pkg/ocidigest"
	"github.com/stretchr/testify/require"
)

func TestParseRelative(t *testing.T) {
	digest := ocidigest.FromBytes([]byte("manifest"))
	sha512 := ocidigest.SHA512.FromBytes([]byte("manifest"))
	for _, test := range []struct {
		input string
		want  Reference
	}{
		{"ubuntu", Reference{Repository: "ubuntu"}},
		{"library/ubuntu", Reference{Repository: "library/ubuntu"}},
		{"team/app:v1", Reference{Repository: "team/app", Tag: "v1"}},
		{"team/app:Release_1.2-rc", Reference{Repository: "team/app", Tag: "Release_1.2-rc"}},
		{"team/app@" + digest.String(), Reference{Repository: "team/app", Digest: digest}},
		{"team/app:v1@" + digest.String(), Reference{Repository: "team/app", Tag: "v1", Digest: digest}},
		{"team/app@" + sha512.String(), Reference{Repository: "team/app", Digest: sha512}},
		{"example.com/team/app:v1", Reference{Repository: "example.com/team/app", Tag: "v1"}},
		{"example.com/team/app:v1@" + digest.String(), Reference{Repository: "example.com/team/app", Tag: "v1", Digest: digest}},
		{"docker.io/ubuntu", Reference{Repository: "docker.io/ubuntu"}},
		{"index.docker.io/ubuntu", Reference{Repository: "index.docker.io/ubuntu"}},
		{"localhost/app:v1", Reference{Repository: "localhost/app", Tag: "v1"}},
		{"192.168.1.1/app", Reference{Repository: "192.168.1.1/app"}},
		{"example.com:5000", Reference{Repository: "example.com", Tag: "5000"}},
		{"name_with__separators/app--name:v1", Reference{Repository: "name_with__separators/app--name", Tag: "v1"}},
		{strings.Repeat("a", 255), Reference{Repository: strings.Repeat("a", 255)}},
		{"app:" + strings.Repeat("a", 128), Reference{Repository: "app", Tag: strings.Repeat("a", 128)}},
	} {
		t.Run(test.input, func(t *testing.T) {
			ref, err := ParseRelative(test.input)
			require.NoError(t, err)
			require.Equal(t, test.want, ref)
			require.Empty(t, ref.Host)
			require.Equal(t, test.input, ref.String())
		})
	}
}

func TestParseRelativeRejectsInvalidReferences(t *testing.T) {
	for _, input := range []string{
		"", "/app", "app/", "team//app", ".", "..", "team/../app",
		"App", "team/App", "EXAMPLE.com/app", "https://example.com/app:v1",
		"example.com:5000/team/app:v1", "localhost:5000/app", "[::1]/app",
		"app:", "app:.tag", "app:-tag", "app:tag:extra", "app:tag/extra", "app:tag with space",
		"app@", "@sha256:" + strings.Repeat("a", 64), "app@sha256:bad",
		"app@sha256:" + strings.Repeat("a", 64) + "@extra",
		"app:tag@sha256:" + strings.Repeat("a", 63),
		strings.Repeat("a", 256), "app:" + strings.Repeat("a", 129),
	} {
		t.Run(input, func(t *testing.T) {
			ref, err := ParseRelative(input)
			require.Error(t, err)
			require.Equal(t, Reference{}, ref, "invalid references must not return partially parsed fields")
		})
	}
}

func TestParseRelativeCountsEntireRepositoryLength(t *testing.T) {
	// The host-like component belongs to the repository in relative mode and
	// must count toward the 255-byte name limit, unlike Docker-style parsing.
	input := "example.com/" + strings.Repeat("a", 244)
	_, err := ParseRelative(input)
	require.ErrorContains(t, err, "repository name too long")
	ref, err := ParseRelativeDocker(input)
	require.NoError(t, err)
	require.Equal(t, "example.com", ref.Host)
	require.Len(t, ref.Repository, 244)
}

func TestParseRelativeDockerChecksNormalizedRepositoryLength(t *testing.T) {
	// Adding library/ must still enforce the final length limit.
	_, err := ParseRelativeDocker(strings.Repeat("a", 255))
	require.ErrorContains(t, err, "repository name too long")
	ref, err := ParseRelativeDocker(strings.Repeat("a", 247))
	require.NoError(t, err)
	require.Equal(t, "docker.io", ref.Host)
	require.Len(t, ref.Repository, 255)
}

func ExampleParseRelative() {
	ref, err := ParseRelative("example.com/team/app:v1")
	if err != nil {
		panic(err)
	}
	fmt.Printf("host=%q repository=%q tag=%q\n", ref.Host, ref.Repository, ref.Tag)
	// Output: host="" repository="example.com/team/app" tag="v1"
}

func ExampleParseRelativeDocker() {
	ref, err := ParseRelativeDocker("ubuntu:24.04")
	if err != nil {
		panic(err)
	}
	fmt.Println(ref.String())
	// Output: docker.io/library/ubuntu:24.04
}
