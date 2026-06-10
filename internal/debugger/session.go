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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/client"
	"github.com/nektos/act/pkg/common"
	"github.com/nektos/act/pkg/container"
	"github.com/nektos/act/pkg/model"
	"github.com/nektos/act/pkg/runner"

	"github.com/ruzmuh/actl/internal/workflow"
)

// ErrAborted is the run error when the front-end aborts via Abort.
var ErrAborted = errors.New("debugger: run aborted by user")

// DockerUnavailableError is returned by New when the Docker daemon can't be
// reached. act execs every step into a real job container, so a running daemon
// is a hard prerequisite; surfacing it up front (with a friendly message) beats
// act's cryptic mid-run failure. Cause is the underlying connection/ping error.
type DockerUnavailableError struct{ Cause error }

func (e *DockerUnavailableError) Error() string {
	return fmt.Sprintf("Docker is not reachable — is the Docker daemon running? "+
		"(set DOCKER_HOST if it runs elsewhere): %v", e.Cause)
}

func (e *DockerUnavailableError) Unwrap() error { return e.Cause }

// dockerPreflight verifies the Docker daemon is reachable. GetDockerClient alone
// is not enough: client.New(FromEnv) constructs lazily and succeeds without ever
// contacting the daemon, so we Ping to actually confirm it's up, under a short
// timeout so a dead socket can't hang the startup.
func dockerPreflight() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cli, err := container.GetDockerClient(ctx)
	if err != nil {
		return &DockerUnavailableError{Cause: err}
	}
	defer cli.Close()
	if _, err := cli.Ping(ctx, client.PingOptions{}); err != nil {
		return &DockerUnavailableError{Cause: err}
	}
	return nil
}

// EnvOverlay is a per-`environment:` overlay of secrets/vars applied when the debugged
// job targets that deployment environment (GHA scopes secrets/vars by `environment:`).
// The host (cmd/actl) resolves each environment's secret-file/inline-vars into these
// flat maps; the core merges the matching one over the flat defaults (CLI overrides
// still win — see New).
type EnvOverlay struct {
	Secrets map[string]string
	Vars    map[string]string
}

// Options configures a debug Session. Only WorkflowPath is required.
type Options struct {
	WorkflowPath    string                     // path to the workflow file
	EventName       string                     // event to plan for (default "push")
	JobID           string                     // which job to debug; required only if the event plans more than one
	Matrix          map[string]map[string]bool // matrix combination to pin (act's Config.Matrix shape: key→value→true); required only if the job's matrix has more than one combination
	WithDeps        bool                       // run the job's upstream needs for real (to completion) before debugging it, instead of isolating
	Image           string                     // docker image mapped to ubuntu-latest when Images is empty (default catthehacker); back-compat sugar for Images
	Images          map[string]string          // runner label → docker image (act's -P/Platforms map); empty falls back to {ubuntu-latest: Image}
	Workdir         string                     // workspace bind-mounted into the container so local 'uses: ./' actions resolve; empty = an isolated empty temp dir (steps can't write to your tree). NOTE: a set workdir is mounted, so steps can write to it
	Source          string                     // working tree a default actions/checkout copies into the workspace (no host mutation); empty = current dir. Ignored when Workdir is set
	Secrets         map[string]string          // secrets.* base (flat defaults, e.g. from secret-file); the env overlay and SecretOverrides layer on top
	Vars            map[string]string          // vars.* base (flat defaults); the env overlay and VarOverrides layer on top
	Env             map[string]string          // extra env for containers
	Environments    map[string]EnvOverlay      // per-`environment:` secrets/vars overlays, keyed by environment name; the one matching the debugged job's `environment:` is merged over Secrets/Vars
	SecretOverrides map[string]string          // secrets.* applied last (after the env overlay) so an explicit CLI -secret wins; nil for library callers
	VarOverrides    map[string]string          // vars.* applied last (after the env overlay) so an explicit CLI -var wins; nil for library callers
	GCP             *GCPIdentity               // ambient GCP creds to substitute for a federated google-github-actions/auth (nil = leave auth steps untouched)
	AWS             *AWSIdentity               // ambient AWS creds to substitute for a federated aws-actions/configure-aws-credentials (nil = leave auth steps untouched)
	Needs           map[string]NeedsInput      // seeded needs.<job>.* for isolated debugging, keyed by upstream job id (ignored with WithDeps)
	BreakOnError    bool                       // in Continue mode, halt after a step that errored
	Breakpoints     []int                      // zero-based step indices to halt before, in Continue mode
	BreakpointNames []string                   // step names to halt before, resolved to indices against the job's steps in New (a name with no matching step is an error)

	// Runtime context GitHub injects in real CI that a clean local runner lacks
	// — all seed-and-be-honest surfaces (CLAUDE.md §4), each with a transparency line.
	GitHubToken string            // GITHUB_TOKEN → github.token (and mirrored into secrets.GITHUB_TOKEN); empty falls back to Secrets["GITHUB_TOKEN"]
	Inputs      map[string]string // workflow_dispatch/workflow_call inputs.* (user values; act applies declared defaults + typing on top)
	EventPath   string            // path to a github.event payload JSON; empty = "{}" (plus any Inputs)
	Repository  string            // override github.repository (env GITHUB_REPOSITORY); empty = act derives from local git
	Ref         string            // override github.ref (env GITHUB_REF); empty = act derives from local git
	Sha         string            // override github.sha (env SHA_REF, act's read key); empty = act derives from local git
	Actor       string            // override github.actor (Config.Actor); empty = act's "nektos/act" placeholder
	// GitHubOverrides names the github.* fields the user set explicitly by flag (any of
	// "repository"/"ref"/"sha"/"actor"), so the transparency line marks those as overrides.
	// The values above may also be filled from local git for an honest display — only an
	// entry here means the user overrode it, not merely that the value is non-empty.
	GitHubOverrides []string
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

// ProgressEvent is emitted as the run passes a step's "before" boundary without
// halting (Continue mode, no breakpoint), so a frontend can follow execution — the
// step-list highlight tracks the running step even when no pause fires. It's purely
// advisory: the send is non-blocking and may be dropped under load, and the
// authoritative state is still PauseEvent/Done. Emitted only for the debugged job.
type ProgressEvent struct {
	Index int         // zero-based step index now starting
	Step  *model.Step // the step at this boundary
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
	jobID           string
	steps           []*model.Step
	needs           []NeedsSummary       // how the selected job's needs were satisfied locally (for transparency)
	withDeps        bool                 // upstream needs run for real before the target job
	isolatedWS      bool                 // workspace is an empty temp dir (no user repo) → local actions won't resolve
	workspace       string               // the bind-mounted workspace path (empty unless -workdir given)
	interceptors    []stepInterceptor    // neutralize-and-substitute hooks (checkout, cloud-auth), driven by the barrier
	checkoutLabels  []string             // intercepted default-checkout labels, for transparency
	checkoutSource  string               // host tree copied into the workspace at checkout (empty unless copy mode)
	configSummary   ConfigSummary        // redacted names of supplied secrets/vars/env (for transparency)
	envSummary      EnvSummary           // per-`environment:` overlay applied for the job (for transparency)
	servicesSummary ServicesSummary      // names of the job's `services:` containers (for transparency)
	gcpSummary      GCPSummary           // redacted view of the GCP identity substitution (for transparency)
	awsSummary      AWSSummary           // redacted view of the AWS identity substitution (for transparency)
	tokenSummary    TokenSummary         // how github.token was satisfied (for transparency)
	inputsSummary   InputsSummary        // declared/supplied workflow inputs (for transparency)
	eventSummary    EventSummary         // the github.event payload backing the run (for transparency)
	ghcSummary      GitHubContextSummary // resolved github.* runtime context (for transparency)
	runner          runner.Runner
	plan            *model.Plan
	tmpDir          string // non-empty if we created (and must clean up) the workdir
	eventFile       string // non-empty if we wrote (and must clean up) a merged event.json

	pauses   chan PauseEvent
	progress chan ProgressEvent // advisory step-progress for Continue mode (buffered, lossy)
	resume   chan control
	logs     chan string
	factory  *logFactory
	done     chan struct{}

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
	if opts.Image == "" {
		opts.Image = "catthehacker/ubuntu:act-latest"
	}

	plan, err := parsePlan(&opts)
	if err != nil {
		return nil, err
	}
	run, err := selectRun(plan, opts.JobID)
	if err != nil {
		return nil, err
	}

	// A matrix job expands into one run per combination at act's run time, but a
	// single-job debugger debugs one combination — so pin Config.Matrix to exactly
	// one. This is a selection problem like -job: if the supplied -matrix doesn't
	// narrow to one combo, return a MultipleMatrixError listing the candidates. Joins
	// the pure-invocation errors above (no daemon needed to pick a combo).
	matrix, err := selectMatrix(run, opts.Matrix)
	if err != nil {
		return nil, err
	}

	// Past the pure-invocation errors (parse, MultipleJobsError, unknown job),
	// we're about to need a container — fail fast with a friendly message if the
	// daemon is down rather than letting act stumble on it deep in the run.
	if err := dockerPreflight(); err != nil {
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

	// Resolve config breakpoint names (e.g. `breakpoints: ["Run tests"]`) to step
	// indices now, against the original steps and before any interceptor rewrites a
	// label — an unknown name is a usage error listing the available step names. Done
	// before the workspace temp dir exists, so there's nothing to clean up on error.
	breakIdx, err := resolveBreakpoints(steps, opts.Breakpoints, opts.BreakpointNames)
	if err != nil {
		return nil, err
	}

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

	// Per-`environment:` secrets/vars overlay (CLAUDE.md §4 multi-env). The job may
	// target a deployment environment (e.g. `environment: production`) whose secrets/
	// vars differ from the flat defaults; GHA scopes them by environment and act knows
	// nothing of that, so we resolve the overlay ourselves. Layer flat defaults ← the
	// matching overlay ← CLI overrides, so an explicit -secret/-var still wins. act's
	// model drops the `environment:` key, so read it straight from the raw YAML.
	envName, _ := workflow.JobEnvironment(opts.WorkflowPath, run.JobID)
	overlay := opts.Environments[envName]
	secrets := layer(opts.Secrets, overlay.Secrets, opts.SecretOverrides)
	vars := layer(opts.Vars, overlay.Vars, opts.VarOverrides)
	envSummary := EnvSummary{Name: envName}
	if envName != "" {
		envSummary.Secrets, envSummary.Vars = len(overlay.Secrets), len(overlay.Vars)
	}

	// Redacted transparency summary of the effective secrets/vars/env — taken before
	// resolveToken mirrors github.token into secrets, so the token isn't double-counted
	// here (it has its own TokenSummary).
	configSummary := summarizeConfig(secrets, vars, opts.Env)

	// Runtime context GitHub injects that a clean local runner lacks (CLAUDE.md §4):
	// GITHUB_TOKEN, workflow inputs, the event payload, and the github.* context.
	// Each is seeded here and reported via a transparency line; none needs a fork patch.
	token, tokenSummary := resolveToken(opts.GitHubToken, secrets) // also mirrors token into secrets.GITHUB_TOKEN
	eventPath, eventFile, eventSummary, err := buildEvent(opts)
	if err != nil {
		return nil, err
	}
	// buildEvent may have written a temp event.json; the Session owns it and
	// removes it on cleanup, but until it's constructed an early error return
	// below (workdir/source Abs, MkdirTemp) would leak the file. Drop it on any
	// failure path; cleared once the Session takes ownership.
	ok := false
	defer func() {
		if !ok && eventFile != "" {
			_ = os.Remove(eventFile)
		}
	}()
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
				// Mirror actions/checkout's `submodules:` input: it defaults to
				// false (submodules are not fetched), and only `true`/`recursive`
				// pull them in. So the local workspace copy skips submodule paths
				// unless the step asked for them.
				return info.CopyWorkdir(ctx, checkoutWantsSubmodules(info.Step))
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

	s := &Session{
		jobID:           run.JobID,
		steps:           steps,
		needs:           needs,
		withDeps:        opts.WithDeps,
		isolatedWS:      tmpDir != "", // we created an empty temp workspace (no user repo)
		workspace:       bindWS,
		interceptors:    interceptors,
		checkoutLabels:  stepLabelsOf(steps, checkouts),
		checkoutSource:  checkoutSource,
		configSummary:   configSummary,
		envSummary:      envSummary,
		servicesSummary: buildServices(run.Job()),
		gcpSummary:      gcpSummary,
		awsSummary:      awsSummary,
		tokenSummary:    tokenSummary,
		inputsSummary:   inputsSummary,
		eventSummary:    eventSummary,
		ghcSummary:      ghcSummary,
		plan:            execPlan,
		tmpDir:          tmpDir,
		eventFile:       eventFile,
		pauses:          make(chan PauseEvent),
		progress:        make(chan ProgressEvent, 256), // lossy: a slow frontend drops, never blocks act
		resume:          make(chan control),
		logs:            make(chan string, 1024),
		done:            make(chan struct{}),
		curMode:         modeStep, // stop at entry (before the first step)
		breakpoints:     breakIdx,
		breakOnErr:      opts.BreakOnError,
	}
	s.factory = &logFactory{w: &lineWriter{sink: s.logs, stop: s.done, drop: isGitContextNoise}}
	ok = true // Session now owns eventFile/tmpDir; cleanup() handles them past here

	cfg := &runner.Config{
		Workdir:    workdir,
		EventName:  opts.EventName,
		EventPath:  eventPath,            // user-supplied or merged-with-inputs event.json ("" = act synthesizes)
		Inputs:     orEmpty(opts.Inputs), // ignored by act when EventPath is set; harmless otherwise
		Token:      token,                // github.token (mirrored into secrets.GITHUB_TOKEN above)
		Actor:      opts.Actor,           // github.actor ("" → act's "nektos/act" placeholder)
		Platforms:  buildPlatforms(opts),
		AutoRemove: true,
		LogOutput:  true,                                // route step stdout through the logger (captured below)
		Env:        mergeEnv(orEmpty(opts.Env), ghcEnv), // github.* overrides (GITHUB_REPOSITORY/GITHUB_REF/SHA_REF) layered onto -env
		Secrets:    secrets,
		Vars:       vars,
		Matrix:     matrix, // pinned to one combination by selectMatrix (nil for a matrix-less job)
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

// EnvSummary returns the per-`environment:` overlay applied for the debugged job (the
// environment it targets and how many secrets/vars its overlay contributed), for a
// transparency line. Its Name is empty when the job targets no environment.
func (s *Session) EnvSummary() EnvSummary { return s.envSummary }

// ServicesSummary returns the names of the job's `services:` containers (which act
// starts natively when the job runs), for a transparency line. Empty when none.
func (s *Session) ServicesSummary() ServicesSummary { return s.servicesSummary }

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

// Progress delivers an advisory ProgressEvent as each step starts while the run is
// passing through (Continue mode), letting a frontend track the running step without
// a halt. The channel is buffered and lossy — the run never blocks on it — so it's a
// hint, not a guarantee; Pauses/Done remain authoritative. Optional to consume.
func (s *Session) Progress() <-chan ProgressEvent { return s.progress }

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
		// Not halting — advertise progress so a frontend can follow the run in
		// Continue mode (the step highlight tracks execution even with no pause).
		s.emitProgress(info)
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

// emitProgress publishes an advisory ProgressEvent as a step starts, so a frontend
// can follow Continue-mode execution. Only the debugged job's "before" boundaries
// count (the After boundary doesn't move the highlight; --with-deps upstream jobs are
// not the focus). The send is non-blocking — act must never wait on a frontend — so a
// full buffer just drops the hint; Pauses/Done keep the display correct regardless.
func (s *Session) emitProgress(info runner.StepBarrierInfo) {
	if info.When != runner.BarrierBefore {
		return
	}
	if info.JobID != "" && info.JobID != s.jobID {
		return
	}
	select {
	case s.progress <- ProgressEvent{Index: info.Index, Step: info.Step}:
	default:
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

// parsePlan parses the workflow and plans the chosen event — the Docker-free prefix
// shared by New (which then preflights Docker and builds a Session) and List (which
// stops here). It defaults EventName and validates WorkflowPath, mutating opts so both
// callers see the same normalized values.
func parsePlan(opts *Options) (*model.Plan, error) {
	if opts.WorkflowPath == "" {
		return nil, errors.New("debugger: WorkflowPath is required")
	}
	if opts.EventName == "" {
		opts.EventName = "push"
	}
	planner, err := model.NewWorkflowPlanner(opts.WorkflowPath, true, false)
	if err != nil {
		return nil, fmt.Errorf("debugger: planner: %w", err)
	}
	plan, err := planner.PlanEvent(opts.EventName)
	if err != nil {
		return nil, fmt.Errorf("debugger: plan event %q: %w", opts.EventName, err)
	}
	return plan, nil
}

// buildPlatforms assembles act's runner-label → image map (its -P/Platforms). The
// common ubuntu labels default to opts.Image (so an unmapped `runs-on: ubuntu-22.04`
// still resolves to a sane image rather than act's bare node:16), and opts.Images
// overrides per label (keys lowercased, as act compares them). With no Images set this
// reduces to the historical {ubuntu-latest: Image} behavior plus the extra defaults.
func buildPlatforms(opts Options) map[string]string {
	out := map[string]string{}
	for _, label := range []string{"ubuntu-latest", "ubuntu-24.04", "ubuntu-22.04", "ubuntu-20.04"} {
		out[label] = opts.Image
	}
	for label, image := range opts.Images {
		out[strings.ToLower(label)] = image
	}
	return out
}

// layer merges maps left-to-right (later wins) into a fresh, non-nil map — used to
// stack secrets/vars as flat defaults ← env overlay ← CLI overrides.
func layer(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// resolveBreakpoints builds the halt-before index set from explicit indices plus step
// names resolved against the job's steps. A name matches a step's `name:` or its display
// label (Step.String(), covering unnamed `uses:`/`run:` steps); an unknown name is an
// error listing the available step labels so the user can correct the config.
func resolveBreakpoints(steps []*model.Step, indices []int, names []string) (map[int]bool, error) {
	out := make(map[int]bool, len(indices)+len(names))
	for _, i := range indices {
		out[i] = true
	}
	for _, name := range names {
		idx := -1
		for i, st := range steps {
			if st.Name == name || st.String() == name {
				idx = i
				break
			}
		}
		if idx < 0 {
			labels := make([]string, len(steps))
			for i, st := range steps {
				labels[i] = st.String()
			}
			return nil, fmt.Errorf("debugger: breakpoint step %q not found; steps are: %s", name, strings.Join(labels, " | "))
		}
		out[idx] = true
	}
	return out, nil
}

func toWhen(w runner.BarrierWhen) When {
	if w == runner.BarrierAfter {
		return After
	}
	return Before
}
