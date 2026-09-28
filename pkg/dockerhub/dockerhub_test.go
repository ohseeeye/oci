package dockerhub_test

import (
	"slices"
	"testing"

	"github.com/ohseeeye/oci/pkg/dockerhub"
)

func TestHosts(t *testing.T) {
	want := []string{"docker.io", "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com"}
	if got := dockerhub.Hosts(); !slices.Equal(got, want) {
		t.Fatalf("Hosts() = %v, want %v", got, want)
	}
	if dockerhub.ReferenceHost != want[0] || dockerhub.RegistryHost != want[2] {
		t.Fatal("canonical host constants do not match the expected hostnames")
	}
}

func TestIsHost(t *testing.T) {
	for _, host := range dockerhub.Hosts() {
		if !dockerhub.IsHost(host) {
			t.Errorf("IsHost(%q) = false", host)
		}
	}
	for _, host := range []string{"", "example.com", "https://docker.io", "docker.io:443", "DOCKER.IO", "docker.io.example.com"} {
		if dockerhub.IsHost(host) {
			t.Errorf("IsHost(%q) = true", host)
		}
	}
}

func TestHostsReturnsIndependentSlice(t *testing.T) {
	first, second := dockerhub.Hosts(), dockerhub.Hosts()
	first[0] = "example.com"
	if second[0] != dockerhub.ReferenceHost || dockerhub.Hosts()[0] != dockerhub.ReferenceHost {
		t.Fatal("mutating Hosts() changed another caller's hostnames")
	}
	if !dockerhub.IsHost(dockerhub.ReferenceHost) || dockerhub.IsHost("example.com") {
		t.Fatal("mutating Hosts() changed hostname membership")
	}
}
