// Package dockerhub provides Docker Hub hostnames for reference normalization,
// registry connections, and credential lookup.
package dockerhub

import "slices"

const (
	// ReferenceHost is the canonical hostname used in Docker Hub image references.
	ReferenceHost = "docker.io"
	// RegistryHost is the hostname of Docker Hub's registry API.
	RegistryHost = "registry-1.docker.io"
)

var hosts = [...]string{
	ReferenceHost,
	"index.docker.io",
	RegistryHost,
	"registry.hub.docker.com",
}

// IsHost reports whether host is a recognized Docker Hub hostname.
// It matches exact hostnames; URLs, ports, and an empty host are not recognized.
func IsHost(host string) bool {
	return slices.Contains(hosts[:], host)
}

// Hosts returns recognized hostnames in credential lookup order.
// Each call returns a fresh slice that the caller can modify safely.
func Hosts() []string {
	return slices.Clone(hosts[:])
}
