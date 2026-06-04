// Command actl is the entry point for the actl debugger.
//
// SPIKE STATUS (Spike 1 of the roadmap in CLAUDE.md): for now this binary only
// proves that act's libraries import and work as plain dependencies — it parses
// a workflow via act/pkg/model and evaluates a ${{ }} expression via
// act/pkg/exprparser. The Bubble Tea TUI and the pause-barrier debugger come in
// later spikes; this main will be replaced once the core lands.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/nektos/act/pkg/exprparser"
	"github.com/nektos/act/pkg/model"

	"github.com/ruzmuh/actl/internal/expr"
	"github.com/ruzmuh/actl/internal/workflow"
)

func main() {
	strict := flag.Bool("strict", false, "validate the workflow against the GitHub Actions schema")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: actl [flags] [workflow.yml]\n\n")
		fmt.Fprintf(os.Stderr, "Spike: parse a workflow and evaluate a sample expression.\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	path := "testdata/workflows/sample.yml"
	if flag.NArg() > 0 {
		path = flag.Arg(0)
	}

	if err := run(path, *strict); err != nil {
		fmt.Fprintln(os.Stderr, "actl:", err)
		os.Exit(1)
	}
}

func run(path string, strict bool) error {
	wf, err := workflow.Load(path, strict)
	if err != nil {
		return err
	}

	// --- 1. workflow parsing (act/pkg/model) ---
	fmt.Printf("workflow %q  (%s)\n", wf.Name, wf.File)
	fmt.Printf("  on: %v\n", wf.On())
	for jobID, job := range wf.Jobs {
		fmt.Printf("  job %q\n", jobID)
		for i, step := range job.Steps {
			fmt.Printf("    %d. [%-7s] %s\n", i+1, workflow.KindOf(step), step.String())
		}
	}

	// --- 2. expression evaluation (act/pkg/exprparser) ---
	env := &exprparser.EvaluationEnvironment{
		Github: &model.GithubContext{EventName: "push"},
		Env:    wf.Env,
	}
	fmt.Println("\nexpression checks:")
	for _, in := range []string{
		"github.event_name",
		"github.event_name == 'push'",
		"format('{0} world', env.GREETING)",
		// Note: the GHA expression language has no arithmetic operators — only
		// comparison, logic, indexing, and a fixed set of functions.
		"contains('step-debugger', 'bug') && startsWith(env.GREETING, 'hel')",
	} {
		v, err := expr.Evaluate(in, env)
		if err != nil {
			return err
		}
		fmt.Printf("  %-40s => %v\n", in, v)
	}
	return nil
}
