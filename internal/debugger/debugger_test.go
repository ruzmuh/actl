package debugger

import (
	"errors"
	"testing"

	"github.com/nektos/act/pkg/runner"
)

const sampleWorkflow = "../../testdata/workflows/sample.yml"

// barrier must satisfy the soft-fork StepBarrier signature. This line is the
// fork smoke guard: a bad rebase that drops the patch fails to compile here.
var _ runner.StepBarrier = (&Session{}).barrier

// TestNew exercises the act libraries + the fork wiring without Docker: it parses
// the workflow, builds a single-job plan, and constructs the runner with the
// StepBarrier hook installed.
func TestNew(t *testing.T) {
	s, err := New(Options{WorkflowPath: sampleWorkflow, Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if s.JobID() != "build" {
		t.Errorf("JobID = %q, want %q", s.JobID(), "build")
	}
	if n := len(s.Steps()); n != 2 {
		t.Fatalf("len(Steps) = %d, want 2", n)
	}
	if got := s.Steps()[0].String(); got != "first" {
		t.Errorf("step 1 = %q, want %q", got, "first")
	}
}

func TestInspectionNilWhileRunning(t *testing.T) {
	s, err := New(Options{WorkflowPath: sampleWorkflow, Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if s.Env() != nil {
		t.Errorf("Env() = %v, want nil before a pause", s.Env())
	}
	if s.ContainerName() != "" {
		t.Errorf("ContainerName() = %q, want empty before a pause", s.ContainerName())
	}
}

func TestSingleJobOnly(t *testing.T) {
	if _, err := New(Options{WorkflowPath: "testdata/two-jobs.yml", Workdir: t.TempDir()}); err == nil {
		t.Fatal("expected an error for a multi-job workflow, got nil")
	}
}

func TestGitContextNoiseFiltered(t *testing.T) {
	drop := []string{
		"path/tmp/actl-123not located inside a git repository",
		"unable to get git ref: repository does not exist",
		"unable to get git revision: repository does not exist",
	}
	for _, l := range drop {
		if !isGitContextNoise(l) {
			t.Errorf("expected dropped, kept: %q", l)
		}
	}
	keep := []string{"⭐ Run Main first", "step one", "🏁  Job succeeded", "git status looks fine"}
	for _, l := range keep {
		if isGitContextNoise(l) {
			t.Errorf("expected kept, dropped: %q", l)
		}
	}
}

// TestShouldHaltPolicy covers the halt/pass decision in isolation (no Docker).
func TestShouldHaltPolicy(t *testing.T) {
	info := func(w runner.BarrierWhen, idx int, err error) runner.StepBarrierInfo {
		return runner.StepBarrierInfo{When: w, Index: idx, Err: err}
	}
	boom := errors.New("boom")

	tests := []struct {
		name        string
		mode        mode
		breakpoints map[int]bool
		breakOnErr  bool
		in          runner.StepBarrierInfo
		want        bool
	}{
		{"step mode halts before", modeStep, nil, false, info(runner.BarrierBefore, 0, nil), true},
		{"step mode halts after", modeStep, nil, false, info(runner.BarrierAfter, 0, nil), true},
		{"continue passes plain", modeContinue, nil, false, info(runner.BarrierBefore, 0, nil), false},
		{"continue halts at breakpoint", modeContinue, map[int]bool{1: true}, false, info(runner.BarrierBefore, 1, nil), true},
		{"continue ignores breakpoint after", modeContinue, map[int]bool{1: true}, false, info(runner.BarrierAfter, 1, nil), false},
		{"break-on-error halts after failure", modeContinue, nil, true, info(runner.BarrierAfter, 0, boom), true},
		{"break-on-error passes on success", modeContinue, nil, true, info(runner.BarrierAfter, 0, nil), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Session{curMode: tt.mode, breakpoints: tt.breakpoints, breakOnErr: tt.breakOnErr}
			if got := s.shouldHalt(tt.in); got != tt.want {
				t.Errorf("shouldHalt = %v, want %v", got, tt.want)
			}
		})
	}
}
