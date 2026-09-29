// Package conformance runs the OCI distribution conformance suite against registries.
// It is intended for integration tests and requires Docker.
package conformance

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/ociserver"
)

const conformanceImage = "ohseeeye-oci-conformance:integration"

//go:embed assets/Dockerfile assets/oci-conformance.yaml
var assets embed.FS

// Run serves backend over HTTP and runs the pinned upstream suite against it.
// name must contain lowercase letters, digits, dots, underscores, or hyphens,
// beginning with a letter or digit. Reports are retained in results/name under
// the calling test's working directory, in a unique directory for each run.
// Call Run from an integration-tagged test. Short mode skips the suite.
func Run(t *testing.T, name string, backend oci.Registry) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping OCI conformance tests in short mode")
	}
	if err := validateName(name); err != nil {
		t.Fatal(err)
	}
	assetsDir := t.TempDir()
	if err := writeAssets(assetsDir); err != nil {
		t.Fatalf("preparing conformance assets: %v", err)
	}
	requireDocker(t, assetsDir)
	buildConformanceImage(t, assetsDir)
	backendName := name

	handler, err := ociserver.New(backend, nil)
	if err != nil {
		t.Fatalf("creating OCI server: %v", err)
	}
	// Docker must reach this test server through the host gateway.
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(t.Context(), "tcp4", "0.0.0.0:0") // #nosec G102 -- integration-only server reachable from Docker
	if err != nil {
		t.Fatalf("listening for OCI server: %v", err)
	}
	httpServer := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpServer.Serve(listener)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			t.Errorf("shutting down OCI server: %v", err)
			_ = httpServer.Close()
		}
		select {
		case err := <-serveErr:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("serving OCI registry: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("timed out waiting for OCI server to stop")
		}
	})

	port := listener.Addr().(*net.TCPAddr).Port
	waitForRegistry(t, fmt.Sprintf("http://127.0.0.1:%d/v2/", port))

	resultsDir, err := createResultsDir(backendName)
	if err != nil {
		t.Fatalf("creating conformance results directory: %v", err)
	}
	t.Logf("OCI conformance reports: %s", resultsDir)

	runID := strconv.FormatInt(time.Now().UnixNano(), 36)
	args := []string{
		"run", "--rm",
		"--add-host=host.docker.internal:host-gateway",
		"-v", filepath.Join(assetsDir, "oci-conformance.yaml") + ":/work/oci-conformance.yaml:ro",
		"-v", resultsDir + ":/results",
		"-e", fmt.Sprintf("OCI_REGISTRY=host.docker.internal:%d", port),
		"-e", "OCI_REPO1=conformance/" + backendName + "/" + runID + "/repo1",
		"-e", "OCI_REPO2=conformance/" + backendName + "/" + runID + "/repo2",
	}
	if currentUser, err := user.Current(); err == nil && currentUser.Uid != "" && currentUser.Gid != "" {
		args = append(args, "--user", currentUser.Uid+":"+currentUser.Gid)
	}
	args = append(args, conformanceImage)

	runCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	output, err := runCommand(runCtx, assetsDir, "docker", args...)
	t.Logf("OCI conformance output for %s:\n%s", backendName, output)
	if err != nil {
		t.Fatalf("OCI conformance failed for %s: %v; reports: %s", backendName, err, resultsDir)
	}
}

func validateName(name string) error {
	for i, ch := range name {
		if ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' {
			continue
		}
		if i > 0 && (ch == '.' || ch == '_' || ch == '-') {
			continue
		}
		return fmt.Errorf("invalid conformance backend name %q", name)
	}
	if name == "" {
		return fmt.Errorf("conformance backend name must not be empty")
	}
	return nil
}

func writeAssets(dir string) error {
	for _, name := range []string{"Dockerfile", "oci-conformance.yaml"} {
		data, err := assets.ReadFile("assets/" + name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func createResultsDir(name string) (string, error) {
	if err := validateName(name); err != nil {
		return "", err
	}
	base, err := filepath.Abs(filepath.Join("results", name))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(base, 0o750); err != nil {
		return "", err
	}
	return os.MkdirTemp(base, "run-")
}

func requireDocker(t *testing.T, root string) {
	t.Helper()
	if output, err := runCommand(context.Background(), root, "docker", "version"); err != nil {
		t.Fatalf("Docker is required for conformance tests: %v\n%s", err, output)
	}
}

func buildConformanceImage(t *testing.T, root string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	output, err := runCommand(ctx, root, "docker", "build",
		"-f", "Dockerfile",
		"-t", conformanceImage,
		".",
	)
	if err != nil {
		t.Fatalf("building OCI conformance image: %v\n%s", err, output)
	}
}

func waitForRegistry(t *testing.T, registryURL string) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, registryURL, nil)
	if err != nil {
		t.Fatalf("creating registry readiness request: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("status %s", resp.Status)
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for registry %s: %v", registryURL, lastErr)
}

func runCommand(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(output), nil
}
