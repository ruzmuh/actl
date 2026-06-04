package debugger

import (
	"errors"
	"os"
	"path/filepath"
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
	if n := len(s.Steps()); n != 6 {
		t.Fatalf("len(Steps) = %d, want 6", n)
	}
	if got := s.Steps()[0].String(); got != "greet" {
		t.Errorf("step 1 = %q, want %q", got, "greet")
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

func TestSelectJob(t *testing.T) {
	const wf = "testdata/two-jobs.yml"

	// no JobID + multiple jobs -> MultipleJobsError listing the choices
	_, err := New(Options{WorkflowPath: wf, Workdir: t.TempDir()})
	var multi *MultipleJobsError
	if !errors.As(err, &multi) {
		t.Fatalf("want MultipleJobsError, got %v", err)
	}
	if got := multi.Jobs; len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("MultipleJobsError.Jobs = %v, want [a b]", got)
	}

	// explicit job -> that job
	s, err := New(Options{WorkflowPath: wf, JobID: "b", Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if s.JobID() != "b" {
		t.Errorf("JobID = %q, want %q", s.JobID(), "b")
	}

	// unknown job -> plain error, not MultipleJobsError
	_, err = New(Options{WorkflowPath: wf, JobID: "nope", Workdir: t.TempDir()})
	if err == nil || errors.As(err, &multi) {
		t.Errorf("want a plain not-found error, got %v", err)
	}
}

// TestIsolatedPlan guards against re-executing the whole dependency graph: the
// session must run only the selected job's run, even when it has needs.
func TestIsolatedPlan(t *testing.T) {
	s, err := New(Options{WorkflowPath: "../../testdata/workflows/pipeline.yml", JobID: "deploy", Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.plan.Stages) != 1 || len(s.plan.Stages[0].Runs) != 1 {
		t.Fatalf("plan should hold exactly one run, got stages=%d", len(s.plan.Stages))
	}
	if got := s.plan.Stages[0].Runs[0].JobID; got != "deploy" {
		t.Errorf("isolated run = %q, want deploy", got)
	}
}

// TestWithDepsRunsFullPlan checks that --with-deps executes the whole plan (so
// upstream jobs run for real) and marks the needs as live.
func TestWithDepsRunsFullPlan(t *testing.T) {
	s, err := New(Options{WorkflowPath: "../../testdata/workflows/pipeline.yml", JobID: "deploy", WithDeps: true, Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !s.WithDeps() {
		t.Error("WithDeps() = false, want true")
	}
	runs := 0
	for _, st := range s.plan.Stages {
		runs += len(st.Runs)
	}
	if runs != 3 {
		t.Errorf("plan runs = %d, want 3 (build, test, deploy)", runs)
	}
	sum := s.NeedsSummary()
	if len(sum) != 2 {
		t.Fatalf("NeedsSummary = %+v, want 2 live entries", sum)
	}
	for _, n := range sum {
		if !n.Live {
			t.Errorf("need %q: Live = false, want true", n.Job)
		}
	}
}

// TestShouldHaltSkipsOtherJobs covers the --with-deps gate: barriers from jobs
// other than the one being debugged must always pass through.
func TestShouldHaltSkipsOtherJobs(t *testing.T) {
	s := &Session{curMode: modeStep, jobID: "deploy"}
	if s.shouldHalt(runner.StepBarrierInfo{When: runner.BarrierBefore, JobID: "build"}) {
		t.Error("halted on a non-target job; want pass-through")
	}
	if !s.shouldHalt(runner.StepBarrierInfo{When: runner.BarrierBefore, JobID: "deploy"}) {
		t.Error("did not halt on the target job in step mode")
	}
	if !s.shouldHalt(runner.StepBarrierInfo{When: runner.BarrierBefore, JobID: ""}) {
		t.Error("empty JobID should be treated as the target (isolation)")
	}
}

// TestSeedNeeds verifies isolated-run needs seeding: outputs are replaced by only
// the supplied keys (so unseeded ones resolve empty, like GitHub), and the result
// defaults to success unless overridden.
func TestSeedNeeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wf.yml")
	const wf = `name: t
on: push
jobs:
  a:
    runs-on: ubuntu-latest
    outputs:
      image: ${{ steps.x.outputs.image }}
      tag: ${{ steps.x.outputs.tag }}
    steps:
      - id: x
        run: echo hi
  b:
    runs-on: ubuntu-latest
    needs: a
    steps:
      - run: echo ${{ needs.a.outputs.image }}
`
	if err := os.WriteFile(path, []byte(wf), 0o600); err != nil {
		t.Fatal(err)
	}

	// seed one output; leave result to default, leave 'tag' unseeded
	s, err := New(Options{
		WorkflowPath: path,
		JobID:        "b",
		Workdir:      t.TempDir(),
		Needs:        map[string]NeedsInput{"a": {Outputs: map[string]string{"image": "bar"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sum := s.NeedsSummary()
	if len(sum) != 1 || sum[0].Job != "a" {
		t.Fatalf("NeedsSummary = %+v, want one entry for job a", sum)
	}
	if !sum[0].Assumed || sum[0].Result != "success" {
		t.Errorf("result = %q assumed=%v, want success assumed", sum[0].Result, sum[0].Assumed)
	}
	if got := sum[0].Outputs; len(got) != 1 || got["image"] != "bar" {
		t.Errorf("outputs = %v, want only {image:bar} (unseeded 'tag' dropped)", got)
	}

	// overriding the result clears the assumed flag
	s2, err := New(Options{
		WorkflowPath: path,
		JobID:        "b",
		Workdir:      t.TempDir(),
		Needs:        map[string]NeedsInput{"a": {Result: "failure"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sum := s2.NeedsSummary(); sum[0].Assumed || sum[0].Result != "failure" {
		t.Errorf("override result = %q assumed=%v, want failure not-assumed", sum[0].Result, sum[0].Assumed)
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
