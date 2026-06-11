package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ruzmuh/actl/internal/config"
)

func TestOrConfig(t *testing.T) {
	if got := orConfig(true, "flag", "cfg"); got != "flag" {
		t.Errorf("set flag should win: got %q", got)
	}
	if got := orConfig(false, "default", "cfg"); got != "cfg" {
		t.Errorf("unset flag → config: got %q", got)
	}
	if got := orConfig(false, "default", ""); got != "default" {
		t.Errorf("unset flag, no config → default: got %q", got)
	}
}

func TestBoolOrConfig(t *testing.T) {
	tru := true
	if got := boolOrConfig(true, true, &[]bool{false}[0]); !got {
		t.Error("set flag should win over config")
	}
	if got := boolOrConfig(false, false, &tru); !got {
		t.Error("unset flag → config value")
	}
	if got := boolOrConfig(false, false, nil); got {
		t.Error("unset flag, no config → flag default (false)")
	}
}

func TestIdentityConfig(t *testing.T) {
	tru := true
	cfg := &config.Config{Identity: config.Identity{
		GCP:   config.CloudIdentity{File: "gcp.json", Ambient: &tru},
		AWS:   config.CloudIdentity{File: "aws.env"},
		Azure: config.CloudIdentity{File: "azure.json"},
	}}
	if identityFile(cfg, "gcp") != "gcp.json" || identityFile(cfg, "aws") != "aws.env" || identityFile(cfg, "azure") != "azure.json" {
		t.Errorf("identityFile wrong: %q %q %q", identityFile(cfg, "gcp"), identityFile(cfg, "aws"), identityFile(cfg, "azure"))
	}
	if a := identityAmbient(cfg, "gcp"); a == nil || !*a {
		t.Errorf("identityAmbient(gcp) = %v, want true", a)
	}
	if identityAmbient(cfg, "aws") != nil {
		t.Error("identityAmbient(aws) should be nil when unset")
	}
	if identityFile(cfg, "bogus") != "" {
		t.Error("unknown cloud → empty file")
	}
}

func TestReadCredFiles(t *testing.T) {
	dir := t.TempDir()
	if got, err := readCredFile(""); got != "" || err != nil {
		t.Errorf("empty path → empty, got %q %v", got, err)
	}
	if _, err := readCredFile(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("a set-but-missing credential file should error")
	}
	keyPath := filepath.Join(dir, "key.json")
	if err := os.WriteFile(keyPath, []byte("  {\"client_email\":\"x\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := readCredFile(keyPath); got != `{"client_email":"x"}` {
		t.Errorf("readCredFile trimmed = %q", got)
	}

	awsPath := filepath.Join(dir, "aws.env")
	if err := os.WriteFile(awsPath, []byte("AWS_ACCESS_KEY_ID=AKIA\nAWS_SECRET_ACCESS_KEY=shh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, secret, err := readAWSKeys(awsPath)
	if err != nil || id != "AKIA" || secret != "shh" {
		t.Errorf("readAWSKeys = %q %q %v", id, secret, err)
	}
	if id, secret, _ := readAWSKeys(""); id != "" || secret != "" {
		t.Errorf("empty path → empty keys, got %q %q", id, secret)
	}
}

func TestResolveMatrix(t *testing.T) {
	// Config seeds, CLI overrides the same key.
	m := resolveMatrix(map[string]string{"os": "ubuntu-latest", "go": "1.22"}, []string{"os=macos-latest"})
	if !m["os"]["macos-latest"] || m["os"]["ubuntu-latest"] {
		t.Errorf("CLI -matrix should replace config's os: %v", m["os"])
	}
	if !m["go"]["1.22"] {
		t.Errorf("config matrix key should survive: %v", m["go"])
	}
	if resolveMatrix(nil, nil) != nil {
		t.Error("no matrix → nil")
	}
}

func TestResolveImages(t *testing.T) {
	im := resolveImages(map[string]string{"ubuntu-latest": "a", "ubuntu-22.04": "b"}, []string{"ubuntu-22.04=cli"})
	if im["ubuntu-latest"] != "a" || im["ubuntu-22.04"] != "cli" {
		t.Errorf("images = %v, want CLI -platform to win", im)
	}
	if resolveImages(nil, nil) != nil {
		t.Error("no images → nil")
	}
}

func TestSplitBreakpoints(t *testing.T) {
	idx, names := splitBreakpoints([]config.Breakpoint{{Index: 3}, {Index: -1, Name: "Run tests"}, {Index: 0}})
	if len(idx) != 2 || idx[0] != 3 || idx[1] != 0 {
		t.Errorf("indices = %v, want [3 0]", idx)
	}
	if len(names) != 1 || names[0] != "Run tests" {
		t.Errorf("names = %v, want [Run tests]", names)
	}
}

func TestResolveEnvironments(t *testing.T) {
	dir := t.TempDir()
	secrets := filepath.Join(dir, ".secrets.prod")
	if err := os.WriteFile(secrets, []byte("DEPLOY_KEY=xyz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envs, err := resolveEnvironments(map[string]config.EnvOverlay{
		"production": {SecretFile: secrets, Vars: map[string]string{"REGION": "us-west-2"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	prod := envs["production"]
	if prod.Secrets["DEPLOY_KEY"] != "xyz" {
		t.Errorf("secret-file not read into overlay: %v", prod.Secrets)
	}
	if prod.Vars["REGION"] != "us-west-2" {
		t.Errorf("overlay vars not passed: %v", prod.Vars)
	}

	// A missing per-environment secret-file is non-fatal (only the debugged job's
	// environment overlay is used; an absent file for another env must not fail the
	// run): the overlay just carries no secrets.
	envs, err = resolveEnvironments(map[string]config.EnvOverlay{
		"staging": {SecretFile: filepath.Join(dir, "nope"), Vars: map[string]string{"REGION": "eu"}},
	})
	if err != nil {
		t.Fatalf("missing env secret-file should be non-fatal, got %v", err)
	}
	if len(envs["staging"].Secrets) != 0 || envs["staging"].Vars["REGION"] != "eu" {
		t.Errorf("staging overlay = %+v, want no secrets + its vars", envs["staging"])
	}
}
