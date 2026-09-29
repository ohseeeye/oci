package conformance

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateName(t *testing.T) {
	for _, name := range []string{"ocimem", "ocisqlite", "backend-1", "backend.v2", "backend_test"} {
		if err := validateName(name); err != nil {
			t.Errorf("validateName(%q): %v", name, err)
		}
	}
	for _, name := range []string{"", ".", "..", "../escape", "/absolute", "a/b", "a\\b", "UPPER", "white space"} {
		if err := validateName(name); err == nil {
			t.Errorf("validateName(%q) unexpectedly succeeded", name)
		}
	}
}

func TestWriteAssets(t *testing.T) {
	// Asset lookup does not depend on a checkout-relative working directory.
	t.Chdir(t.TempDir())
	dir := t.TempDir()
	if err := writeAssets(dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Dockerfile", "oci-conformance.yaml"} {
		want, err := assets.ReadFile("assets/" + name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("extracted %s does not match embedded asset", name)
		}
	}
}

func TestCreateResultsDir(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	first, err := createResultsDir("ocisqlite")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(first, "report.txt")
	if err := os.WriteFile(marker, []byte("previous report"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := createResultsDir("ocisqlite")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("runs reused a report directory")
	}
	if !filepath.IsAbs(first) || filepath.Dir(first) != filepath.Join(dir, "results", "ocisqlite") {
		t.Fatalf("unexpected report directory: %s", first)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("previous report was removed: %v", err)
	}
	if _, err := createResultsDir("../escape"); err == nil {
		t.Fatal("unsafe backend name unexpectedly accepted")
	}
}
