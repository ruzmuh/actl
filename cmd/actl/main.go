// Command actl is the TUI step-debugger for GitHub Actions workflows. It runs a
// workflow locally through act's engine, pausing before/after each step so you
// can drive it interactively.
//
//	actl [flags] [workflow.yml]
//
// Requires Docker: act starts a real job container and execs each step into it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ruzmuh/actl/internal/debugger"
	"github.com/ruzmuh/actl/internal/tui"
)

// stringSlice collects a repeatable string flag (e.g. -need a -need b).
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	event := flag.String("event", "push", "event name to plan for")
	job := flag.String("job", "", "job to debug (required only if the event plans more than one)")
	withDeps := flag.Bool("with-deps", false, "run the job's upstream needs for real (to completion) before debugging it, instead of isolating it")
	image := flag.String("image", "catthehacker/ubuntu:act-latest", "docker image mapped to ubuntu-latest (first run pulls it)")
	breakOnError := flag.Bool("break-on-error", true, "halt after a step that fails")
	var needs, envs stringSlice
	flag.Var(&needs, "need", "seed an upstream needs value: 'JOB.outputs.NAME=VALUE' or 'JOB.result=VALUE' (repeatable)")
	flag.Var(&envs, "env", "set an env var for the run: 'KEY=VALUE' (repeatable)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: actl [flags] [workflow.yml]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	path := "testdata/workflows/sample.yml"
	if flag.NArg() > 0 {
		path = flag.Arg(0)
	}

	needsMap, err := parseNeeds(needs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "actl:", err)
		os.Exit(2)
	}

	opts := debugger.Options{
		WorkflowPath: path,
		EventName:    *event,
		JobID:        *job,
		WithDeps:     *withDeps,
		Image:        *image,
		BreakOnError: *breakOnError,
		Needs:        needsMap,
		Env:          parseKeyVals(envs),
	}

	if err := run(opts); err != nil {
		fmt.Fprintln(os.Stderr, "actl:", err)
		os.Exit(1)
	}
}

// parseNeeds turns -need flags into the core's needs map. Each entry is
// 'JOB.outputs.NAME=VALUE' or 'JOB.result=VALUE', mirroring the needs.* path.
func parseNeeds(entries []string) (map[string]debugger.NeedsInput, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := map[string]debugger.NeedsInput{}
	for _, e := range entries {
		path, value, ok := strings.Cut(e, "=")
		if !ok {
			return nil, fmt.Errorf("bad -need %q: want JOB.outputs.NAME=VALUE or JOB.result=VALUE", e)
		}
		parts := strings.SplitN(path, ".", 3)
		job := parts[0]
		in := out[job]
		switch {
		case len(parts) == 2 && parts[1] == "result":
			in.Result = value
		case len(parts) == 3 && parts[1] == "outputs":
			if in.Outputs == nil {
				in.Outputs = map[string]string{}
			}
			in.Outputs[parts[2]] = value
		default:
			return nil, fmt.Errorf("bad -need %q: want JOB.outputs.NAME=VALUE or JOB.result=VALUE", e)
		}
		out[job] = in
	}
	return out, nil
}

// parseKeyVals turns 'KEY=VALUE' entries into a map (later wins on duplicates).
func parseKeyVals(entries []string) map[string]string {
	if len(entries) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, e := range entries {
		if k, v, ok := strings.Cut(e, "="); ok {
			out[k] = v
		}
	}
	return out
}

func run(opts debugger.Options) error {
	sess, err := debugger.New(opts)
	if err != nil {
		var multi *debugger.MultipleJobsError
		if errors.As(err, &multi) {
			fmt.Fprintf(os.Stderr, "actl: %v\n", multi)
			os.Exit(2)
		}
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess.Start(ctx)

	p := tea.NewProgram(tui.New(sess, cancel), tea.WithAltScreen())
	_, err = p.Run()
	return err
}
