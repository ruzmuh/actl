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
	"strings"

	"github.com/ruzmuh/actl/internal/debugger"
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
	sess, err := debugger.New(debugger.Options{
		WorkflowPath: workflowPath,
		EventName:    event,
		Image:        image,
		BreakOnError: true,
	})
	if err != nil {
		return err
	}

	fmt.Printf("debugging job %q (%s, event %q)\n", sess.JobID(), workflowPath, event)
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
			fmt.Print("   > ")
			cmd := ""
			if stdin.Scan() {
				cmd = strings.TrimSpace(stdin.Text())
			}
			switch cmd {
			case "c":
				fmt.Println("▶️  continue")
				sess.Continue()
			case "q":
				fmt.Println("⏹️  abort")
				sess.Abort()
			default:
				fmt.Println("▶️  step")
				sess.Step()
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
