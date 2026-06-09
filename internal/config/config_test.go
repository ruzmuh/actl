package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write drops content into a temp .actl.yml and returns its path.
func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".actl.yml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad(t *testing.T) {
	path := write(t, `
workflow: .github/workflows/deploy.yml
job: deploy
event: workflow_dispatch
with-deps: true
matrix:
  os: ubuntu-latest
images:
  ubuntu-latest: catthehacker/ubuntu:act-latest
breakpoints: [3, "Run tests"]
secret-file: .secrets
vars:
  REGION: us-east-1
env:
  LOG_LEVEL: debug
environments:
  production:
    secret-file: .secrets.prod
    vars: { REGION: us-west-2 }
inputs:
  version: "1.2.3"
needs:
  build:
    result: success
    outputs: { artifact: out.tar }
`)
	c, err := Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if c.Job != "deploy" || c.Event != "workflow_dispatch" {
		t.Errorf("job/event = %q/%q", c.Job, c.Event)
	}
	if c.WithDeps == nil || !*c.WithDeps {
		t.Errorf("with-deps = %v, want true", c.WithDeps)
	}
	if c.Matrix["os"] != "ubuntu-latest" {
		t.Errorf("matrix os = %q", c.Matrix["os"])
	}
	if c.Vars["REGION"] != "us-east-1" || c.Env["LOG_LEVEL"] != "debug" {
		t.Errorf("inline vars/env not parsed: %v %v", c.Vars, c.Env)
	}
	if c.SecretFile != ".secrets" {
		t.Errorf("secret-file = %q", c.SecretFile)
	}
	prod, ok := c.Environments["production"]
	if !ok || prod.SecretFile != ".secrets.prod" || prod.Vars["REGION"] != "us-west-2" {
		t.Errorf("environment overlay not parsed: %+v", c.Environments)
	}
	// breakpoints: one index, one name.
	if len(c.Breakpoints) != 2 {
		t.Fatalf("breakpoints = %+v", c.Breakpoints)
	}
	if c.Breakpoints[0].Index != 3 || c.Breakpoints[0].Name != "" {
		t.Errorf("bp[0] = %+v, want index 3", c.Breakpoints[0])
	}
	if c.Breakpoints[1].Index != -1 || c.Breakpoints[1].Name != "Run tests" {
		t.Errorf("bp[1] = %+v, want name", c.Breakpoints[1])
	}
	if c.Needs["build"].Result != "success" || c.Needs["build"].Outputs["artifact"] != "out.tar" {
		t.Errorf("needs not parsed: %+v", c.Needs)
	}
}

func TestLoadRejectsInlineSecrets(t *testing.T) {
	path := write(t, "secrets:\n  TOKEN: hunter2\n")
	_, err := Load(path, true)
	if err == nil || !strings.Contains(err.Error(), "secret-file") {
		t.Fatalf("want inline-secrets error pointing at secret-file, got %v", err)
	}
}

func TestLoadRejectsInlineEnvSecrets(t *testing.T) {
	path := write(t, "environments:\n  production:\n    secrets:\n      TOKEN: hunter2\n")
	_, err := Load(path, true)
	if err == nil || !strings.Contains(err.Error(), "environments.production.secrets") {
		t.Fatalf("want per-env inline-secrets error, got %v", err)
	}
}

func TestLoadRejectsUnknownKey(t *testing.T) {
	path := write(t, "jub: deploy\n") // typo for job:
	if _, err := Load(path, true); err == nil {
		t.Fatal("want an error on an unknown key (KnownFields), got nil")
	}
}

func TestLoadMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), ".actl.yml")
	// Default path (not explicit): absent file → (nil, nil), proceed on flags.
	if c, err := Load(missing, false); err != nil || c != nil {
		t.Errorf("default missing: got %v, %v; want nil, nil", c, err)
	}
	// Explicitly pointed at a missing file → error.
	if _, err := Load(missing, true); err == nil {
		t.Error("explicit missing: want error, got nil")
	}
}
