package ociauth

import (
	"testing"

	"github.com/ohseeeye/oci/pkg/dockerhub"
	"github.com/stretchr/testify/require"
)

func TestDockerWrapperCredentialLookup(t *testing.T) {
	for _, test := range []struct {
		name    string
		host    string
		entries map[string]ConfigEntry
		want    ConfigEntry
		calls   []string
	}{
		{
			name: "exact host takes precedence",
			host: dockerhub.RegistryHost,
			entries: map[string]ConfigEntry{
				dockerhub.RegistryHost:  {Username: "exact"},
				dockerhub.ReferenceHost: {Username: "fallback"},
			},
			want:  ConfigEntry{Username: "exact"},
			calls: []string{dockerhub.RegistryHost},
		},
		{
			name: "aliases searched in priority order",
			host: "registry.hub.docker.com",
			entries: map[string]ConfigEntry{
				"index.docker.io":      {Username: "index"},
				dockerhub.RegistryHost: {Username: "registry"},
			},
			want:  ConfigEntry{Username: "index"},
			calls: []string{"registry.hub.docker.com", dockerhub.ReferenceHost, "index.docker.io"},
		},
		{
			name:    "unrelated registry has no fallback",
			host:    "example.com",
			entries: map[string]ConfigEntry{dockerhub.ReferenceHost: {Username: "hub"}},
			calls:   []string{"example.com"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls []string
			config := configFunc(func(host string) (ConfigEntry, error) {
				calls = append(calls, host)
				return test.entries[host], nil
			})
			entry, err := (dockerWrapper{Config: config}).EntryForRegistry(test.host)
			require.NoError(t, err)
			require.Equal(t, test.want, entry)
			require.Equal(t, test.calls, calls)
		})
	}
}
