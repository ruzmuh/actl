// Package debugger is actl's frontend-agnostic core (see CLAUDE.md §5). It owns
// the barrier-driven pause loop and the halt/pass policy; it imports no frontend,
// prints nothing, and owns no terminal. The TUI, a future DAP server, and the
// headless/agent driver are all peers that consume this API:
//
//	commands in  — Step / Continue / Abort / SetBreakpoint
//	events out   — PauseEvent on Pauses(), completion on Done()
//
// It drives act through the soft-fork StepBarrier hook (third_party/act): act
// keeps the job container alive between step execs, so blocking inside the
// barrier yields a live workspace + env to inspect.
package debugger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/nektos/act/pkg/common"
	"github.com/nektos/act/pkg/model"
	"github.com/nektos/act/pkg/runner"
)

// ErrAborted is the run error when the front-end aborts via Abort.
var ErrAborted = errors.New("debugger: run aborted by user")

// MultipleJobsError is returned by New when the workflow has more than one job
// and Options.JobID did not pick one. It lists the available job ids so a
// frontend can prompt for a choice.
type MultipleJobsError struct{ Jobs []string }

func (e *MultipleJobsError) Error() string {
	return fmt.Sprintf("workflow has %d jobs; select one with -job: %s", len(e.Jobs), strings.Join(e.Jobs, ", "))
}

// NeedsInput seeds an upstream job's contribution to the needs context when a
// downstream job is debugged in isolation (the upstream job is not run). Outputs
// holds only the keys the user provided; Result defaults to "success" if empty.
type NeedsInput struct {
	Outputs map[string]string
	Result  string
}

// NeedsSummary describes how one of the selected job's needs was satisfied
// locally, for a transparency line. With Live (the --with-deps mode) the upstream
// job runs for real and the seeded fields are unused; otherwise Result is the
// effective value (Assumed when defaulted) and Outputs holds the seeded keys.
type NeedsSummary struct {
	Job     string
	Live    bool
	Result  string
	Assumed bool
	Outputs map[string]string
}

// ConfigSummary is a redacted view of the secrets/vars/env supplied to the run:
// the names that loaded, never their values, so a transparency line (and any
// screenshot of it) leaks nothing sensitive.
type ConfigSummary struct {
	Secrets []string // secret names (sorted), values withheld
	Vars    []string // var names (sorted)
	Env     []string // env names (sorted)
}

// GCPIdentity is the host-resolved ambient GCP credentials the CLI passes in for
// identity substitution (CLAUDE.md §4). Locally there is no GitHub OIDC issuer, so
// `google-github-actions/auth` can't federate; instead we intercept that step and
// inject the dev's already-present credentials. The core never shells out to gcloud
// — discovery/minting is a host concern (cmd/actl); the core only consumes the data.
type GCPIdentity struct {
	CredentialFile string // host path to the ADC json, bind-mounted ro into the container
	AccessToken    string // ambient ADC access token → CLOUDSDK_AUTH_ACCESS_TOKEN
	Account        string // local identity, for the transparency line (best-effort, may be empty)
}

// AWSIdentity is the host-resolved ambient AWS credentials the CLI passes in for
// identity substitution (CLAUDE.md §4), the AWS analog of GCPIdentity. Locally there
// is no GitHub OIDC issuer, so `aws-actions/configure-aws-credentials` can't federate
// a role; instead we intercept that step and inject the dev's already-resolved
// session credentials (e.g. from `aws configure export-credentials`). Unlike GCP these
// are env-only — no file is mounted — so the core just injects them at the step's
// position. Discovery is a host concern (cmd/actl); the core only consumes the data.
type AWSIdentity struct {
	AccessKeyID     string // → AWS_ACCESS_KEY_ID
	SecretAccessKey string // → AWS_SECRET_ACCESS_KEY
	SessionToken    string // → AWS_SESSION_TOKEN (empty for long-lived creds)
	Account         string // local caller identity (arn), for the transparency line (best-effort)
}

// TokenSummary is a redacted view of the GITHUB_TOKEN substitution for a
// transparency line: whether github.token was set and where it came from, never
// the token itself.
type TokenSummary struct {
	Present bool   // github.token (and secrets.GITHUB_TOKEN) was set
	Source  string // "flag", "secret", or "" when absent
}

// InputsSummary lists the workflow's declared dispatch/call inputs and which were
// supplied vs. left to their declared default, for a transparency line. Values are
// the user's own CLI inputs (not secrets), so they're shown.
type InputsSummary struct {
	Provided map[string]string // inputs.* the user supplied
	Defaults []string          // declared inputs not supplied (act fills their default)
	Declared bool              // the workflow declares inputs for this event
}

// EventSummary describes the github.event payload backing the run, for a
// transparency line.
type EventSummary struct {
	EventName string // the planned event (github.event_name)
	Path      string // user-supplied event JSON path, if any (else synthesized "{}")
	Synthetic bool   // payload was synthesized (no -event-file)
}

// GitHubContextSummary is the resolved github.* runtime context for a transparency
// line: the values act will expose (repository/ref/sha/actor), which the user
// overrode, and a note that run ids are placeholders locally.
type GitHubContextSummary struct {
	Repository string   // github.repository (resolved from local git or overridden)
	Ref        string   // github.ref
	Sha        string   // github.sha (short, for display)
	Actor      string   // github.actor ("" → act's "nektos/act" placeholder)
	Overridden []string // which of repository/ref/sha/actor came from a flag
}

// GCPSummary is a redacted view of the GCP identity substitution for a transparency
// line: which auth steps were intercepted, the federation target each one would have
// used in real CI, and the local identity we run as instead. No token material is
// retained here.
type GCPSummary struct {
	Steps   []string // intercepted google-github-actions/auth step labels
	Targets []string // "<service_account> via <workload_identity_provider>" per step (as declared)
	Account string   // local ambient identity we run as ("" if none was found)
	File    bool     // an ADC credential file was mounted into the container
	Token   bool     // an access token was injected
}

// AWSSummary is a redacted view of the AWS identity substitution for a transparency
// line, the AWS analog of GCPSummary: which auth steps were intercepted, the role +
// region each would have federated as in real CI, and the local identity we run as.
// No credential material is retained here.
type AWSSummary struct {
	Steps     []string // intercepted aws-actions/configure-aws-credentials step labels
	Targets   []string // "<role-to-assume> in <region>" per step (as declared)
	Account   string   // local caller arn we run as ("" if unknown)
	Region    string   // the declared aws-region actl honors ("" if none / an expression)
	Creds     bool     // ambient credentials were injected
	RegionSet bool     // AWS_REGION/AWS_DEFAULT_REGION were injected from the declared region
}

// container path the ambient ADC file is mounted at (matches the env we inject).
const gcpCredsContainerPath = "/actl/gcp/adc.json"

// Options configures a debug Session. Only WorkflowPath is required.
type Options struct {
	WorkflowPath string                // path to the workflow file
	EventName    string                // event to plan for (default "push")
	JobID        string                // which job to debug; required only if the event plans more than one
	WithDeps     bool                  // run the job's upstream needs for real (to completion) before debugging it, instead of isolating
	Image        string                // docker image mapped to ubuntu-latest (default catthehacker)
	Workdir      string                // workspace bind-mounted into the container so local 'uses: ./' actions resolve; empty = an isolated empty temp dir (steps can't write to your tree). NOTE: a set workdir is mounted, so steps can write to it
	Source       string                // working tree a default actions/checkout copies into the workspace (no host mutation); empty = current dir. Ignored when Workdir is set
	Secrets      map[string]string     // secrets.* exposed to the workflow
	Vars         map[string]string     // vars.* exposed to the workflow
	Env          map[string]string     // extra env for containers
	GCP          *GCPIdentity          // ambient GCP creds to substitute for a federated google-github-actions/auth (nil = leave auth steps untouched)
	AWS          *AWSIdentity          // ambient AWS creds to substitute for a federated aws-actions/configure-aws-credentials (nil = leave auth steps untouched)
	Needs        map[string]NeedsInput // seeded needs.<job>.* for isolated debugging, keyed by upstream job id (ignored with WithDeps)
	BreakOnError bool                  // in Continue mode, halt after a step that errored
	Breakpoints  []int                 // zero-based step indices to halt before, in Continue mode

	// Runtime context GitHub injects in real CI that a clean local runner lacks
	// — all seed-and-be-honest surfaces (CLAUDE.md §4), each with a transparency line.
	GitHubToken string            // GITHUB_TOKEN → github.token (and mirrored into secrets.GITHUB_TOKEN); empty falls back to Secrets["GITHUB_TOKEN"]
	Inputs      map[string]string // workflow_dispatch/workflow_call inputs.* (user values; act applies declared defaults + typing on top)
	EventPath   string            // path to a github.event payload JSON; empty = "{}" (plus any Inputs)
	Repository  string            // override github.repository (env GITHUB_REPOSITORY); empty = act derives from local git
	Ref         string            // override github.ref (env GITHUB_REF); empty = act derives from local git
	Sha         string            // override github.sha (env SHA_REF, act's read key); empty = act derives from local git
	Actor       string            // override github.actor (Config.Actor); empty = act's "nektos/act" placeholder
}

// When marks which side of a step's main executor a pause occurred on. It mirrors
// the fork's runner.BarrierWhen but keeps act's type out of frontend code.
type When int

const (
	Before When = iota // before the step's main executor ran
	After              // after the step's main executor returned
)

func (w When) String() string {
	if w == After {
		return "after"
	}
	return "before"
}

// PauseEvent is emitted when the run halts at a step boundary.
type PauseEvent struct {
	When  When        // before or after the step's main executor
	Index int         // zero-based step index within the job
	Step  *model.Step // the step at this boundary
	Err   error       // for When==After: the step's error, or nil
}

// mode is the run policy consulted at each barrier.
type mode int

const (
	modeStep     mode = iota // halt at every barrier
	modeContinue             // pass through; halt only on a breakpoint or break-on-error
)

type control struct {
	mode  mode
	abort bool
}

// Session is one debug run of a single job. Construct with New, then Start.
type Session struct {
	jobID          string
	steps          []*model.Step
	needs          []NeedsSummary       // how the selected job's needs were satisfied locally (for transparency)
	withDeps       bool                 // upstream needs run for real before the target job
	isolatedWS     bool                 // workspace is an empty temp dir (no user repo) → local actions won't resolve
	workspace      string               // the bind-mounted workspace path (empty unless -workdir given)
	interceptors   []stepInterceptor    // neutralize-and-substitute hooks (checkout, cloud-auth), driven by the barrier
	checkoutLabels []string             // intercepted default-checkout labels, for transparency
	checkoutSource string               // host tree copied into the workspace at checkout (empty unless copy mode)
	configSummary  ConfigSummary        // redacted names of supplied secrets/vars/env (for transparency)
	gcpSummary     GCPSummary           // redacted view of the GCP identity substitution (for transparency)
	awsSummary     AWSSummary           // redacted view of the AWS identity substitution (for transparency)
	tokenSummary   TokenSummary         // how github.token was satisfied (for transparency)
	inputsSummary  InputsSummary        // declared/supplied workflow inputs (for transparency)
	eventSummary   EventSummary         // the github.event payload backing the run (for transparency)
	ghcSummary     GitHubContextSummary // resolved github.* runtime context (for transparency)
	runner         runner.Runner
	plan           *model.Plan
	tmpDir         string // non-empty if we created (and must clean up) the workdir
	eventFile      string // non-empty if we wrote (and must clean up) a merged event.json

	pauses  chan PauseEvent
	resume  chan control
	logs    chan string
	factory *logFactory
	done    chan struct{}

	mu           sync.Mutex
	curMode      mode
	breakpoints  map[int]bool
	breakOnErr   bool
	curEnv       map[string]string // live env at the current pause (nil while running)
	curContainer string            // job container name at the current pause
	curStep      *model.Step       // live step at the current pause (for editing)
	curWhen      When              // boundary of the current pause
	curRerun     func(context.Context) error
	curCtx       context.Context //nolint:containedctx // the barrier's exec ctx, used only to re-run a step while paused; cleared on resume

	err error // run result, valid once Done is closed
}

// New parses the workflow, plans the chosen job, and wires the pause barrier. It
// does not start execution; call Start. v0.1 debugs a single job: if the event
// plans more than one, Options.JobID must pick one (else a MultipleJobsError
// lists the choices).
func New(opts Options) (*Session, error) {
	if opts.WorkflowPath == "" {
		return nil, errors.New("debugger: WorkflowPath is required")
	}
	if opts.EventName == "" {
		opts.EventName = "push"
	}
	if opts.Image == "" {
		opts.Image = "catthehacker/ubuntu:act-latest"
	}

	planner, err := model.NewWorkflowPlanner(opts.WorkflowPath, true, false)
	if err != nil {
		return nil, fmt.Errorf("debugger: planner: %w", err)
	}
	plan, err := planner.PlanEvent(opts.EventName)
	if err != nil {
		return nil, fmt.Errorf("debugger: plan event %q: %w", opts.EventName, err)
	}
	run, err := selectRun(plan, opts.JobID)
	if err != nil {
		return nil, err
	}

	// Two execution modes. With WithDeps we run the full plan: act executes the
	// upstream jobs for real (so needs.* are genuine), and the barrier halts only
	// on the target job's steps (see shouldHalt's JobID gate). Otherwise we
	// isolate: act's planners expand the dependency closure (PlanEvent plans every
	// job; even PlanJob pulls in job.Needs() transitively) and NewPlanExecutor runs
	// the whole plan, so we hand it a plan holding just this one run, and seed the
	// needs context the job reads straight from the workflow model.
	var execPlan *model.Plan
	var needs []NeedsSummary
	if opts.WithDeps {
		execPlan = plan
		needs = liveNeeds(run)
	} else {
		execPlan = &model.Plan{Stages: []*model.Stage{{Runs: []*model.Run{run}}}}
		// Unseeded outputs are simply absent → empty, exactly as GitHub resolves a
		// non-existent output; the result defaults to success so typical
		// `if: needs.x.result == 'success'` gates don't skip the whole job.
		needs = seedNeeds(run, opts.Needs)
	}

	steps := run.Job().Steps

	// Step interceptors neutralize step classes that can't run faithfully on a
	// local runner — rewriting them to no-ops and substituting the real local effect
	// at the step's position (see barrier). Each is built once here; the barrier and
	// the container wiring then iterate s.interceptors uniformly, so adding AWS/Azure
	// is one more builder + append.
	//
	// Faithful local checkout: a default `actions/checkout` (no ref/repository/path)
	// would clone a remote over the workspace, losing your local changes. Rewrite it
	// to a no-op now; when we have a source tree the barrier populates the workspace
	// from it at the checkout step's position — so steps before checkout still see an
	// empty workspace, exactly as on GitHub. Checkouts with inputs stay a real clone.
	// The scan must precede the workspace-mode decision below (it branches on whether
	// there's a checkout to satisfy).
	checkouts := interceptSteps(steps, isDefaultCheckout, checkoutNoopMsg, nil)

	// Ambient GCP identity substitution (CLAUDE.md §4): a federated
	// google-github-actions/auth can't mint a GitHub OIDC token locally, so it would
	// fail and kill the job. The interceptor rewrites it to a no-op and (given ambient
	// creds) injects them at its position. Built even without credentials so the auth
	// step is still neutralized and non-cloud steps stay debuggable.
	gcpItc, gcpSummary := buildGCPInterceptor(opts.GCP, steps)

	// Ambient AWS identity substitution (CLAUDE.md §4), same shape as GCP: a federated
	// aws-actions/configure-aws-credentials can't assume a role via a GitHub OIDC token
	// locally, so the interceptor rewrites it to a no-op and (given ambient creds)
	// injects them — and the step's declared aws-region — at its position.
	awsItc, awsSummary := buildAWSInterceptor(opts.AWS, steps)

	// Runtime context GitHub injects that a clean local runner lacks (CLAUDE.md §4):
	// GITHUB_TOKEN, workflow inputs, the event payload, and the github.* context.
	// Each is seeded here and reported via a transparency line; none needs a fork patch.
	secrets := orEmpty(opts.Secrets)
	token, tokenSummary := resolveToken(opts.GitHubToken, secrets) // also mirrors token into secrets.GITHUB_TOKEN
	eventPath, eventFile, eventSummary, err := buildEvent(opts)
	if err != nil {
		return nil, err
	}
	inputsSummary := summarizeInputs(run.Workflow, opts.EventName, opts.Inputs)
	ghcEnv, ghcSummary := buildGitHubContext(opts)

	workdir := opts.Workdir
	var tmpDir, bindWS, checkoutSource string
	switch {
	case workdir != "":
		// explicit bind: the mounted tree already holds the code, so an intercepted
		// checkout is a pure no-op (no copy). act maps the container action path
		// from Config.Workdir, so it must be absolute (act's CLI abs's it too).
		if workdir, err = filepath.Abs(workdir); err != nil {
			return nil, fmt.Errorf("debugger: workdir: %w", err)
		}
		bindWS = workdir
	case len(checkouts) > 0:
		// copy the source tree into the workspace at checkout time (no host mutation).
		src := opts.Source
		if src == "" {
			if src, err = os.Getwd(); err != nil {
				return nil, fmt.Errorf("debugger: source dir: %w", err)
			}
		}
		if workdir, err = filepath.Abs(src); err != nil {
			return nil, fmt.Errorf("debugger: source dir: %w", err)
		}
		checkoutSource = workdir
	default:
		// no workspace needed: an empty temp dir keeps the repo (incl. the act
		// submodule) out of the container.
		if workdir, err = os.MkdirTemp("", "actl-"); err != nil {
			return nil, fmt.Errorf("debugger: temp workdir: %w", err)
		}
		tmpDir = workdir
	}

	// Assemble the interceptor list now the workspace mode is known: a copy-mode
	// checkout populates the workspace at its position (CopyWorkdir), while a bind/
	// temp workspace already holds the code so the rewritten checkout is a pure no-op
	// (inject nil). Only attach interceptors that actually own a step.
	var interceptors []stepInterceptor
	if len(checkouts) > 0 {
		var inject func(context.Context, runner.StepBarrierInfo) error
		if checkoutSource != "" {
			inject = func(ctx context.Context, info runner.StepBarrierInfo) error {
				if info.CopyWorkdir == nil {
					return nil
				}
				return info.CopyWorkdir(ctx)
			}
		}
		interceptors = append(interceptors, stepInterceptor{name: "checkout", steps: checkouts, inject: inject})
	}
	if len(gcpItc.steps) > 0 {
		interceptors = append(interceptors, gcpItc)
	}
	if len(awsItc.steps) > 0 {
		interceptors = append(interceptors, awsItc)
	}

	breakpoints := make(map[int]bool, len(opts.Breakpoints))
	for _, i := range opts.Breakpoints {
		breakpoints[i] = true
	}

	s := &Session{
		jobID:          run.JobID,
		steps:          steps,
		needs:          needs,
		withDeps:       opts.WithDeps,
		isolatedWS:     tmpDir != "", // we created an empty temp workspace (no user repo)
		workspace:      bindWS,
		interceptors:   interceptors,
		checkoutLabels: stepLabelsOf(steps, checkouts),
		checkoutSource: checkoutSource,
		configSummary:  summarizeConfig(opts.Secrets, opts.Vars, opts.Env),
		gcpSummary:     gcpSummary,
		awsSummary:     awsSummary,
		tokenSummary:   tokenSummary,
		inputsSummary:  inputsSummary,
		eventSummary:   eventSummary,
		ghcSummary:     ghcSummary,
		plan:           execPlan,
		tmpDir:         tmpDir,
		eventFile:      eventFile,
		pauses:         make(chan PauseEvent),
		resume:         make(chan control),
		logs:           make(chan string, 1024),
		done:           make(chan struct{}),
		curMode:        modeStep, // stop at entry (before the first step)
		breakpoints:    breakpoints,
		breakOnErr:     opts.BreakOnError,
	}
	s.factory = &logFactory{w: &lineWriter{sink: s.logs, stop: s.done, drop: isGitContextNoise}}

	cfg := &runner.Config{
		Workdir:    workdir,
		EventName:  opts.EventName,
		EventPath:  eventPath,            // user-supplied or merged-with-inputs event.json ("" = act synthesizes)
		Inputs:     orEmpty(opts.Inputs), // ignored by act when EventPath is set; harmless otherwise
		Token:      token,                // github.token (mirrored into secrets.GITHUB_TOKEN above)
		Actor:      opts.Actor,           // github.actor ("" → act's "nektos/act" placeholder)
		Platforms:  map[string]string{"ubuntu-latest": opts.Image},
		AutoRemove: true,
		LogOutput:  true,                                // route step stdout through the logger (captured below)
		Env:        mergeEnv(orEmpty(opts.Env), ghcEnv), // github.* overrides (GITHUB_REPOSITORY/GITHUB_REF/SHA_REF) layered onto -env
		Secrets:    secrets,
		Vars:       orEmpty(opts.Vars),
		// A user-supplied workdir is bind-mounted: act's copy mode leaves the
		// workspace volume empty (it never populates it), so local `uses: ./…`
		// actions only resolve when the dir is mounted. The empty-workdir default
		// uses an isolated temp dir and never mounts the user's tree.
		BindWorkdir:  opts.Workdir != "",
		UseGitIgnore: true, // act default: don't copy .gitignored paths into the container
		// act's CLI defaults this; we build Config ourselves, and an empty value
		// makes the github server URL "https://" (no host), so remote `uses:`
		// actions fail to clone. Set the public default explicitly.
		GitHubInstance: "github.com",
		StepBarrier:    s.barrier,
	}
	// Some interceptors need docker flags on the job container (e.g. GCP mounts the
	// ambient ADC file read-only). act parses ContainerOptions as docker flags, so
	// these reach the container without a fork patch.
	for _, itc := range interceptors {
		if itc.container == "" {
			continue
		}
		if cfg.ContainerOptions == "" {
			cfg.ContainerOptions = itc.container
		} else {
			cfg.ContainerOptions += " " + itc.container
		}
	}
	r, err := runner.New(cfg)
	if err != nil {
		s.cleanup()
		return nil, fmt.Errorf("debugger: new runner: %w", err)
	}
	s.runner = r
	return s, nil
}

// JobID is the id of the job being debugged.
func (s *Session) JobID() string { return s.jobID }

// Steps returns the job's steps in declaration order (for rendering a step list
// before the run starts).
func (s *Session) Steps() []*model.Step { return s.steps }

// NeedsSummary reports how the selected job's needs were satisfied (seeded in
// isolation, or live with --with-deps), for a transparency line. Empty when the
// job has no needs.
func (s *Session) NeedsSummary() []NeedsSummary { return s.needs }

// WithDeps reports whether the job's upstream needs run for real before it.
func (s *Session) WithDeps() bool { return s.withDeps }

// WorkspaceIsolated reports whether the run uses an empty temp workspace (no user
// repo), in which case local `uses: ./…` actions and checkout can't resolve.
func (s *Session) WorkspaceIsolated() bool { return s.isolatedWS }

// Workspace returns the bind-mounted workspace path, or empty when isolated. When
// non-empty, steps run in the container can write to this path on the host.
func (s *Session) Workspace() string { return s.workspace }

// CheckoutSteps returns the labels of default `actions/checkout` steps that were
// intercepted (rewritten to no-ops), for a transparency line. Empty when none.
func (s *Session) CheckoutSteps() []string { return s.checkoutLabels }

// CheckoutSource is the host working tree copied into the workspace at checkout
// time; empty when there's nothing to copy (e.g. a -workdir mount already holds
// the code, or there were no intercepted checkouts).
func (s *Session) CheckoutSource() string { return s.checkoutSource }

// ConfigSummary returns the redacted names of the secrets/vars/env supplied to
// the run (values withheld), for a transparency line.
func (s *Session) ConfigSummary() ConfigSummary { return s.configSummary }

// GCPSummary returns the redacted view of the GCP identity substitution (which
// auth steps were intercepted, the federation target vs the local identity), for a
// transparency line. Its Steps are empty when the job has no auth step.
func (s *Session) GCPSummary() GCPSummary { return s.gcpSummary }

// AWSSummary returns the redacted view of the AWS identity substitution (which auth
// steps were intercepted, the role+region vs the local identity), for a transparency
// line. Its Steps are empty when the job has no aws-actions/configure-aws-credentials step.
func (s *Session) AWSSummary() AWSSummary { return s.awsSummary }

// TokenSummary reports how github.token was satisfied (set from a flag/secret, or
// absent), for a transparency line.
func (s *Session) TokenSummary() TokenSummary { return s.tokenSummary }

// InputsSummary reports the workflow's declared inputs and which were supplied vs
// defaulted, for a transparency line. Declared is false when the event takes no inputs.
func (s *Session) InputsSummary() InputsSummary { return s.inputsSummary }

// EventSummary reports the github.event payload backing the run, for a transparency line.
func (s *Session) EventSummary() EventSummary { return s.eventSummary }

// GitHubContextSummary reports the resolved github.* runtime context (repository/
// ref/sha/actor and any overrides), for a transparency line.
func (s *Session) GitHubContextSummary() GitHubContextSummary { return s.ghcSummary }

// LocalUsesSteps returns the labels of steps that reference a local action
// (`uses: ./…`) — these need a real workspace (run with a workdir set).
func (s *Session) LocalUsesSteps() []string {
	var out []string
	for _, st := range s.steps {
		if strings.HasPrefix(st.Uses, "./") {
			out = append(out, st.String())
		}
	}
	return out
}

// Start launches the run in the background. The run halts at the first barrier
// (before the first step) per the default Step mode; drive it via the control
// methods and Pauses. Done is closed when the run finishes.
func (s *Session) Start(ctx context.Context) {
	// Route act's logging into our sink (see logFactory) so it never reaches the
	// terminal a frontend may own. WithJobLoggerFactory covers per-job/step
	// loggers; the base WithLogger covers anything logged before the job logger.
	ctx = common.WithLogger(runner.WithJobLoggerFactory(ctx, s.factory), s.factory.WithJobLogger())
	go func() {
		defer close(s.done)
		defer s.cleanup()
		s.err = s.runner.NewPlanExecutor(s.plan)(ctx)
	}()
}

// Logs delivers act's output line by line (job + step logs, with secrets masked).
// Drain it concurrently; it is buffered but a frontend should keep reading.
func (s *Session) Logs() <-chan string { return s.logs }

// Pauses delivers a PauseEvent each time the run halts. The run stays blocked
// until a control method (Step/Continue/Abort) is called.
func (s *Session) Pauses() <-chan PauseEvent { return s.pauses }

// Done is closed when the run has finished (successfully, with an error, or
// aborted). Read Err afterwards.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err returns the run result. Only valid once Done is closed.
func (s *Session) Err() error { return s.err }

// Step resumes and halts again at the next barrier.
func (s *Session) Step() { s.send(control{mode: modeStep}) }

// Continue resumes and runs until a breakpoint, a break-on-error stop, or the
// end of the job.
func (s *Session) Continue() { s.send(control{mode: modeContinue}) }

// Abort resumes with an order to stop the job (the run ends with ErrAborted).
func (s *Session) Abort() { s.send(control{mode: modeContinue, abort: true}) }

// SetBreakpoint toggles a halt before the step at the given zero-based index
// (consulted in Continue mode). Safe to call before Start or while paused.
func (s *Session) SetBreakpoint(index int, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if on {
		s.breakpoints[index] = true
	} else {
		delete(s.breakpoints, index)
	}
}

// send delivers a control to a waiting barrier, or no-ops if the run is already
// done (so a stray control after completion can't deadlock the caller).
func (s *Session) send(c control) {
	select {
	case s.resume <- c:
	case <-s.done:
	}
}

// barrier is the StepBarrier hook installed on runner.Config. It runs in the act
// goroutine. If policy says pass, it returns immediately; if policy says halt, it
// emits a PauseEvent and blocks until a control arrives.
func (s *Session) barrier(ctx context.Context, info runner.StepBarrierInfo) error {
	// At each step's "before" boundary, run any interceptor that owns this step (in
	// the debugged job): a default checkout copies the working tree here, a cloud-auth
	// injects ambient credentials into the live env here — earlier steps don't see the
	// effect, later steps do, exactly as the real step would apply it. info.Env is the
	// same live rc.Env map SetEnv mutates, so an env change propagates to later execs.
	// The JobID gate keeps --with-deps upstream jobs out; adding AWS/Azure is another
	// entry in s.interceptors, this loop is unchanged.
	if info.When == runner.BarrierBefore && info.JobID == s.jobID {
		for _, itc := range s.interceptors {
			if itc.inject != nil && itc.steps[info.Index] {
				if err := itc.inject(ctx, info); err != nil {
					s.logf("actl: %s substitution failed: %v", itc.name, err)
				}
			}
		}
	}

	if !s.shouldHalt(info) {
		return nil
	}

	// Publish the live inspection state for the duration of the pause. act is
	// blocked here, so the env map is stable to read.
	s.mu.Lock()
	s.curEnv = info.Env
	s.curContainer = info.ContainerName
	s.curStep = info.Step
	s.curWhen = toWhen(info.When)
	s.curRerun = info.Rerun
	s.curCtx = ctx
	s.mu.Unlock()

	ev := PauseEvent{When: toWhen(info.When), Index: info.Index, Step: info.Step, Err: info.Err}
	select {
	case s.pauses <- ev:
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case c := <-s.resume:
		s.mu.Lock()
		s.curMode = c.mode
		s.curEnv = nil
		s.curContainer = ""
		s.curStep = nil
		s.curRerun = nil
		s.curCtx = nil
		s.mu.Unlock()
		if c.abort {
			return ErrAborted
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Env returns a copy of the job's environment captured at the current pause, or
// nil while the run is executing. Inspection is only meaningful while paused.
func (s *Session) Env() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.curEnv == nil {
		return nil
	}
	out := make(map[string]string, len(s.curEnv))
	for k, v := range s.curEnv {
		out[k] = v
	}
	return out
}

// ContainerName is the docker name of the live job container at the current
// pause (empty while running or if no container is in use). A frontend can drop
// an interactive shell into it; the core stays out of the terminal (CLAUDE.md §5).
func (s *Session) ContainerName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.curContainer
}

// CurrentRun returns the paused step's `run:` script (empty for a `uses:` step
// or while running) — for pre-filling an editor before Rerun.
func (s *Session) CurrentRun() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.curStep == nil {
		return ""
	}
	return s.curStep.Run
}

// SetRun replaces the paused step's `run:` script in memory (the file on disk is
// untouched). The next Rerun picks it up. No-op while running.
func (s *Session) SetRun(script string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.curStep != nil {
		s.curStep.Run = script
	}
}

// SetEnv sets or overrides a job env var in memory. The next Rerun (and later
// steps) see it. No-op while running.
func (s *Session) SetEnv(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.curEnv != nil {
		s.curEnv[key] = value
	}
}

// CanRerun reports whether Rerun is available — only after a step's main has run
// (rerunning before it would double-execute on resume).
func (s *Session) CanRerun() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.curRerun != nil && s.curWhen == After
}

// Rerun re-executes the paused step's main in the live container, picking up any
// SetRun/SetEnv edits. It blocks until the step finishes; output arrives on
// Logs(). Only valid while paused after a step has run.
func (s *Session) Rerun() error {
	s.mu.Lock()
	rerun, rctx, when := s.curRerun, s.curCtx, s.curWhen
	s.mu.Unlock()
	if rerun == nil {
		return errors.New("debugger: not paused")
	}
	if when != After {
		return errors.New("debugger: rerun is available only after a step has run (step onto it first)")
	}
	return rerun(rctx)
}

// shouldHalt is the halt/pass policy. Step mode halts everywhere; Continue mode
// halts only at a breakpoint (before a step) or, with break-on-error, after a
// step that errored.
func (s *Session) shouldHalt(info runner.StepBarrierInfo) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	// In --with-deps the full plan runs; the upstream jobs must run to completion
	// without pausing. Only ever halt on the job being debugged. (In isolation the
	// only job is the target, so this is a no-op.)
	if info.JobID != "" && info.JobID != s.jobID {
		return false
	}
	if s.curMode == modeStep {
		return true
	}
	if info.When == runner.BarrierAfter && info.Err != nil && s.breakOnErr {
		return true
	}
	if info.When == runner.BarrierBefore && s.breakpoints[info.Index] {
		return true
	}
	return false
}

// isDefaultCheckout reports whether a step is `actions/checkout` with no input
// that changes which code or where it lands (ref/repository/path). Only those are
// safe to satisfy from the local working tree; anything else stays a real clone.
func isDefaultCheckout(st *model.Step) bool {
	if st.Uses != "actions/checkout" && !strings.HasPrefix(st.Uses, "actions/checkout@") {
		return false
	}
	for _, k := range []string{"ref", "repository", "path"} {
		if st.With[k] != "" {
			return false
		}
	}
	return true
}

// stepInterceptor neutralizes a class of steps that can't run faithfully on a local
// runner (a default actions/checkout, a federated cloud-auth) and substitutes the
// real local effect at the step's position. Built once in New; the barrier and the
// container wiring then iterate []stepInterceptor uniformly, so AWS/Azure are one
// more builder + append — no new barrier code and no new Session fields.
type stepInterceptor struct {
	name      string                                                       // "checkout", "gcp-auth" — names the non-fatal failure log
	steps     map[int]bool                                                 // indices it rewrote to no-ops, within the debugged job
	inject    func(ctx context.Context, info runner.StepBarrierInfo) error // run at BarrierBefore on an owned index for the target job; nil = nothing to substitute
	container string                                                       // docker flags appended to Config.ContainerOptions; "" = none
}

// No-op scripts the rewritten steps run in place of their real action (so the step
// still shows in logs that actl handled it).
const (
	checkoutNoopMsg = `echo "actl: checkout intercepted — using your local working tree"`
	gcpAuthNoopMsg  = `echo "actl: GCP auth intercepted — using ambient identity"`
	awsAuthNoopMsg  = `echo "actl: AWS auth intercepted — using ambient identity"`
)

// interceptSteps rewrites every step matching `match` to a no-op echoing `msg`,
// preserving its label (so the rewritten step still shows its origin), and returns
// the rewritten indices. `capture`, if non-nil, runs on each matched step BEFORE its
// Uses/With are cleared (e.g. to read a cloud-auth step's federation target). This is
// the shared mechanic every interceptor's scan/rewrite is built from.
func interceptSteps(steps []*model.Step, match func(*model.Step) bool, msg string, capture func(*model.Step)) map[int]bool {
	out := map[int]bool{}
	for i, st := range steps {
		if !match(st) {
			continue
		}
		if capture != nil {
			capture(st)
		}
		if st.Name == "" {
			st.Name = st.Uses // keep the original label visible after we clear Uses
		}
		st.Uses = ""
		st.With = nil
		st.Run = msg
		out[i] = true
	}
	return out
}

// isGCPAuth reports whether a step uses google-github-actions/auth (any version).
func isGCPAuth(st *model.Step) bool {
	return st.Uses == "google-github-actions/auth" || strings.HasPrefix(st.Uses, "google-github-actions/auth@")
}

// buildGCPInterceptor scans and rewrites each google-github-actions/auth step to a
// no-op (so it doesn't try to federate against a GitHub OIDC issuer that doesn't
// exist locally), then assembles the substitution: the ambient credential env to
// inject at the step's position and the read-only ADC volume mount. Returns the
// interceptor plus a redacted GCPSummary for the transparency line. With no auth step
// the interceptor owns nothing (and isn't attached); with a step but no identity it
// still neutralizes the step so the job survives, injecting nothing.
func buildGCPInterceptor(id *GCPIdentity, steps []*model.Step) (stepInterceptor, GCPSummary) {
	var targets []string
	gcpSteps := interceptSteps(steps, isGCPAuth, gcpAuthNoopMsg, func(st *model.Step) {
		targets = append(targets, gcpTarget(st))
	})
	env, summary := buildGCP(id, gcpSteps, targets)
	summary.Steps = stepLabelsOf(steps, gcpSteps)

	itc := stepInterceptor{name: "gcp-auth", steps: gcpSteps, container: gcpBind(id, len(gcpSteps) > 0)}
	if len(env) > 0 {
		// info.Env is the live job env map (the same one SetEnv mutates), so writing
		// here propagates the credential contract to subsequent step execs.
		itc.inject = func(_ context.Context, info runner.StepBarrierInfo) error {
			for k, v := range env {
				info.Env[k] = v
			}
			return nil
		}
	}
	return itc, summary
}

// gcpTarget describes the federation an auth step declares, for the transparency
// line — read before we clear With. Falls back to a generic label if unset.
func gcpTarget(st *model.Step) string {
	sa := st.With["service_account"]
	wip := st.With["workload_identity_provider"]
	switch {
	case sa != "" && wip != "":
		return sa + " via " + wip
	case sa != "":
		return sa
	case wip != "":
		return wip
	default:
		return "(federated identity)"
	}
}

// buildGCP assembles the credential env injected at the auth step's position and a
// redacted summary for the transparency line. With no identity (id == nil) it
// returns no env but still summarizes the intercepted steps, so the UI can say the
// steps were neutralized but cloud calls will fail.
func buildGCP(id *GCPIdentity, steps map[int]bool, targets []string) (map[string]string, GCPSummary) {
	summary := GCPSummary{Targets: targets}
	if len(steps) == 0 {
		return nil, GCPSummary{} // no auth step → nothing to report
	}
	if id == nil {
		return nil, summary
	}
	env := map[string]string{}
	if id.CredentialFile != "" {
		// Client libraries (Go/Python/terraform's google provider) discover ADC here.
		// We deliberately do NOT set GOOGLE_GHA_CREDS_PATH: setup-gcloud consumes it to
		// run `gcloud auth login --cred-file`, which rejects the authorized_user ADC
		// that `gcloud auth application-default login` produces ("only external/service
		// account JSON supported"). The access token below authenticates gcloud
		// universally instead, so that path is both unnecessary and harmful here.
		env["GOOGLE_APPLICATION_CREDENTIALS"] = gcpCredsContainerPath
		summary.File = true
	}
	if id.AccessToken != "" {
		// Authenticates gcloud/gsutil/bq directly (any ADC type), and satisfies
		// setup-gcloud's own `isAuthenticated()` check so it doesn't warn.
		env["CLOUDSDK_AUTH_ACCESS_TOKEN"] = id.AccessToken
		summary.Token = true
	}
	// We deliberately do NOT inject a project. In real CI the project comes from the
	// workflow (the auth step's project_id input, or an explicit --project / env on
	// the command); fabricating one from the host's gcloud config wouldn't exist on
	// GitHub. If a run needs GOOGLE_CLOUD_PROJECT, supply it via the generic env path
	// (.env / -env), same as any other env var.
	summary.Account = id.Account
	if len(env) == 0 {
		env = nil
	}
	return env, summary
}

// gcpBind returns the docker volume option that mounts the ambient ADC file
// read-only into the job container, or "" when there's no file to mount or no auth
// step to satisfy. The mount path matches the GOOGLE_APPLICATION_CREDENTIALS env.
func gcpBind(id *GCPIdentity, hasAuthSteps bool) string {
	if id == nil || id.CredentialFile == "" || !hasAuthSteps {
		return ""
	}
	return fmt.Sprintf("-v %s:%s:ro", id.CredentialFile, gcpCredsContainerPath)
}

// isAWSAuth reports whether a step uses aws-actions/configure-aws-credentials (any version).
func isAWSAuth(st *model.Step) bool {
	return st.Uses == "aws-actions/configure-aws-credentials" || strings.HasPrefix(st.Uses, "aws-actions/configure-aws-credentials@")
}

// awsTarget describes the federation an auth step declares (role + region), for the
// transparency line — read before we clear With. Falls back to a generic label.
func awsTarget(st *model.Step) string {
	role := st.With["role-to-assume"]
	region := st.With["aws-region"]
	switch {
	case role != "" && region != "":
		return role + " in " + region
	case role != "":
		return role
	case region != "":
		return "region " + region
	default:
		return "(static credentials)"
	}
}

// buildAWSInterceptor scans and rewrites each aws-actions/configure-aws-credentials
// step to a no-op (so it doesn't try to assume a role via a GitHub OIDC token that
// can't be minted locally), then assembles the substitution: the ambient session
// credentials to inject at the step's position plus the region the step declared.
// Returns the interceptor and a redacted AWSSummary. AWS is env-only — no file mount
// — so unlike GCP the interceptor sets no container option.
func buildAWSInterceptor(id *AWSIdentity, steps []*model.Step) (stepInterceptor, AWSSummary) {
	var targets []string
	var region string
	awsSteps := interceptSteps(steps, isAWSAuth, awsAuthNoopMsg, func(st *model.Step) {
		targets = append(targets, awsTarget(st))
		// Capture the first declared literal region; the action exports it, so
		// reproducing it is faithful (unlike GCP's project, region is a declared input
		// here). Skip expressions — we'd inject the raw unevaluated string otherwise.
		if region == "" {
			if r := st.With["aws-region"]; r != "" && !strings.Contains(r, "${{") {
				region = r
			}
		}
	})
	env, summary := buildAWS(id, awsSteps, targets, region)
	summary.Steps = stepLabelsOf(steps, awsSteps)

	itc := stepInterceptor{name: "aws-auth", steps: awsSteps}
	if len(env) > 0 {
		// info.Env is the live job env map (the same one SetEnv mutates), so writing
		// here propagates the credential contract to subsequent step execs.
		itc.inject = func(_ context.Context, info runner.StepBarrierInfo) error {
			for k, v := range env {
				info.Env[k] = v
			}
			return nil
		}
	}
	return itc, summary
}

// buildAWS assembles the credential env injected at the auth step's position and a
// redacted summary for the transparency line. With no identity (id == nil) it returns
// no env but still summarizes the intercepted steps, so the UI can say the steps were
// neutralized but cloud calls will fail. The region (from the step's declared
// aws-region) is injected only alongside credentials — region without creds is useless.
func buildAWS(id *AWSIdentity, steps map[int]bool, targets []string, region string) (map[string]string, AWSSummary) {
	summary := AWSSummary{Targets: targets, Region: region}
	if len(steps) == 0 {
		return nil, AWSSummary{} // no auth step → nothing to report
	}
	if id == nil {
		return nil, summary
	}
	env := map[string]string{}
	if id.AccessKeyID != "" && id.SecretAccessKey != "" {
		env["AWS_ACCESS_KEY_ID"] = id.AccessKeyID
		env["AWS_SECRET_ACCESS_KEY"] = id.SecretAccessKey
		summary.Creds = true
		if id.SessionToken != "" {
			env["AWS_SESSION_TOKEN"] = id.SessionToken // temporary (SSO / assumed-role) creds
		}
		if region != "" {
			env["AWS_REGION"] = region
			env["AWS_DEFAULT_REGION"] = region
			summary.RegionSet = true
		}
	}
	summary.Account = id.Account
	if len(env) == 0 {
		env = nil
	}
	return env, summary
}

// stepLabelsOf returns the labels of the steps whose indices are set in mark, in
// order — used to name intercepted steps (checkout, GCP auth) for transparency.
func stepLabelsOf(steps []*model.Step, mark map[int]bool) []string {
	var out []string
	for i, st := range steps {
		if mark[i] {
			out = append(out, st.String())
		}
	}
	return out
}

// logf pushes a one-off line into the captured-output stream (non-blocking, so it
// never stalls the act goroutine), for notices that aren't act's own logs.
func (s *Session) logf(format string, args ...any) {
	select {
	case s.logs <- fmt.Sprintf(format, args...):
	default:
	}
}

func (s *Session) cleanup() {
	if s.tmpDir != "" {
		_ = os.RemoveAll(s.tmpDir)
	}
	if s.eventFile != "" {
		_ = os.Remove(s.eventFile)
	}
}

// selectRun picks the single job to debug. With jobID set it returns that job
// (or an error naming the available ones); without it, it returns the sole job,
// or a MultipleJobsError so a frontend can prompt.
func selectRun(plan *model.Plan, jobID string) (*model.Run, error) {
	var runs []*model.Run
	for _, stage := range plan.Stages {
		runs = append(runs, stage.Runs...)
	}
	if len(runs) == 0 {
		return nil, errors.New("debugger: no jobs to run for this event")
	}
	if jobID != "" {
		for _, r := range runs {
			if r.JobID == jobID {
				return r, nil
			}
		}
		return nil, fmt.Errorf("debugger: job %q not found; available jobs: %s", jobID, strings.Join(jobIDsOf(runs), ", "))
	}
	if len(runs) == 1 {
		return runs[0], nil
	}
	return nil, &MultipleJobsError{Jobs: jobIDsOf(runs)}
}

func jobIDsOf(runs []*model.Run) []string {
	ids := make([]string, 0, len(runs))
	for _, r := range runs {
		ids = append(ids, r.JobID)
	}
	sort.Strings(ids)
	return ids
}

// liveNeeds lists the selected job's needs for a transparency line in --with-deps
// mode, where the upstream jobs run for real (no seeding).
func liveNeeds(run *model.Run) []NeedsSummary {
	job := run.Job()
	if job == nil {
		return nil
	}
	out := make([]NeedsSummary, 0, len(job.Needs()))
	for _, name := range job.Needs() {
		out = append(out, NeedsSummary{Job: name, Live: true})
	}
	return out
}

// seedNeeds writes the selected job's upstream needs into the workflow model so
// act resolves needs.<job>.* locally (it reads them straight from there). It
// replaces each upstream job's Outputs with only the user-supplied keys — so an
// unseeded output is absent and resolves to empty, exactly like a non-existent
// one in GitHub — and defaults the result to success unless overridden. Returns
// a summary for a transparency line.
func seedNeeds(run *model.Run, seeded map[string]NeedsInput) []NeedsSummary {
	job := run.Job()
	if job == nil {
		return nil
	}
	var out []NeedsSummary
	for _, name := range job.Needs() {
		upstream := run.Workflow.GetJob(name)
		if upstream == nil {
			continue
		}
		in := seeded[name]
		result := in.Result
		assumed := result == ""
		if assumed {
			result = "success"
		}
		outputs := make(map[string]string, len(in.Outputs))
		for k, v := range in.Outputs {
			outputs[k] = v
		}
		upstream.Outputs = outputs
		upstream.Result = result
		out = append(out, NeedsSummary{Job: name, Result: result, Assumed: assumed, Outputs: outputs})
	}
	return out
}

func toWhen(w runner.BarrierWhen) When {
	if w == runner.BarrierAfter {
		return After
	}
	return Before
}

// summarizeConfig collects the sorted key names of the supplied secrets/vars/env
// for a redacted transparency line — names only, values never retained here.
func summarizeConfig(secrets, vars, env map[string]string) ConfigSummary {
	return ConfigSummary{
		Secrets: sortedKeys(secrets),
		Vars:    sortedKeys(vars),
		Env:     sortedKeys(env),
	}
}

func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// resolveToken decides the value behind github.token. An explicit flag wins; else
// it falls back to a GITHUB_TOKEN already present in secrets (act's CLI does the same
// at cmd/root.go). When a token is found it is mirrored into secrets["GITHUB_TOKEN"]
// so github.token and secrets.GITHUB_TOKEN stay equal, as they are on real GitHub.
// secrets is mutated in place (it is the map handed to runner.Config.Secrets).
func resolveToken(flag string, secrets map[string]string) (string, TokenSummary) {
	token, source := flag, "flag"
	if token == "" {
		token, source = secrets["GITHUB_TOKEN"], "secret"
	}
	if token == "" {
		return "", TokenSummary{}
	}
	secrets["GITHUB_TOKEN"] = token
	return token, TokenSummary{Present: true, Source: source}
}

// buildEvent decides the github.event payload backing the run. GitHub injects an
// event payload; a clean local runner has none. The cases:
//   - no event file, no inputs → act synthesizes "{}" (eventPath empty)
//   - no event file, inputs     → act builds {"inputs": …} from Config.Inputs (eventPath empty)
//   - event file, no inputs     → use it as-is
//   - event file + inputs       → act ignores Config.Inputs once EventPath is set, so we
//     merge the inputs into the file's "inputs" and write a temp event.json
//
// Returns the eventPath to set on Config, a tmp path to clean up (or ""), and a summary.
func buildEvent(opts Options) (eventPath, tmpFile string, _ EventSummary, _ error) {
	summary := EventSummary{EventName: opts.EventName, Path: opts.EventPath, Synthetic: opts.EventPath == ""}
	if opts.EventPath == "" {
		return "", "", summary, nil // act synthesizes "{}" (+ Config.Inputs)
	}
	raw, err := os.ReadFile(opts.EventPath)
	if err != nil {
		return "", "", EventSummary{}, fmt.Errorf("debugger: event file: %w", err)
	}
	if len(opts.Inputs) == 0 {
		return opts.EventPath, "", summary, nil // use the file as-is
	}
	// Merge inputs into the event payload (act would otherwise drop Config.Inputs).
	var event map[string]any
	if err := json.Unmarshal(raw, &event); err != nil {
		return "", "", EventSummary{}, fmt.Errorf("debugger: event file %s: %w", opts.EventPath, err)
	}
	if event == nil {
		event = map[string]any{}
	}
	inputs, _ := event["inputs"].(map[string]any)
	if inputs == nil {
		inputs = map[string]any{}
	}
	for k, v := range opts.Inputs {
		inputs[k] = v
	}
	event["inputs"] = inputs
	merged, err := json.Marshal(event)
	if err != nil {
		return "", "", EventSummary{}, fmt.Errorf("debugger: merge event inputs: %w", err)
	}
	f, err := os.CreateTemp("", "actl-event-*.json")
	if err != nil {
		return "", "", EventSummary{}, fmt.Errorf("debugger: event temp: %w", err)
	}
	if _, err := f.Write(merged); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", "", EventSummary{}, fmt.Errorf("debugger: write event temp: %w", err)
	}
	_ = f.Close()
	return f.Name(), f.Name(), summary, nil
}

// summarizeInputs reports which of the workflow's declared inputs (for the planned
// event) the user supplied vs. left to act's declared-default fill, for a
// transparency line. act applies the defaults and typing itself (expression.go), so
// this is display-only — we never re-derive them.
func summarizeInputs(wf *model.Workflow, event string, provided map[string]string) InputsSummary {
	declared := declaredInputs(wf, event)
	s := InputsSummary{Declared: len(declared) > 0}
	if len(provided) > 0 {
		s.Provided = make(map[string]string, len(provided))
		for k, v := range provided {
			s.Provided[k] = v
		}
	}
	for name := range declared {
		if _, ok := provided[name]; !ok {
			s.Defaults = append(s.Defaults, name)
		}
	}
	sort.Strings(s.Defaults)
	return s
}

// declaredInputs returns the set of input names the workflow declares for the
// planned event (workflow_dispatch or workflow_call), or nil for other events.
func declaredInputs(wf *model.Workflow, event string) map[string]struct{} {
	if wf == nil {
		return nil
	}
	out := map[string]struct{}{}
	switch event {
	case "workflow_dispatch":
		if c := wf.WorkflowDispatchConfig(); c != nil {
			for k := range c.Inputs {
				out[k] = struct{}{}
			}
		}
	case "workflow_call":
		if c := wf.WorkflowCallConfig(); c != nil {
			for k := range c.Inputs {
				out[k] = struct{}{}
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// buildGitHubContext maps the github.* overrides to the env keys act reads when
// synthesizing the github context (github_context.go): GITHUB_REPOSITORY and
// GITHUB_REF survive because act only derives them when unset, and the sha is read
// from SHA_REF. Actor is handled via Config.Actor, not here. Returns the env to
// layer onto -env plus a summary for the transparency line. Unset fields are left
// for act to derive from local git (and the summary shows them empty).
func buildGitHubContext(opts Options) (map[string]string, GitHubContextSummary) {
	env := map[string]string{}
	var overridden []string
	if opts.Repository != "" {
		env["GITHUB_REPOSITORY"] = opts.Repository
		overridden = append(overridden, "repository")
	}
	if opts.Ref != "" {
		env["GITHUB_REF"] = opts.Ref
		overridden = append(overridden, "ref")
	}
	if opts.Sha != "" {
		env["SHA_REF"] = opts.Sha // act reads the sha from SHA_REF (github_context.go)
		overridden = append(overridden, "sha")
	}
	if opts.Actor != "" {
		overridden = append(overridden, "actor")
	}
	return env, GitHubContextSummary{
		Repository: opts.Repository,
		Ref:        opts.Ref,
		Sha:        shortSha(opts.Sha),
		Actor:      opts.Actor,
		Overridden: overridden,
	}
}

// shortSha trims a commit sha to 7 chars for display (leaves shorter strings as-is).
func shortSha(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// mergeEnv overlays add onto base (add wins) without mutating either; base is
// returned when add is empty.
func mergeEnv(base, add map[string]string) map[string]string {
	if len(add) == 0 {
		return base
	}
	out := make(map[string]string, len(base)+len(add))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range add {
		out[k] = v
	}
	return out
}
