package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, ".secrets")
	if err := os.WriteFile(file, []byte("TOKEN=fromfile\nKEEP=yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// File loads; a KEY=VALUE override wins over the file's value.
	got, err := loadConfig(file, true, []string{"TOKEN=override"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got["TOKEN"] != "override" {
		t.Errorf("TOKEN = %q, want override (flag beats file)", got["TOKEN"])
	}
	if got["KEEP"] != "yes" {
		t.Errorf("KEEP = %q, want yes (from file)", got["KEEP"])
	}
}

func TestLoadConfigMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	// Default path (not explicit): absent file is fine, overrides still apply.
	got, err := loadConfig(missing, false, []string{"A=1"})
	if err != nil || got["A"] != "1" {
		t.Errorf("default missing: got %v, err %v", got, err)
	}
	// Nothing at all → nil map, no error.
	if got, err := loadConfig(missing, false, nil); err != nil || got != nil {
		t.Errorf("empty: got %v, err %v; want nil,nil", got, err)
	}
	// Explicitly pointed at a missing file → error (likely a typo).
	if _, err := loadConfig(missing, true, nil); err == nil {
		t.Error("explicit missing: want error, got nil")
	}
}

func TestParseNeeds(t *testing.T) {
	in := []string{
		"build.outputs.image=repo/app:abc",
		"build.outputs.sha=abc123",
		"build.result=failure",
		"test.outputs.coverage=92",
	}
	got, err := parseNeeds(in)
	if err != nil {
		t.Fatal(err)
	}
	if got["build"].Result != "failure" {
		t.Errorf("build.result = %q, want failure", got["build"].Result)
	}
	if got["build"].Outputs["image"] != "repo/app:abc" || got["build"].Outputs["sha"] != "abc123" {
		t.Errorf("build.outputs = %v", got["build"].Outputs)
	}
	if got["test"].Outputs["coverage"] != "92" {
		t.Errorf("test.outputs.coverage = %q, want 92", got["test"].Outputs["coverage"])
	}
}

func TestParseNeedsErrors(t *testing.T) {
	bad := []string{
		"noequalsign",
		"build.bogus=1",    // neither result nor outputs
		"build.outputs=1",  // outputs without a name
		"build.result.x=1", // result takes no sub-path
	}
	for _, e := range bad {
		if _, err := parseNeeds([]string{e}); err == nil {
			t.Errorf("parseNeeds(%q): want error, got nil", e)
		}
	}
}
