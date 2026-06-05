package debugger

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// TestUsesSampleParses confirms a uses-heavy workflow parses and plans through
// act's model with no special-casing on our side (run + node + docker steps).
func TestUsesSampleParses(t *testing.T) {
	s, err := New(Options{WorkflowPath: "../../testdata/workflows/uses.yml", Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(s.Steps()); n != 4 {
		t.Fatalf("len(Steps) = %d, want 4", n)
	}
	// the node action step carries a Uses and no Run
	node := s.Steps()[1]
	if node.Uses == "" || node.Run != "" {
		t.Errorf("step 2 should be a uses: step, got Uses=%q Run=%q", node.Uses, node.Run)
	}
}

// TestInterceptCheckout covers faithful local checkout: a default actions/checkout
// is rewritten to a no-op and copied from the source tree, while a checkout pinned
// to another repo/ref is left as a real clone.
func TestInterceptCheckout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "co.yml")
	const wf = `name: checkout
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - name: checkout
        uses: actions/checkout@v4
      - name: pinned
        uses: actions/checkout@v4
        with:
          repository: other/repo
      - run: cat README.md
`
	if err := os.WriteFile(path, []byte(wf), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{WorkflowPath: path, Source: t.TempDir()}) // copy mode (default checkout present)
	if err != nil {
		t.Fatal(err)
	}

	// default checkout (step 0) -> no-op run, recorded; source set
	if got := s.Steps()[0]; got.Uses != "" || got.Run == "" {
		t.Errorf("default checkout not rewritten: Uses=%q Run=%q", got.Uses, got.Run)
	}
	if labels := s.CheckoutSteps(); len(labels) != 1 || labels[0] != "checkout" {
		t.Errorf("CheckoutSteps = %v, want [checkout]", labels)
	}
	if s.CheckoutSource() == "" {
		t.Error("CheckoutSource empty, want the source tree path")
	}

	// pinned checkout (step 1) -> untouched real clone
	if got := s.Steps()[1]; !strings.HasPrefix(got.Uses, "actions/checkout") {
		t.Errorf("pinned checkout was altered: Uses=%q", got.Uses)
	}
}

// TestCheckoutNoCopyWhenBound: with an explicit -workdir mount, an intercepted
// checkout is a pure no-op (the mount already holds the code) — no source copy.
func TestCheckoutNoCopyWhenBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "co.yml")
	const wf = `name: checkout
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: echo hi
`
	if err := os.WriteFile(path, []byte(wf), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{WorkflowPath: path, Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.CheckoutSteps()) != 1 {
		t.Fatalf("checkout not intercepted under -workdir: %v", s.CheckoutSteps())
	}
	if s.CheckoutSource() != "" {
		t.Errorf("CheckoutSource = %q, want empty (mounted workspace, no copy)", s.CheckoutSource())
	}
}

// TestWorkspaceIsolation covers the workspace caveat surfaced to the user: with
// an empty workspace local `uses: ./` actions can't resolve, and we flag them.
func TestWorkspaceIsolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local.yml")
	const wf = `name: local
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - name: setup
        uses: ./.github/actions/setup
      - name: run
        run: echo hi
`
	if err := os.WriteFile(path, []byte(wf), 0o600); err != nil {
		t.Fatal(err)
	}

	// empty workdir -> isolated temp workspace; the local action is flagged
	s, err := New(Options{WorkflowPath: path}) // no Workdir
	if err != nil {
		t.Fatal(err)
	}
	defer s.cleanup() // we never Start, so reclaim the temp workspace ourselves
	if !s.WorkspaceIsolated() {
		t.Error("WorkspaceIsolated() = false, want true for an empty workdir")
	}
	if locals := s.LocalUsesSteps(); len(locals) != 1 || locals[0] != "setup" {
		t.Errorf("LocalUsesSteps() = %v, want [setup]", locals)
	}

	// a real workdir -> not isolated
	s2, err := New(Options{WorkflowPath: path, Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if s2.WorkspaceIsolated() {
		t.Error("WorkspaceIsolated() = true, want false when a workdir is given")
	}
}

// TestLocalActionFixture guards the committed workspace fixture: it parses and
// its local composite step is detected (the live `-workdir` test runs it).
func TestLocalActionFixture(t *testing.T) {
	s, err := New(Options{WorkflowPath: "../../testdata/workspace/.github/workflows/local.yml"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.cleanup()
	if locals := s.LocalUsesSteps(); len(locals) != 1 {
		t.Errorf("LocalUsesSteps() = %v, want one local action step", locals)
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

// TestInterceptGCPAuth covers detection + rewrite of google-github-actions/auth:
// the step becomes a no-op run (so it can't federate), keeps a visible label, and
// its declared federation target is captured before With is cleared.
func TestInterceptGCPAuth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gcp.yml")
	const wf = `name: gcp
on: push
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - id: auth
        uses: google-github-actions/auth@v2
        with:
          service_account: sa@proj.iam.gserviceaccount.com
          workload_identity_provider: projects/1/locations/global/workloadIdentityPools/p/providers/x
      - run: gcloud projects describe proj
`
	if err := os.WriteFile(path, []byte(wf), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{WorkflowPath: path, Workdir: t.TempDir(),
		GCP: &GCPIdentity{CredentialFile: "/tmp/adc.json", AccessToken: "ya29.x", Account: "me@example.com"}})
	if err != nil {
		t.Fatal(err)
	}

	// auth step (0) -> no-op run, recorded
	if got := s.Steps()[0]; got.Uses != "" || got.Run == "" {
		t.Errorf("auth step not rewritten: Uses=%q Run=%q", got.Uses, got.Run)
	}
	sum := s.GCPSummary()
	if len(sum.Steps) != 1 || sum.Steps[0] != "google-github-actions/auth@v2" {
		t.Errorf("GCPSummary.Steps = %v, want [google-github-actions/auth@v2]", sum.Steps)
	}
	want := "sa@proj.iam.gserviceaccount.com via projects/1/locations/global/workloadIdentityPools/p/providers/x"
	if len(sum.Targets) != 1 || sum.Targets[0] != want {
		t.Errorf("GCPSummary.Targets = %v, want [%q]", sum.Targets, want)
	}
	if !sum.File || !sum.Token || sum.Account != "me@example.com" {
		t.Errorf("GCPSummary = %+v, want File+Token+account", sum)
	}

	// the credential bind that act mounts into the job container.
	if got := gcpBind(&GCPIdentity{CredentialFile: "/tmp/adc.json"}, true); !strings.Contains(got, "/tmp/adc.json:"+gcpCredsContainerPath+":ro") {
		t.Errorf("gcpBind = %q, want the ro credential bind", got)
	}

	// env injected into the live map at the auth step's position. Note we set neither
	// GOOGLE_GHA_CREDS_PATH (setup-gcloud would --cred-file an authorized_user ADC and
	// fail) nor any project (that's the workflow's concern, not the host's).
	if env := s.gcpEnv; env["GOOGLE_APPLICATION_CREDENTIALS"] != gcpCredsContainerPath ||
		env["CLOUDSDK_AUTH_ACCESS_TOKEN"] != "ya29.x" {
		t.Errorf("gcpEnv = %v, want the credential contract", s.gcpEnv)
	}
	for _, k := range []string{"GOOGLE_GHA_CREDS_PATH", "GOOGLE_CLOUD_PROJECT", "CLOUDSDK_CORE_PROJECT"} {
		if _, ok := s.gcpEnv[k]; ok {
			t.Errorf("%s must not be set", k)
		}
	}
}

// TestGCPNoIdentity: an auth step with no ambient credentials is still neutralized
// (so the job survives), but nothing is injected and the summary says so.
func TestGCPNoIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gcp.yml")
	const wf = `name: gcp
on: push
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: google-github-actions/auth@v2
      - run: echo hi
`
	if err := os.WriteFile(path, []byte(wf), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{WorkflowPath: path, Workdir: t.TempDir()}) // GCP nil
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Steps()[0]; got.Uses != "" || got.Run == "" {
		t.Errorf("auth step should still be neutralized without creds: Uses=%q", got.Uses)
	}
	sum := s.GCPSummary()
	if len(sum.Steps) != 1 || sum.File || sum.Token {
		t.Errorf("GCPSummary = %+v, want one step and nothing injected", sum)
	}
	if len(s.gcpEnv) != 0 {
		t.Errorf("gcpEnv = %v, want empty without creds", s.gcpEnv)
	}
	if got := gcpBind(nil, true); got != "" {
		t.Errorf("gcpBind(nil) = %q, want empty without a credential file", got)
	}
}

// TestBuildGCP unit-tests the env/summary assembly directly across the partial-creds
// cases (token-only, file-only) without touching act.
func TestBuildGCP(t *testing.T) {
	steps := map[int]bool{2: true}
	targets := []string{"sa via wip"}

	// no auth steps -> empty, even with an identity
	if env, sum := buildGCP(&GCPIdentity{AccessToken: "t"}, nil, nil); env != nil || len(sum.Steps) != 0 || sum.Token {
		t.Errorf("no steps: env=%v sum=%+v, want empty", env, sum)
	}

	// token only -> CLOUDSDK token, no file
	env, sum := buildGCP(&GCPIdentity{AccessToken: "tok"}, steps, targets)
	if env["CLOUDSDK_AUTH_ACCESS_TOKEN"] != "tok" || sum.Token != true || sum.File {
		t.Errorf("token-only: env=%v sum=%+v", env, sum)
	}
	if _, ok := env["GOOGLE_APPLICATION_CREDENTIALS"]; ok {
		t.Error("token-only should not set GOOGLE_APPLICATION_CREDENTIALS")
	}

	// file only -> the GOOGLE_* contract, no token
	env, sum = buildGCP(&GCPIdentity{CredentialFile: "/x"}, steps, targets)
	if env["GOOGLE_APPLICATION_CREDENTIALS"] != gcpCredsContainerPath || !sum.File || sum.Token {
		t.Errorf("file-only: env=%v sum=%+v", env, sum)
	}

	// targets carried through regardless of identity
	if len(sum.Targets) != 1 || sum.Targets[0] != "sa via wip" {
		t.Errorf("targets = %v, want carried through", sum.Targets)
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
