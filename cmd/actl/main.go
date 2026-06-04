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
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ruzmuh/actl/internal/debugger"
	"github.com/ruzmuh/actl/internal/tui"
)

func main() {
	event := flag.String("event", "push", "event name to plan for")
	image := flag.String("image", "catthehacker/ubuntu:act-latest", "docker image mapped to ubuntu-latest (first run pulls it)")
	breakOnError := flag.Bool("break-on-error", true, "halt after a step that fails")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: actl [flags] [workflow.yml]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	path := "testdata/workflows/sample.yml"
	if flag.NArg() > 0 {
		path = flag.Arg(0)
	}

	if err := run(path, *event, *image, *breakOnError); err != nil {
		fmt.Fprintln(os.Stderr, "actl:", err)
		os.Exit(1)
	}
}

func run(path, event, image string, breakOnError bool) error {
	sess, err := debugger.New(debugger.Options{
		WorkflowPath: path,
		EventName:    event,
		Image:        image,
		BreakOnError: breakOnError,
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess.Start(ctx)

	p := tea.NewProgram(tui.New(sess, cancel), tea.WithAltScreen())
	_, err = p.Run()
	return err
}
