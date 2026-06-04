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
	"sync"

	"github.com/nektos/act/pkg/common"
	"github.com/nektos/act/pkg/model"
	"github.com/nektos/act/pkg/runner"
)

// ErrAborted is the run error when the front-end aborts via Abort.
var ErrAborted = errors.New("debugger: run aborted by user")

// Options configures a debug Session. Only WorkflowPath is required.
type Options struct {
	WorkflowPath string            // path to the workflow file
	EventName    string            // event to plan for (default "push")
	Image        string            // docker image mapped to ubuntu-latest (default catthehacker)
	Workdir      string            // job workdir; a temp dir is created and cleaned up if empty
	Secrets      map[string]string // secrets exposed to the workflow
	Env          map[string]string // extra env for containers
	BreakOnError bool              // in Continue mode, halt after a step that errored
	Breakpoints  []int             // zero-based step indices to halt before, in Continue mode
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
	jobID   string
	steps   []*model.Step
	runner  runner.Runner
	plan    *model.Plan
	tmpDir  string // non-empty if we created (and must clean up) the workdir

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

	err error // run result, valid once Done is closed
}

// New parses the workflow, builds a single-job plan, and wires the pause barrier.
// It does not start execution; call Start. v0.1 supports exactly one job.
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
	run, err := singleRun(plan)
	if err != nil {
		return nil, err
	}

	workdir := opts.Workdir
	var tmpDir string
	if workdir == "" {
		// act copies the workdir into the container; an empty temp dir keeps the
		// repo (incl. the act submodule) out of the container.
		workdir, err = os.MkdirTemp("", "actl-")
		if err != nil {
			return nil, fmt.Errorf("debugger: temp workdir: %w", err)
		}
		tmpDir = workdir
	}

	breakpoints := make(map[int]bool, len(opts.Breakpoints))
	for _, i := range opts.Breakpoints {
		breakpoints[i] = true
	}

	s := &Session{
		jobID:       run.JobID,
		steps:       run.Job().Steps,
		plan:        plan,
		tmpDir:      tmpDir,
		pauses:      make(chan PauseEvent),
		resume:      make(chan control),
		logs:        make(chan string, 1024),
		done:        make(chan struct{}),
		curMode:     modeStep, // stop at entry (before the first step)
		breakpoints: breakpoints,
		breakOnErr:  opts.BreakOnError,
	}
	s.factory = &logFactory{w: &lineWriter{sink: s.logs, stop: s.done, drop: isGitContextNoise}}

	cfg := &runner.Config{
		Workdir:     workdir,
		BindWorkdir: false,
		EventName:   opts.EventName,
		Platforms:   map[string]string{"ubuntu-latest": opts.Image},
		AutoRemove:  true,
		LogOutput:   true, // route step stdout through the logger (captured below)
		Env:         orEmpty(opts.Env),
		Secrets:     orEmpty(opts.Secrets),
		Vars:        map[string]string{},
		StepBarrier: s.barrier,
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
	if !s.shouldHalt(info) {
		return nil
	}

	// Publish the live inspection state for the duration of the pause. act is
	// blocked here, so the env map is stable to read.
	s.mu.Lock()
	s.curEnv = info.Env
	s.curContainer = info.ContainerName
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

// shouldHalt is the halt/pass policy. Step mode halts everywhere; Continue mode
// halts only at a breakpoint (before a step) or, with break-on-error, after a
// step that errored.
func (s *Session) shouldHalt(info runner.StepBarrierInfo) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
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

func (s *Session) cleanup() {
	if s.tmpDir != "" {
		_ = os.RemoveAll(s.tmpDir)
	}
}

func singleRun(plan *model.Plan) (*model.Run, error) {
	var runs []*model.Run
	for _, stage := range plan.Stages {
		runs = append(runs, stage.Runs...)
	}
	switch len(runs) {
	case 0:
		return nil, errors.New("debugger: no jobs to run for this event")
	case 1:
		return runs[0], nil
	default:
		return nil, fmt.Errorf("debugger: v0.1 supports a single job, but the plan has %d", len(runs))
	}
}

func toWhen(w runner.BarrierWhen) When {
	if w == runner.BarrierAfter {
		return After
	}
	return Before
}

func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
