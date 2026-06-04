// Command spike-barrier is a throwaway driver for Spike 2 (see CLAUDE.md): it
// proves the soft-fork pause barrier works end to end. It runs a workflow through
// act's real engine but installs actl's Config.StepBarrier hook, which pauses the
// job pipeline before every step and waits for the user to press Enter to resume.
//
// This is NOT the debugger core or a frontend — it is the crudest possible proof
// that we can stop act between step execs with a live container. The real core
// (internal/debugger) and the TUI come next.
//
// Requires Docker: act starts a real job container and execs each step into it.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/nektos/act/pkg/model"
	"github.com/nektos/act/pkg/runner"
)

func main() {
	workflowPath := flag.String("workflow", "testdata/workflows/sample.yml", "path to the workflow file")
	event := flag.String("event", "push", "event name to plan for")
	image := flag.String("image", "catthehacker/ubuntu:act-latest", "docker image for ubuntu-latest (first run pulls it)")
	flag.Parse()

	if err := run(*workflowPath, *event, *image); err != nil {
		fmt.Fprintln(os.Stderr, "spike-barrier:", err)
		os.Exit(1)
	}
}

func run(workflowPath, event, image string) error {
	// act copies the workdir into the container; use an empty temp dir so we
	// don't haul the whole repo (incl. the act submodule) into the container.
	workdir, err := os.MkdirTemp("", "actl-spike-")
	if err != nil {
		return fmt.Errorf("temp workdir: %w", err)
	}
	defer os.RemoveAll(workdir)

	planner, err := model.NewWorkflowPlanner(workflowPath, true, false)
	if err != nil {
		return fmt.Errorf("planner: %w", err)
	}
	plan, err := planner.PlanEvent(event)
	if err != nil {
		return fmt.Errorf("plan event %q: %w", event, err)
	}
	if plan == nil || len(plan.Stages) == 0 {
		return fmt.Errorf("no jobs to run for event %q", event)
	}

	stdin := bufio.NewScanner(os.Stdin)

	cfg := &runner.Config{
		Workdir:     workdir,
		BindWorkdir: false,
		EventName:   event,
		Platforms:   map[string]string{"ubuntu-latest": image},
		AutoRemove:  true,
		LogOutput:   true, // surface each step's stdout so the spike timeline is legible
		Env:         map[string]string{},
		Secrets:     map[string]string{},
		Vars:        map[string]string{},

		// The whole point of Spike 2: pause before every step.
		StepBarrier: func(_ context.Context, info runner.StepBarrierInfo) error {
			fmt.Printf("\n⏸️  PAUSED before step %d: %q  [%s]\n",
				info.Index+1, info.Step.String(), kind(info.Step))
			fmt.Print("   press Enter to resume the step (or Ctrl-C to quit)... ")
			stdin.Scan()
			fmt.Printf("▶️  resuming step %d\n\n", info.Index+1)
			return nil
		},
	}

	r, err := runner.New(cfg)
	if err != nil {
		return fmt.Errorf("new runner: %w", err)
	}

	fmt.Printf("running %q (event %q) with the pause barrier installed\n", workflowPath, event)
	return r.NewPlanExecutor(plan)(context.Background())
}

func kind(s *model.Step) string {
	if s.Run != "" {
		return "run"
	}
	return "uses: " + s.Uses
}
