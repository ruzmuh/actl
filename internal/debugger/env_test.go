package debugger

import (
	"testing"

	"github.com/nektos/act/pkg/model"
)

const envWorkflow = "../../testdata/workflows/environments.yml"

func TestLayer(t *testing.T) {
	// Later maps win key-by-key; the result is a fresh, non-nil map.
	got := layer(
		map[string]string{"A": "base", "B": "base"},
		map[string]string{"B": "overlay", "C": "overlay"},
		map[string]string{"C": "cli"},
	)
	want := map[string]string{"A": "base", "B": "overlay", "C": "cli"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("layer[%q] = %q, want %q (flat < overlay < CLI)", k, got[k], v)
		}
	}
	if got == nil {
		t.Error("layer should return a non-nil map")
	}
}

func TestBuildPlatforms(t *testing.T) {
	p := buildPlatforms(Options{
		Image:  "default-img",
		Images: map[string]string{"ubuntu-22.04": "custom-img", "Self-Hosted": "shimg"},
	})
	if p["ubuntu-latest"] != "default-img" {
		t.Errorf("ubuntu-latest = %q, want the -image default", p["ubuntu-latest"])
	}
	if p["ubuntu-22.04"] != "custom-img" {
		t.Errorf("ubuntu-22.04 = %q, want the Images override", p["ubuntu-22.04"])
	}
	if p["self-hosted"] != "shimg" {
		t.Errorf("self-hosted = %q, want lowercased Images key", p["self-hosted"])
	}
}

func TestResolveBreakpoints(t *testing.T) {
	steps := []*model.Step{{Name: "first"}, {Name: "Run tests"}, {Name: "last"}}
	got, err := resolveBreakpoints(steps, []int{0}, []string{"Run tests"})
	if err != nil {
		t.Fatal(err)
	}
	if !got[0] || !got[1] || got[2] {
		t.Errorf("breakpoints = %v, want {0,1}", got)
	}
	// An unknown name is an error listing the available steps.
	if _, err := resolveBreakpoints(steps, nil, []string{"nope"}); err == nil {
		t.Error("want error for an unknown breakpoint name, got nil")
	}
}

func TestList(t *testing.T) {
	l, err := List(Options{WorkflowPath: envWorkflow})
	if err != nil {
		t.Fatal(err)
	}
	jobs := map[string]JobListing{}
	for _, j := range l.Jobs {
		jobs[j.ID] = j
	}
	deploy, ok := jobs["deploy"]
	if !ok || deploy.Environment != "production" {
		t.Errorf("deploy environment = %q (ok=%v), want production", deploy.Environment, ok)
	}
	if len(deploy.Matrix) != 0 {
		t.Errorf("deploy matrix = %v, want none", deploy.Matrix)
	}
	if len(deploy.Steps) != 1 || deploy.Steps[0].Label != "Deploy" || deploy.Steps[0].Kind != "run" {
		t.Errorf("deploy steps = %+v", deploy.Steps)
	}
	build, ok := jobs["build"]
	if !ok || build.Environment != "staging" {
		t.Errorf("build environment = %q, want staging", build.Environment)
	}
	if len(build.Matrix) != 2 {
		t.Errorf("build matrix = %v, want 2 combinations", build.Matrix)
	}
}

// TestEnvOverlay exercises the end-to-end overlay through New (needs Docker): the
// debugged job targets `environment: production`, so its overlay's secrets/vars are
// merged in and surfaced via EnvSummary/ConfigSummary.
func TestEnvOverlay(t *testing.T) {
	s, err := New(Options{
		WorkflowPath: envWorkflow,
		JobID:        "deploy",
		Workdir:      t.TempDir(),
		Secrets:      map[string]string{"FLAT": "1"},
		Environments: map[string]EnvOverlay{
			"production": {
				Secrets: map[string]string{"DEPLOY_KEY": "x"},
				Vars:    map[string]string{"REGION": "us-west-2"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	env := s.EnvSummary()
	if env.Name != "production" || env.Secrets != 1 || env.Vars != 1 {
		t.Errorf("EnvSummary = %+v, want {production 1 1}", env)
	}
	if !contains(s.ConfigSummary().Secrets, "DEPLOY_KEY") || !contains(s.ConfigSummary().Secrets, "FLAT") {
		t.Errorf("ConfigSummary secrets = %v, want both FLAT and DEPLOY_KEY", s.ConfigSummary().Secrets)
	}
	if !contains(s.ConfigSummary().Vars, "REGION") {
		t.Errorf("ConfigSummary vars = %v, want REGION from overlay", s.ConfigSummary().Vars)
	}
}

// TestEnvOverlayNoneTargeted: a job with no environment yields an empty EnvSummary.
func TestEnvOverlayNoneTargeted(t *testing.T) {
	s, err := New(Options{WorkflowPath: sampleWorkflow, Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if name := s.EnvSummary().Name; name != "" {
		t.Errorf("EnvSummary.Name = %q, want empty (no environment)", name)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
