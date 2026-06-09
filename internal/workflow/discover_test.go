package workflow

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDiscover(t *testing.T) {
	// A .github/workflows with two yaml flavors + a stray non-workflow file.
	dir := t.TempDir()
	wfDir := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ci.yml", "release.yaml", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(wfDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := Discover(dir)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := []string{
		filepath.Join(wfDir, "ci.yml"),
		filepath.Join(wfDir, "release.yaml"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Discover = %v, want %v (yml+yaml only, sorted)", got, want)
	}
}

func TestDiscoverMissingDir(t *testing.T) {
	// No .github/workflows at all → empty slice, no error (caller gives a
	// friendly "no workflow found" message).
	got, err := Discover(t.TempDir())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Discover on missing dir = %v, want empty", got)
	}
}
