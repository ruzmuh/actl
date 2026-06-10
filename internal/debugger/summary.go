// Package debugger: see session.go for the package doc.
package debugger

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/nektos/act/pkg/model"
)

// ConfigSummary is a redacted view of the secrets/vars/env supplied to the run:
// the names that loaded, never their values, so a transparency line (and any
// screenshot of it) leaks nothing sensitive.
type ConfigSummary struct {
	Secrets []string // secret names (sorted), values withheld
	Vars    []string // var names (sorted)
	Env     []string // env names (sorted)
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

// ServicesSummary lists the names of the job's `services:` containers, for a
// transparency line. act starts these natively when the job runs; actl only surfaces
// that they will start. Empty when the job declares no services.
type ServicesSummary struct {
	Names []string // service container names (sorted)
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

// EnvSummary describes the per-`environment:` overlay applied for the debugged job, for
// a transparency line: the environment the job targets and how many secrets/vars its
// overlay contributed. Name is empty when the job targets no environment; Name set with
// zero counts means the job targets an environment for which no overlay was configured
// (the flat defaults are used as-is). Values are never retained here.
type EnvSummary struct {
	Name    string // the job's `environment:` (deployment environment), "" if none
	Secrets int    // overlay secret keys merged in
	Vars    int    // overlay var keys merged in
}

// buildServices lists the names of the job's `services:` containers for a transparency
// line. act starts them natively when the job runs; we only surface that they will.
func buildServices(job *model.Job) ServicesSummary {
	if len(job.Services) == 0 {
		return ServicesSummary{}
	}
	names := make([]string, 0, len(job.Services))
	for name := range job.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return ServicesSummary{Names: names}
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

// buildGitHubContext maps the github.* values to the env keys act reads when
// synthesizing the github context (github_context.go): GITHUB_REPOSITORY and
// GITHUB_REF survive because act only derives them when unset, and the sha is read
// from SHA_REF. Actor is handled via Config.Actor, not here. Returns the env to
// layer onto -env plus a summary for the transparency line.
//
// Two independent things, deliberately kept apart: env injection happens for any
// non-empty value (so a repo/ref/sha derived from local git still populates the
// context), while the "(override)" mark comes only from opts.GitHubOverrides — the
// fields the user actually set by flag. Conflating them (marking every non-empty
// value an override) is the bug this guards against: after the CLI fills these from
// git, almost everything is non-empty, so the transparency line would cry override
// when the user overrode nothing.
func buildGitHubContext(opts Options) (map[string]string, GitHubContextSummary) {
	env := map[string]string{}
	if opts.Repository != "" {
		env["GITHUB_REPOSITORY"] = opts.Repository
	}
	if opts.Ref != "" {
		env["GITHUB_REF"] = opts.Ref
	}
	if opts.Sha != "" {
		env["SHA_REF"] = opts.Sha // act reads the sha from SHA_REF (github_context.go)
	}
	overridden := append([]string(nil), opts.GitHubOverrides...)
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
