package debugger

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nektos/act/pkg/model"
	"github.com/nektos/act/pkg/runner"
)

const sampleWorkflow = "../../testdata/workflows/sample.yml"

func TestDockerUnavailableError(t *testing.T) {
	cause := errors.New("dial /var/run/docker.sock: connect: connection refused")
	err := &DockerUnavailableError{Cause: cause}
	if !strings.Contains(err.Error(), "Docker daemon running") {
		t.Errorf("message %q lacks the friendly hint", err.Error())
	}
	if !errors.Is(err, cause) {
		t.Error("DockerUnavailableError should unwrap to its cause")
	}
}

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

// TestInterceptSteps covers the shared scan/rewrite mechanic: a matching step is
// rewritten to a no-op echoing the message, its label is preserved, non-matching
// steps are left alone, and `capture` runs before With is cleared.
func TestInterceptSteps(t *testing.T) {
	steps := []*model.Step{
		{ID: "a", Name: "named", Uses: "actions/checkout@v4", With: map[string]string{"x": "y"}},
		{ID: "b", Uses: "actions/setup-go@v5"}, // no match
		{ID: "c", Uses: "actions/checkout@v4"}, // matches, no Name → label falls back to Uses
	}
	var captured []string
	match := func(st *model.Step) bool { return strings.HasPrefix(st.Uses, "actions/checkout") }
	got := interceptSteps(steps, match, checkoutNoopMsg, func(st *model.Step) {
		captured = append(captured, st.With["x"]) // proves capture sees With before it's cleared
	})

	if len(got) != 2 || !got[0] || !got[2] {
		t.Errorf("rewritten indices = %v, want {0,2}", got)
	}
	if steps[0].Uses != "" || steps[0].With != nil || steps[0].Run != checkoutNoopMsg {
		t.Errorf("step 0 not rewritten to no-op: %+v", steps[0])
	}
	if steps[0].Name != "named" || steps[2].Name != "actions/checkout@v4" {
		t.Errorf("labels not preserved: %q, %q", steps[0].Name, steps[2].Name)
	}
	if steps[1].Uses != "actions/setup-go@v5" {
		t.Errorf("non-matching step altered: %q", steps[1].Uses)
	}
	// capture fires once per match (two here), and step 0's value proves it sees
	// With before interceptSteps clears it.
	if len(captured) != 2 || captured[0] != "y" {
		t.Errorf("capture = %v, want [y ...] (seen before With cleared)", captured)
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

// planRun parses an inline workflow and returns the single planned run for jobID,
// without touching Docker — so the pure selection/summary helpers can be unit-tested.
func planRun(t *testing.T, wf, jobID string) *model.Run {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wf.yml")
	if err := os.WriteFile(path, []byte(wf), 0o600); err != nil {
		t.Fatal(err)
	}
	planner, err := model.NewWorkflowPlanner(path, true, false)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.PlanEvent("push")
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range plan.Stages {
		for _, r := range st.Runs {
			if r.JobID == jobID {
				return r
			}
		}
	}
	t.Fatalf("job %q not planned", jobID)
	return nil
}

const matrixWorkflow = `name: m
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        os: [ubuntu-latest, macos-latest]
        go: ['1.21', '1.22']
    steps:
      - run: echo hi
`

// TestSelectMatrix covers the four selection outcomes: no matrix (selection moot),
// a full pin to one combo, an under-specified selection (MultipleMatrixError), and a
// selection matching nothing (plain error).
func TestSelectMatrix(t *testing.T) {
	// (a) matrix-less job: any selection is irrelevant, returned untouched.
	noMatrix := planRun(t, "name: n\non: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo a\n", "a")
	sel := map[string]map[string]bool{"os": {"ubuntu-latest": true}}
	got, err := selectMatrix(noMatrix, sel)
	if err != nil {
		t.Fatalf("no-matrix: %v", err)
	}
	if len(got) != 1 { // returned as-is (act ignores it on a matrix-less job)
		t.Errorf("no-matrix: got %v, want the selection unchanged", got)
	}

	run := planRun(t, matrixWorkflow, "build") // 4 combinations

	// (b) full pin -> the same selection, ready for Config.Matrix.
	pin := map[string]map[string]bool{"os": {"ubuntu-latest": true}, "go": {"1.21": true}}
	if _, err := selectMatrix(run, pin); err != nil {
		t.Errorf("full pin: %v", err)
	}

	// (c) under-specified (one of two keys) -> MultipleMatrixError with the candidates.
	under := map[string]map[string]bool{"os": {"ubuntu-latest": true}}
	_, err = selectMatrix(run, under)
	var multi *MultipleMatrixError
	if !errors.As(err, &multi) {
		t.Fatalf("under-specified: want MultipleMatrixError, got %v", err)
	}
	if multi.Job != "build" || len(multi.Combos) != 2 {
		t.Errorf("MultipleMatrixError = %+v, want job=build with 2 combos", multi)
	}
	if multi.Combos[0] != "go=1.21, os=ubuntu-latest" {
		t.Errorf("combo label = %q, want sorted 'go=…, os=…'", multi.Combos[0])
	}

	// (d) no combination matches -> plain error, not MultipleMatrixError.
	none := map[string]map[string]bool{"os": {"windows-latest": true}}
	_, err = selectMatrix(run, none)
	if err == nil || errors.As(err, &multi) {
		t.Errorf("no-match: want a plain error, got %v", err)
	}
}

// TestBuildServices checks the services summary lists names sorted, and is empty when
// the job declares no services.
func TestBuildServices(t *testing.T) {
	const wf = `name: s
on: push
jobs:
  it:
    runs-on: ubuntu-latest
    services:
      redis:
        image: redis
      postgres:
        image: postgres:16
    steps:
      - run: echo hi
`
	got := buildServices(planRun(t, wf, "it").Job())
	if len(got.Names) != 2 || got.Names[0] != "postgres" || got.Names[1] != "redis" {
		t.Errorf("buildServices = %v, want sorted [postgres redis]", got.Names)
	}

	none := buildServices(planRun(t, "name: n\non: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo a\n", "a").Job())
	if len(none.Names) != 0 {
		t.Errorf("no services: got %v, want empty", none.Names)
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
	// (the env injected at the auth step's position is asserted in TestBuildGCP.)
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
	if got := gcpBind(nil, true); got != "" {
		t.Errorf("gcpBind(nil) = %q, want empty without a credential file", got)
	}
	// (the no-identity env path — neutralized step, nothing injected — is asserted in TestBuildGCP.)
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

	// auth step present but no identity -> nothing injected (the step is still
	// neutralized at the New layer; here buildGCP just yields no env).
	if env, sum := buildGCP(nil, steps, targets); env != nil || sum.File || sum.Token {
		t.Errorf("no-identity: env=%v sum=%+v, want nil env, nothing injected", env, sum)
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

	// full identity -> both surfaces of the contract, and NEITHER a project nor
	// GOOGLE_GHA_CREDS_PATH (setup-gcloud would --cred-file an authorized_user ADC
	// and fail; the project is the workflow's concern, not the host's).
	env, sum = buildGCP(&GCPIdentity{CredentialFile: "/x", AccessToken: "ya29.x"}, steps, targets)
	if env["GOOGLE_APPLICATION_CREDENTIALS"] != gcpCredsContainerPath || env["CLOUDSDK_AUTH_ACCESS_TOKEN"] != "ya29.x" || !sum.File || !sum.Token {
		t.Errorf("full: env=%v sum=%+v, want the full credential contract", env, sum)
	}
	for _, k := range []string{"GOOGLE_GHA_CREDS_PATH", "GOOGLE_CLOUD_PROJECT", "CLOUDSDK_CORE_PROJECT"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s must not be set", k)
		}
	}

	// targets carried through regardless of identity
	if len(sum.Targets) != 1 || sum.Targets[0] != "sa via wip" {
		t.Errorf("targets = %v, want carried through", sum.Targets)
	}
}

// TestInterceptAWSAuth mirrors TestInterceptGCPAuth: aws-actions/configure-aws-
// credentials is rewritten to a no-op, keeps a visible label, its declared role+region
// target is captured, and the ambient credentials + region are summarized.
func TestInterceptAWSAuth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aws.yml")
	const wf = `name: aws
on: push
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - id: auth
        uses: aws-actions/configure-aws-credentials@v4
        with:
          role-to-assume: arn:aws:iam::123456789012:role/deploy
          aws-region: eu-west-1
      - run: aws s3 ls
`
	if err := os.WriteFile(path, []byte(wf), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{WorkflowPath: path, Workdir: t.TempDir(),
		AWS: &AWSIdentity{AccessKeyID: "AKIA", SecretAccessKey: "secret", SessionToken: "tok", Account: "arn:aws:iam::1:user/me"}})
	if err != nil {
		t.Fatal(err)
	}

	if got := s.Steps()[0]; got.Uses != "" || got.Run == "" {
		t.Errorf("auth step not rewritten: Uses=%q Run=%q", got.Uses, got.Run)
	}
	sum := s.AWSSummary()
	if len(sum.Steps) != 1 || sum.Steps[0] != "aws-actions/configure-aws-credentials@v4" {
		t.Errorf("AWSSummary.Steps = %v", sum.Steps)
	}
	want := "arn:aws:iam::123456789012:role/deploy in eu-west-1"
	if len(sum.Targets) != 1 || sum.Targets[0] != want {
		t.Errorf("AWSSummary.Targets = %v, want [%q]", sum.Targets, want)
	}
	if !sum.Creds || !sum.RegionSet || sum.Region != "eu-west-1" || sum.Account != "arn:aws:iam::1:user/me" {
		t.Errorf("AWSSummary = %+v, want creds+region from the declared step", sum)
	}
	// (the injected env contract is asserted directly in TestBuildAWS.)
}

// TestAWSNoIdentity: an auth step with no ambient credentials is still neutralized
// (so the job survives), but nothing is injected and the summary says so.
func TestAWSNoIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aws.yml")
	const wf = `name: aws
on: push
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: aws-actions/configure-aws-credentials@v4
      - run: echo hi
`
	if err := os.WriteFile(path, []byte(wf), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{WorkflowPath: path, Workdir: t.TempDir()}) // AWS nil
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Steps()[0]; got.Uses != "" || got.Run == "" {
		t.Errorf("auth step should still be neutralized without creds: Uses=%q", got.Uses)
	}
	if sum := s.AWSSummary(); len(sum.Steps) != 1 || sum.Creds || sum.RegionSet {
		t.Errorf("AWSSummary = %+v, want one step and nothing injected", sum)
	}
}

// TestAWSRegionExpression covers the deliberate choice to honor only a literal
// aws-region: an expression is left to act (we'd inject the raw unevaluated string).
func TestAWSRegionExpression(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aws.yml")
	const wf = `name: aws
on: push
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: aws-actions/configure-aws-credentials@v4
        with:
          aws-region: ${{ vars.REGION }}
      - run: aws s3 ls
`
	if err := os.WriteFile(path, []byte(wf), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{WorkflowPath: path, Workdir: t.TempDir(),
		AWS: &AWSIdentity{AccessKeyID: "AKIA", SecretAccessKey: "secret"}})
	if err != nil {
		t.Fatal(err)
	}
	if sum := s.AWSSummary(); sum.RegionSet || sum.Region != "" {
		t.Errorf("AWSSummary = %+v, want no region injected for an expression", sum)
	}
}

// TestBuildAWS unit-tests the env/summary assembly directly across the cases
// (no steps, no identity, full creds+region, long-lived creds) without touching act.
func TestBuildAWS(t *testing.T) {
	steps := map[int]bool{1: true}
	targets := []string{"role in eu-west-1"}

	// no auth steps -> empty, even with an identity
	if env, sum := buildAWS(&AWSIdentity{AccessKeyID: "AKIA", SecretAccessKey: "s"}, nil, nil, "eu-west-1"); env != nil || len(sum.Steps) != 0 || sum.Creds {
		t.Errorf("no steps: env=%v sum=%+v, want empty", env, sum)
	}

	// auth step present but no identity -> nothing injected
	if env, sum := buildAWS(nil, steps, targets, "eu-west-1"); env != nil || sum.Creds || sum.RegionSet {
		t.Errorf("no-identity: env=%v sum=%+v, want nil env", env, sum)
	}

	// full session creds + literal region -> the env contract, both region keys
	env, sum := buildAWS(&AWSIdentity{AccessKeyID: "AKIA", SecretAccessKey: "secret", SessionToken: "tok"}, steps, targets, "eu-west-1")
	if env["AWS_ACCESS_KEY_ID"] != "AKIA" || env["AWS_SECRET_ACCESS_KEY"] != "secret" || env["AWS_SESSION_TOKEN"] != "tok" {
		t.Errorf("creds: env=%v", env)
	}
	if env["AWS_REGION"] != "eu-west-1" || env["AWS_DEFAULT_REGION"] != "eu-west-1" || !sum.RegionSet {
		t.Errorf("region not injected: env=%v sum=%+v", env, sum)
	}
	if !sum.Creds || sum.Region != "eu-west-1" {
		t.Errorf("summary = %+v", sum)
	}
	if len(sum.Targets) != 1 || sum.Targets[0] != "role in eu-west-1" {
		t.Errorf("targets = %v, want carried through", sum.Targets)
	}

	// long-lived creds (no session token), no region -> neither optional key set
	env, _ = buildAWS(&AWSIdentity{AccessKeyID: "AKIA", SecretAccessKey: "secret"}, steps, targets, "")
	if _, ok := env["AWS_SESSION_TOKEN"]; ok {
		t.Error("no session token should not set AWS_SESSION_TOKEN")
	}
	if _, ok := env["AWS_REGION"]; ok {
		t.Error("empty region should not set AWS_REGION")
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

// TestEmitProgress: progress is advisory and emitted only for the debugged job's
// "before" boundaries; After boundaries and other jobs (--with-deps) don't fire it,
// and a full buffer drops silently rather than blocking the act goroutine.
func TestEmitProgress(t *testing.T) {
	s := &Session{jobID: "build", progress: make(chan ProgressEvent, 2)}
	info := func(w runner.BarrierWhen, job string, idx int) runner.StepBarrierInfo {
		return runner.StepBarrierInfo{When: w, JobID: job, Index: idx}
	}

	// before-boundary on the debugged job → one event with the right index
	s.emitProgress(info(runner.BarrierBefore, "build", 3))
	select {
	case ev := <-s.progress:
		if ev.Index != 3 {
			t.Errorf("progress index = %d, want 3", ev.Index)
		}
	default:
		t.Fatal("expected a progress event for the before boundary")
	}

	// after-boundary and a different job emit nothing
	s.emitProgress(info(runner.BarrierAfter, "build", 3))
	s.emitProgress(info(runner.BarrierBefore, "deploy", 0))
	select {
	case ev := <-s.progress:
		t.Errorf("unexpected progress event: %+v", ev)
	default:
	}

	// a full buffer drops without blocking
	s.emitProgress(info(runner.BarrierBefore, "build", 0))
	s.emitProgress(info(runner.BarrierBefore, "build", 1))
	s.emitProgress(info(runner.BarrierBefore, "build", 2)) // buffer full → dropped, must not block
}

// TestResolveToken: an explicit token wins; otherwise a GITHUB_TOKEN secret is
// used and mirrored so github.token and secrets.GITHUB_TOKEN stay equal; absent
// yields an empty, not-present summary.
func TestResolveToken(t *testing.T) {
	// flag wins, and is mirrored into secrets so secrets.GITHUB_TOKEN resolves too
	sec := map[string]string{}
	tok, sum := resolveToken("ghp_flag", sec)
	if tok != "ghp_flag" || !sum.Present || sum.Source != "flag" {
		t.Errorf("flag path: tok=%q sum=%+v", tok, sum)
	}
	if sec["GITHUB_TOKEN"] != "ghp_flag" {
		t.Errorf("flag token not mirrored into secrets: %v", sec)
	}

	// secret fallback when no flag
	sec = map[string]string{"GITHUB_TOKEN": "ghp_secret"}
	tok, sum = resolveToken("", sec)
	if tok != "ghp_secret" || sum.Source != "secret" {
		t.Errorf("secret path: tok=%q sum=%+v", tok, sum)
	}

	// absent
	if tok, sum := resolveToken("", map[string]string{}); tok != "" || sum.Present {
		t.Errorf("absent path: tok=%q sum=%+v", tok, sum)
	}
}

// TestBuildEvent covers the four payload cases, especially merging -input values
// into a supplied event file (act drops Config.Inputs once EventPath is set).
func TestBuildEvent(t *testing.T) {
	// synthetic: no file, no inputs → empty eventPath, marked synthetic
	if p, tmp, sum, err := buildEvent(Options{EventName: "push"}); err != nil || p != "" || tmp != "" || !sum.Synthetic {
		t.Errorf("synthetic: p=%q tmp=%q sum=%+v err=%v", p, tmp, sum, err)
	}

	// inputs only: still no event file (act builds {"inputs":…} from Config.Inputs)
	if p, tmp, _, err := buildEvent(Options{EventName: "workflow_dispatch", Inputs: map[string]string{"x": "1"}}); err != nil || p != "" || tmp != "" {
		t.Errorf("inputs-only: p=%q tmp=%q err=%v", p, tmp, err)
	}

	// event file only: passed through as-is, not synthetic
	dir := t.TempDir()
	ef := filepath.Join(dir, "event.json")
	if err := os.WriteFile(ef, []byte(`{"action":"opened"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if p, tmp, sum, err := buildEvent(Options{EventName: "pull_request", EventPath: ef}); err != nil || p != ef || tmp != "" || sum.Synthetic {
		t.Errorf("file-only: p=%q tmp=%q sum=%+v err=%v", p, tmp, sum, err)
	}

	// event file + inputs: merged into a temp file with inputs present
	p, tmp, _, err := buildEvent(Options{EventName: "workflow_dispatch", EventPath: ef, Inputs: map[string]string{"env": "prod"}})
	if err != nil || p == "" || p != tmp {
		t.Fatalf("merge: p=%q tmp=%q err=%v", p, tmp, err)
	}
	defer os.Remove(tmp)
	var got map[string]any
	raw, _ := os.ReadFile(tmp)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	inputs, _ := got["inputs"].(map[string]any)
	if inputs["env"] != "prod" || got["action"] != "opened" {
		t.Errorf("merged event wrong: %v", got)
	}

	// missing event file → error
	if _, _, _, err := buildEvent(Options{EventPath: filepath.Join(dir, "nope.json")}); err == nil {
		t.Error("missing event file should error")
	}
}

// TestBuildGitHubContext maps values to act's env keys (sha → SHA_REF) and marks as
// overridden only the fields GitHubOverrides names — env injection follows non-empty
// values, the override mark follows the explicit flag set (the two are independent).
func TestBuildGitHubContext(t *testing.T) {
	env, sum := buildGitHubContext(Options{
		Repository: "o/r", Ref: "refs/heads/x", Sha: "0123456789abcdef", Actor: "me",
		GitHubOverrides: []string{"repository", "ref", "sha", "actor"},
	})
	if env["GITHUB_REPOSITORY"] != "o/r" || env["GITHUB_REF"] != "refs/heads/x" || env["SHA_REF"] != "0123456789abcdef" {
		t.Errorf("env wrong: %v", env)
	}
	if sum.Sha != "0123456" {
		t.Errorf("sha not shortened for display: %q", sum.Sha)
	}
	if strings.Join(sum.Overridden, ",") != "repository,ref,sha,actor" {
		t.Errorf("overridden wrong: %v", sum.Overridden)
	}

	// Values present (as if derived from local git) but no flag set → env is still
	// injected, but nothing is marked an override. This is the bug guard: a plain run
	// must not cry override over git-derived defaults.
	env, sum = buildGitHubContext(Options{Repository: "o/r", Ref: "refs/heads/x", Sha: "0123456789abcdef"})
	if env["GITHUB_REPOSITORY"] != "o/r" {
		t.Errorf("derived value should still inject env: %v", env)
	}
	if len(sum.Overridden) != 0 {
		t.Errorf("no flag set → no overrides, got %v", sum.Overridden)
	}

	// nothing set → empty env, no overrides
	if env, sum := buildGitHubContext(Options{}); len(env) != 0 || len(sum.Overridden) != 0 {
		t.Errorf("empty: env=%v sum=%+v", env, sum)
	}
}

// TestRuntimeContextWiring checks New threads the §4 runtime-context options into
// the Session summaries the TUI reads (token, event, github.* context).
func TestRuntimeContextWiring(t *testing.T) {
	s, err := New(Options{
		WorkflowPath: sampleWorkflow,
		Workdir:      t.TempDir(),
		Secrets:      map[string]string{"GITHUB_TOKEN": "ghp_x"},
		Repository:      "o/r",
		Ref:             "refs/heads/feature",
		Sha:             "deadbeefcafebabe",
		Actor:           "alice",
		GitHubOverrides: []string{"repository", "ref", "sha", "actor"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if tok := s.TokenSummary(); !tok.Present || tok.Source != "secret" {
		t.Errorf("token summary = %+v, want present from secret", tok)
	}
	ghc := s.GitHubContextSummary()
	if ghc.Repository != "o/r" || ghc.Ref != "refs/heads/feature" || ghc.Sha != "deadbee" || ghc.Actor != "alice" {
		t.Errorf("ghc summary = %+v", ghc)
	}
	if len(ghc.Overridden) != 4 {
		t.Errorf("want all 4 fields overridden, got %v", ghc.Overridden)
	}
	if ev := s.EventSummary(); !ev.Synthetic || ev.EventName != "push" {
		t.Errorf("event summary = %+v, want synthetic push", ev)
	}
}

// TestInputsFixture guards the inputs.yml sample: as a workflow_dispatch it declares
// three inputs, omitted ones are reported as using their declared default, and a
// supplied input moves from defaults to provided.
func TestInputsFixture(t *testing.T) {
	const wf = "../../testdata/workflows/inputs.yml"

	// no -input → all three declared inputs use their defaults
	s, err := New(Options{WorkflowPath: wf, EventName: "workflow_dispatch", Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	in := s.InputsSummary()
	if !in.Declared || len(in.Defaults) != 3 || len(in.Provided) != 0 {
		t.Errorf("no-input summary = %+v, want 3 defaulted, 0 provided", in)
	}

	// supplying one input moves it to provided, leaving two defaulted
	s2, err := New(Options{WorkflowPath: wf, EventName: "workflow_dispatch", Workdir: t.TempDir(),
		Inputs: map[string]string{"environment": "production"}})
	if err != nil {
		t.Fatal(err)
	}
	in2 := s2.InputsSummary()
	if in2.Provided["environment"] != "production" || len(in2.Defaults) != 2 {
		t.Errorf("one-input summary = %+v, want environment provided + 2 defaulted", in2)
	}
}

// TestTokenFixture guards the token.yml sample: it parses, and a supplied
// GITHUB_TOKEN secret surfaces as a present github.token.
func TestTokenFixture(t *testing.T) {
	s, err := New(Options{
		WorkflowPath: "../../testdata/workflows/token.yml",
		Workdir:      t.TempDir(),
		Secrets:      map[string]string{"GITHUB_TOKEN": "ghp_fixture"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if tok := s.TokenSummary(); !tok.Present || tok.Source != "secret" {
		t.Errorf("token summary = %+v, want present from secret", tok)
	}
}
