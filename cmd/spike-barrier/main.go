// Command spike-barrier is a throwaway, line-based driver over the debugger core
// (internal/debugger). It is the crudest possible front-end — a stand-in for the
// real TUI — used to validate that the core's pause/step/continue loop works end
// to end against act's real engine.
//
// At each pause it prints the boundary and reads one key from stdin:
//
//	(enter)/s = step to the next boundary
//	c         = continue to the next breakpoint / break-on-error / end
//	q         = abort the run
//
// Requires Docker: act starts a real job container and execs each step into it.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/ruzmuh/actl/internal/debugger"
)

// stringSlice collects a repeatable flag (e.g. -input a=1 -input b=2).
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	workflowPath := flag.String("workflow", "testdata/workflows/sample.yml", "path to the workflow file")
	event := flag.String("event", "push", "event name to plan for")
	image := flag.String("image", "catthehacker/ubuntu:act-latest", "docker image for ubuntu-latest (first run pulls it)")
	breaks := flag.String("break", "", "comma-separated zero-based step indices to break before (Continue mode)")
	githubToken := flag.String("github-token", "", "value for github.token / secrets.GITHUB_TOKEN")
	var inputs stringSlice
	flag.Var(&inputs, "input", "workflow_dispatch/workflow_call input NAME=VALUE (repeatable)")
	flag.Parse()

	if err := run(runConfig{
		workflowPath: *workflowPath,
		event:        *event,
		image:        *image,
		breaks:       parseBreaks(*breaks),
		token:        *githubToken,
		inputs:       parseKeyVals(inputs),
	}); err != nil {
		fmt.Fprintln(os.Stderr, "spike-barrier:", err)
		os.Exit(1)
	}
}

// parseKeyVals turns NAME=VALUE entries into a map (nil when empty).
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

type runConfig struct {
	workflowPath, event, image, token string
	breaks                            []int
	inputs                            map[string]string
}

func parseBreaks(s string) []int {
	var out []int
	for _, f := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(f)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func run(cfg runConfig) error {
	sess, err := debugger.New(debugger.Options{
		WorkflowPath: cfg.workflowPath,
		EventName:    cfg.event,
		Image:        cfg.image,
		BreakOnError: true,
		Breakpoints:  cfg.breaks,
		GitHubToken:  cfg.token,
		Inputs:       cfg.inputs,
	})
	if err != nil {
		return err
	}

	fmt.Printf("debugging job %q (%s, event %q)\n", sess.JobID(), cfg.workflowPath, cfg.event)
	for i, st := range sess.Steps() {
		fmt.Printf("  %d. %s\n", i+1, st.String())
	}
	fmt.Println("controls: [enter]/s step · c continue · q quit")

	stdin := bufio.NewScanner(os.Stdin)
	sess.Start(context.Background())

	// Drain captured act logs (proves they no longer hit stderr).
	go func() {
		for line := range sess.Logs() {
			fmt.Printf("│ %s\n", line)
		}
	}()

	for {
		select {
		case ev := <-sess.Pauses():
			fmt.Printf("\n⏸️  %-6s step %d: %q%s\n", ev.When, ev.Index+1, ev.Step.String(), outcome(ev))
			fmt.Printf("   container=%s  env=%d vars  (e.g. CI=%q)\n",
				sess.ContainerName(), len(sess.Env()), sess.Env()["CI"])
		resumeLoop:
			for {
				fmt.Print("   > (enter=step c=continue x=edit+rerun q=quit) ")
				cmd := ""
				if stdin.Scan() {
					cmd = strings.TrimSpace(stdin.Text())
				}
				switch cmd {
				case "c":
					fmt.Println("▶️  continue")
					sess.Continue()
					break resumeLoop
				case "q":
					fmt.Println("⏹️  abort")
					sess.Abort()
					break resumeLoop
				case "x":
					fmt.Println("✏️  edit run -> 'echo EDITED-RERUN-OK', rerun")
					sess.SetRun("echo EDITED-RERUN-OK")
					if err := sess.Rerun(); err != nil {
						fmt.Println("   rerun error:", err)
					}
					// stay paused; prompt again
				default:
					fmt.Println("▶️  step")
					sess.Step()
					break resumeLoop
				}
			}
		case <-sess.Done():
			if err := sess.Err(); err != nil {
				return err
			}
			fmt.Println("\n✅ run complete")
			return nil
		}
	}
}

func outcome(ev debugger.PauseEvent) string {
	if ev.When != debugger.After {
		return ""
	}
	if ev.Err != nil {
		return "  → ❌ " + ev.Err.Error()
	}
	return "  → ✅ ok"
}
