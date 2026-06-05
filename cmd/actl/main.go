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
	"github.com/joho/godotenv"

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
	workdir := flag.String("workdir", "", "bind-mount this dir as the workspace so local 'uses: ./' actions resolve (e.g. '.'); empty = isolated empty workspace. NOTE: a mounted workspace is writable — steps can change your tree")
	source := flag.String("source", "", "working tree a default actions/checkout copies into the workspace (no host mutation); default current dir")
	image := flag.String("image", "catthehacker/ubuntu:act-latest", "docker image mapped to ubuntu-latest (first run pulls it)")
	breakOnError := flag.Bool("break-on-error", true, "halt after a step that fails")
	secretFile := flag.String("secret-file", ".secrets", "dotenv file of secrets.* (skipped if absent; keep it out of git)")
	varFile := flag.String("var-file", ".vars", "dotenv file of vars.* (skipped if absent)")
	envFile := flag.String("env-file", ".env", "dotenv file of env vars (skipped if absent)")
	var needs, envs, secrets, vars stringSlice
	flag.Var(&needs, "need", "seed an upstream needs value: 'JOB.outputs.NAME=VALUE' or 'JOB.result=VALUE' (repeatable)")
	flag.Var(&envs, "env", "set an env var for the run: 'KEY=VALUE' (repeatable, overrides -env-file)")
	flag.Var(&secrets, "secret", "set a secret for the run: 'KEY=VALUE' (repeatable, overrides -secret-file)")
	flag.Var(&vars, "var", "set a var for the run: 'KEY=VALUE' (repeatable, overrides -var-file)")
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

	// A missing dotenv file is fine when the path is the default; if the user
	// pointed a flag at a file, a missing/unreadable file is an error (likely a typo).
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	secretMap, serr := loadConfig(*secretFile, set["secret-file"], secrets)
	varMap, verr := loadConfig(*varFile, set["var-file"], vars)
	envMap, eerr := loadConfig(*envFile, set["env-file"], envs)
	for _, e := range []error{serr, verr, eerr} {
		if e != nil {
			fmt.Fprintln(os.Stderr, "actl:", e)
			os.Exit(2)
		}
	}

	opts := debugger.Options{
		WorkflowPath: path,
		EventName:    *event,
		JobID:        *job,
		WithDeps:     *withDeps,
		Workdir:      *workdir,
		Source:       *source,
		Image:        *image,
		BreakOnError: *breakOnError,
		Needs:        needsMap,
		Secrets:      secretMap,
		Vars:         varMap,
		Env:          envMap,
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

// loadConfig reads a dotenv file into a map, then applies KEY=VALUE flag
// overrides (which win). A missing file is skipped unless explicit, in which case
// it (and any parse error) is reported. Returns nil when nothing loaded.
func loadConfig(file string, explicit bool, overrides []string) (map[string]string, error) {
	out := map[string]string{}
	data, err := godotenv.Read(file)
	switch {
	case err == nil:
		for k, v := range data {
			out[k] = v
		}
	case explicit || !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("read %s: %w", file, err)
	}
	for k, v := range parseKeyVals(overrides) {
		out[k] = v
	}
	if len(out) == 0 {
		return nil, nil
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
