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
	"os/exec"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/joho/godotenv"

	"github.com/ruzmuh/actl/internal/config"
	"github.com/ruzmuh/actl/internal/debugger"
	"github.com/ruzmuh/actl/internal/tui"
	"github.com/ruzmuh/actl/internal/workflow"
)

// version is the actl release, stamped at build time via
// -ldflags "-X main.version=…" (GoReleaser's default). "dev" for plain `go build`.
var version = "dev"

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
	gcpKeyFile := flag.String("gcp-key-file", "", "service-account key JSON to substitute for a federated google-github-actions/auth step (the default bring-a-credential path)")
	azureCredsFile := flag.String("azure-creds-file", "", "service-principal creds JSON to substitute for a federated azure/login step (Azure has no ambient fallback)")
	awsKeysFile := flag.String("aws-keys-file", "", "dotenv file with AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY to substitute for a federated aws-actions/configure-aws-credentials step")
	gcpAmbient := flag.Bool("gcp-ambient", false, "opt-in fallback: use ambient gcloud ADC for a federated google-github-actions/auth step (mounts a refresh-token credential into the container)")
	awsAmbient := flag.Bool("aws-ambient", false, "opt-in fallback: use ambient AWS credentials for a federated aws-actions/configure-aws-credentials step")
	gcpIdentity := flag.Bool("gcp-identity", false, "deprecated alias for -gcp-ambient")
	awsIdentity := flag.Bool("aws-identity", false, "deprecated alias for -aws-ambient")
	gcpCreds := flag.String("gcp-credentials", "", "path to the ADC json for -gcp-ambient (default: discover gcloud's application_default_credentials.json)")
	awsProfile := flag.String("aws-profile", "", "AWS profile for -aws-ambient (default: the default profile / environment)")
	githubToken := flag.String("github-token", "", "value for github.token / secrets.GITHUB_TOKEN (default: GITHUB_TOKEN from .secrets, else ambient 'gh auth token')")
	eventFile := flag.String("event-file", "", "path to a github.event payload JSON (sets github.event.*)")
	repository := flag.String("repository", "", "override github.repository (default: derive owner/repo from the local git 'origin' remote)")
	ref := flag.String("ref", "", "override github.ref, e.g. refs/heads/main (default: derive from the local git HEAD)")
	sha := flag.String("sha", "", "override github.sha (default: derive from the local git HEAD)")
	actor := flag.String("actor", "", "override github.actor (default: act's 'nektos/act' placeholder)")
	configPath := flag.String("config", ".actl.yml", "project config file for the debug slice (job/matrix/breakpoints/secrets/vars/env); skipped if absent unless set explicitly")
	list := flag.Bool("list", false, "list the workflow's jobs and steps (with matrix combinations and environment) without running, then exit")
	showVersion := flag.Bool("version", false, "print actl version and exit")
	var needs, envs, secrets, vars, inputs, matrix, platforms stringSlice
	flag.Var(&matrix, "matrix", "pin a matrix combination: 'KEY=VALUE' (repeatable; required only if the job's matrix has more than one combination)")
	flag.Var(&platforms, "platform", "map a runner label to a docker image: 'LABEL=IMAGE' (repeatable, act's -P; overrides .actl.yml images)")
	flag.Var(&needs, "need", "seed an upstream needs value: 'JOB.outputs.NAME=VALUE' or 'JOB.result=VALUE' (repeatable)")
	flag.Var(&envs, "env", "set an env var for the run: 'KEY=VALUE' (repeatable, overrides -env-file)")
	flag.Var(&secrets, "secret", "set a secret for the run: 'KEY=VALUE' (repeatable, overrides -secret-file and any environment overlay)")
	flag.Var(&vars, "var", "set a var for the run: 'KEY=VALUE' (repeatable, overrides -var-file and any environment overlay)")
	flag.Var(&inputs, "input", "set a workflow_dispatch/workflow_call input: 'NAME=VALUE' (repeatable)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: actl [flags] [workflow.yml]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Println("actl", version)
		return
	}

	// Which flags the user set explicitly (vs left at their default) — drives the
	// precedence rule CLI flag > .actl.yml > built-in default, and decides whether a
	// missing dotenv file is a typo (explicit) or just absent (default).
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })

	cfg, err := config.Load(*configPath, set["config"])
	if err != nil {
		fmt.Fprintln(os.Stderr, "actl:", err)
		os.Exit(2)
	}
	if cfg == nil {
		cfg = &config.Config{} // no config file → all-empty, so the lookups below fall through to flags
	}

	path, err := resolveWorkflowPath(flag.Arg(0), cfg.Workflow)
	if err != nil {
		fmt.Fprintln(os.Stderr, "actl:", err)
		os.Exit(2)
	}

	// Scalars: a set flag wins, else the config value, else the flag's default.
	eventName := orConfig(set["event"], *event, cfg.Event)
	jobID := orConfig(set["job"], *job, cfg.Job)
	workdirPath := orConfig(set["workdir"], *workdir, cfg.Workdir)
	sourceDir := orConfig(set["source"], *source, cfg.Source)
	secretFilePath := orConfig(set["secret-file"], *secretFile, cfg.SecretFile)
	withDepsVal := *withDeps
	if !set["with-deps"] && cfg.WithDeps != nil {
		withDepsVal = *cfg.WithDeps
	}

	// -list is a read-only inventory: print jobs/steps without touching Docker or
	// shelling out for identity, then exit. Done here, before any of that.
	if *list {
		listing, err := debugger.List(debugger.Options{WorkflowPath: path, EventName: eventName})
		if err != nil {
			fmt.Fprintln(os.Stderr, "actl:", err)
			os.Exit(1)
		}
		printListing(listing)
		return
	}

	needsMap, err := resolveNeeds(needs, cfg.Needs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "actl:", err)
		os.Exit(2)
	}

	// secrets/vars/env precedence (a missing dotenv file is fine unless its path was
	// given explicitly). Secrets are file-only (.actl.yml can't inline them); vars/env
	// may also come from the config inline maps, under the dotenv file. CLI -secret/-var
	// are kept separate as *Overrides so the core can apply them AFTER the per-environment
	// overlay — an explicit override always wins; -env has no overlay so it folds in here.
	secretMap, serr := loadConfig(nil, secretFilePath, set["secret-file"] || cfg.SecretFile != "", nil)
	varMap, verr := loadConfig(cfg.Vars, *varFile, set["var-file"], nil)
	envMap, eerr := loadConfig(cfg.Env, *envFile, set["env-file"], envs)
	environments, oerr := resolveEnvironments(cfg.Environments)
	for _, e := range []error{serr, verr, eerr, oerr} {
		if e != nil {
			fmt.Fprintln(os.Stderr, "actl:", e)
			os.Exit(2)
		}
	}
	secretOverrides := parseKeyVals(secrets)
	varOverrides := parseKeyVals(vars)

	bpIdx, bpNames := splitBreakpoints(cfg.Breakpoints)

	// Cloud identity (CLAUDE.md §4). Default: bring a credential — a key/creds file
	// rewrites the federated auth step to its secret/key mode so the real action runs.
	// Ambient personal login is an opt-in fallback (-…-ambient; GCP/AWS only — Azure has
	// none). The deprecated -gcp-identity/-aws-identity flags alias the ambient bools.
	// All file reads / CLI shell-outs stay lazy — only when the workflow uses the action.
	gcpKeyFilePath := orConfig(set["gcp-key-file"], *gcpKeyFile, identityFile(cfg, "gcp"))
	awsKeysFilePath := orConfig(set["aws-keys-file"], *awsKeysFile, identityFile(cfg, "aws"))
	azureCredsFilePath := orConfig(set["azure-creds-file"], *azureCredsFile, identityFile(cfg, "azure"))
	useGCPAmbient := boolOrConfig(set["gcp-ambient"], *gcpAmbient, identityAmbient(cfg, "gcp")) || *gcpIdentity
	useAWSAmbient := boolOrConfig(set["aws-ambient"], *awsAmbient, identityAmbient(cfg, "aws")) || *awsIdentity
	if set["gcp-identity"] {
		fmt.Fprintln(os.Stderr, "actl: -gcp-identity is deprecated; use -gcp-ambient")
	}
	if set["aws-identity"] {
		fmt.Fprintln(os.Stderr, "actl: -aws-identity is deprecated; use -aws-ambient")
	}

	var gcpKeyJSON, azureCredsJSON, awsKeyID, awsSecretKey string
	var gcp *debugger.GCPIdentity
	var aws *debugger.AWSIdentity
	if workflowUses(path, debugger.GCPAuthAction) {
		if gcpKeyFilePath != "" {
			if gcpKeyJSON, err = readCredFile(gcpKeyFilePath); err != nil {
				fmt.Fprintln(os.Stderr, "actl:", err)
				os.Exit(2)
			}
		} else if useGCPAmbient {
			gcp = gatherGCPIdentity(path, *gcpCreds)
		}
	}
	if workflowUses(path, debugger.AWSAuthAction) {
		if awsKeysFilePath != "" {
			if awsKeyID, awsSecretKey, err = readAWSKeys(awsKeysFilePath); err != nil {
				fmt.Fprintln(os.Stderr, "actl:", err)
				os.Exit(2)
			}
		} else if useAWSAmbient {
			aws = gatherAWSIdentity(path, *awsProfile)
		}
	}
	if workflowUses(path, debugger.AzureAuthAction) && azureCredsFilePath != "" {
		if azureCredsJSON, err = readCredFile(azureCredsFilePath); err != nil {
			fmt.Fprintln(os.Stderr, "actl:", err)
			os.Exit(2)
		}
	}

	// github.token: explicit flag wins; the core falls back to a GITHUB_TOKEN in the
	// secrets (file or -secret); failing both, try the dev's ambient `gh` login
	// (best-effort, like gcloud for GCP). The transparency line makes it loud.
	token := *githubToken
	if token == "" && secretMap["GITHUB_TOKEN"] == "" && secretOverrides["GITHUB_TOKEN"] == "" {
		token = ghValue("auth", "token")
	}

	// github.* runtime context: each flag wins, otherwise derive from the local git
	// repo (the source tree being debugged) so the transparency line shows real
	// values; empty leaves it for act to derive.
	gitDir := firstNonEmpty(sourceDir, workdirPath, ".")
	repo, gref, gsha := resolveGitHubContext(gitDir, *repository, *ref, *sha)
	// Which github.* fields the user set by flag (vs. derived from git above) — only
	// these are real overrides, so the transparency line marks just them. resolveGitHubContext
	// fills the rest from local git for honest display, which must NOT read as an override.
	ghOverrides := setFlags(set, "repository", "ref", "sha", "actor")

	opts := debugger.Options{
		WorkflowPath:       path,
		EventName:          eventName,
		JobID:              jobID,
		Matrix:             resolveMatrix(cfg.Matrix, matrix),
		WithDeps:           withDepsVal,
		Workdir:            workdirPath,
		Source:             sourceDir,
		Image:              *image,
		Images:             resolveImages(cfg.Images, platforms),
		BreakOnError:       *breakOnError,
		Breakpoints:        bpIdx,
		BreakpointNames:    bpNames,
		Needs:              needsMap,
		Secrets:            secretMap,
		Vars:               varMap,
		Env:                envMap,
		Environments:       environments,
		SecretOverrides:    secretOverrides,
		VarOverrides:       varOverrides,
		GCPKeyJSON:         gcpKeyJSON,
		AzureCredsJSON:     azureCredsJSON,
		AWSAccessKeyID:     awsKeyID,
		AWSSecretAccessKey: awsSecretKey,
		GCP:                gcp,
		AWS:                aws,
		GitHubToken:        token,
		Inputs:             mergeStrMap(cfg.Inputs, parseKeyVals(inputs)),
		EventPath:          *eventFile,
		Repository:         repo,
		Ref:                gref,
		Sha:                gsha,
		Actor:              *actor,
		GitHubOverrides:    ghOverrides,
	}

	if err := run(opts); err != nil {
		fmt.Fprintln(os.Stderr, "actl:", err)
		os.Exit(1)
	}
}

// resolveWorkflowPath decides which workflow to debug. An explicit path argument
// wins. Otherwise it auto-discovers .github/workflows/*.{yml,yaml} in the current
// directory: exactly one is used (with an honest note on stderr); none or several
// is a friendly error telling the user to pass one explicitly — mirroring how
// -job handles a multi-job workflow.
func resolveWorkflowPath(arg, cfgWorkflow string) (string, error) {
	if arg != "" {
		return arg, nil
	}
	if cfgWorkflow != "" {
		return cfgWorkflow, nil
	}
	found, err := workflow.Discover(".")
	if err != nil {
		return "", err
	}
	switch len(found) {
	case 0:
		return "", errors.New("no workflow found in ./.github/workflows — pass one explicitly: actl path/to/workflow.yml")
	case 1:
		fmt.Fprintf(os.Stderr, "actl: debugging %s (the only workflow found)\n", found[0])
		return found[0], nil
	default:
		return "", fmt.Errorf("found %d workflows; pass one explicitly: %s", len(found), strings.Join(found, ", "))
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

// loadConfig layers a flat key/value set: a base map (e.g. .actl.yml inline vars/env),
// then a dotenv file, then KEY=VALUE flag overrides — each winning over the last. A
// missing file is skipped unless explicit, in which case it (and any parse error) is
// reported. Returns nil when nothing loaded.
func loadConfig(base map[string]string, file string, explicit bool, overrides []string) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
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

// orConfig implements the scalar precedence CLI flag > .actl.yml > default: a flag the
// user set explicitly wins; otherwise a non-empty config value; otherwise the flag's
// own (default) value.
func orConfig(set bool, flagVal, cfgVal string) string {
	if !set && cfgVal != "" {
		return cfgVal
	}
	return flagVal
}

// boolOrConfig resolves a bool with the precedence CLI flag > .actl.yml > flag default:
// a flag the user set wins; otherwise a non-nil config value; otherwise the flag's default.
func boolOrConfig(set bool, flagVal bool, cfgVal *bool) bool {
	if !set && cfgVal != nil {
		return *cfgVal
	}
	return flagVal
}

// identityFile / identityAmbient read one cloud's identity config from .actl.yml (the
// brought-credential file path and the opt-in ambient flag). cloud is "gcp"/"aws"/"azure".
func identityFile(cfg *config.Config, cloud string) string { return cloudIdentity(cfg, cloud).File }
func identityAmbient(cfg *config.Config, cloud string) *bool {
	return cloudIdentity(cfg, cloud).Ambient
}

func cloudIdentity(cfg *config.Config, cloud string) config.CloudIdentity {
	switch cloud {
	case "gcp":
		return cfg.Identity.GCP
	case "aws":
		return cfg.Identity.AWS
	case "azure":
		return cfg.Identity.Azure
	}
	return config.CloudIdentity{}
}

// readCredFile reads a brought-credential file's content (SA key / SP creds JSON), trimmed.
// An empty path yields "" (no credential); a set-but-unreadable path is an error.
func readCredFile(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read credential file %s: %w", path, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// readAWSKeys reads a dotenv file of AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY (the brought
// static keys). An empty path yields empty strings; a set-but-unreadable path is an error.
func readAWSKeys(path string) (id, secret string, err error) {
	if path == "" {
		return "", "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("read AWS keys file %s: %w", path, err)
	}
	kv := parseKeyVals(strings.Split(string(b), "\n"))
	return kv["AWS_ACCESS_KEY_ID"], kv["AWS_SECRET_ACCESS_KEY"], nil
}

// mergeStrMap overlays b onto a (b wins) into a fresh map, or nil when both are empty.
func mergeStrMap(a, b map[string]string) map[string]string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// resolveMatrix merges the config matrix (one value per key) under the CLI -matrix flags
// (act's key→value→true shape); a key given on the CLI replaces the config's value for
// that key. Returns nil when neither constrains the matrix.
func resolveMatrix(cfgMatrix map[string]string, cli []string) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for k, v := range cfgMatrix {
		out[k] = map[string]bool{v: true}
	}
	for k, vs := range parseMatrixSel(cli) {
		out[k] = vs // CLI replaces the config's selection for this key
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// resolveImages builds the runner label → image map from .actl.yml images: under the
// CLI -platform flags (flag wins). Keys are lowercased to match how act compares labels.
func resolveImages(cfgImages map[string]string, platforms []string) map[string]string {
	out := map[string]string{}
	for k, v := range cfgImages {
		out[strings.ToLower(k)] = v
	}
	for k, v := range parseKeyVals(platforms) {
		out[strings.ToLower(k)] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// resolveNeeds merges config needs under the CLI -need flags: a job named on the CLI
// replaces its config seed entirely.
func resolveNeeds(cli []string, cfgNeeds map[string]config.Need) (map[string]debugger.NeedsInput, error) {
	cliMap, err := parseNeeds(cli)
	if err != nil {
		return nil, err
	}
	if len(cfgNeeds) == 0 {
		return cliMap, nil
	}
	out := map[string]debugger.NeedsInput{}
	for job, n := range cfgNeeds {
		out[job] = debugger.NeedsInput{Result: n.Result, Outputs: n.Outputs}
	}
	for job, n := range cliMap {
		out[job] = n
	}
	return out, nil
}

// resolveEnvironments turns the config's per-`environment:` overlays into the core's
// EnvOverlay shape: each environment's secret-file is read into a flat secret map (a
// missing/unreadable file is an error — the path was given explicitly), and its inline
// vars pass through. Returns nil when no environments are configured.
func resolveEnvironments(cfgEnvs map[string]config.EnvOverlay) (map[string]debugger.EnvOverlay, error) {
	if len(cfgEnvs) == 0 {
		return nil, nil
	}
	out := make(map[string]debugger.EnvOverlay, len(cfgEnvs))
	for name, ov := range cfgEnvs {
		var secrets map[string]string
		if ov.SecretFile != "" {
			// Non-fatal on a missing file: only the debugged job's environment
			// overlay is actually used, so an absent secret-file for some other
			// environment must not fail the run (you may hold .secrets.prod only on
			// the box that debugs prod). A malformed file still errors. The overlay's
			// secret count in the transparency line shows what actually loaded.
			m, err := loadConfig(nil, ov.SecretFile, false, nil)
			if err != nil {
				return nil, fmt.Errorf("environment %s: %w", name, err)
			}
			secrets = m
		}
		out[name] = debugger.EnvOverlay{Secrets: secrets, Vars: ov.Vars}
	}
	return out, nil
}

// splitBreakpoints separates config breakpoints into explicit step indices and step
// names (the core resolves names against the job's steps).
func splitBreakpoints(bps []config.Breakpoint) (indices []int, names []string) {
	for _, b := range bps {
		if b.Name != "" {
			names = append(names, b.Name)
		} else {
			indices = append(indices, b.Index)
		}
	}
	return indices, names
}

// printListing renders `actl -list`: each job (with its environment and matrix
// combinations) and its steps, to stdout.
func printListing(l *debugger.Listing) {
	fmt.Printf("%s (event: %s)\n", l.WorkflowPath, l.Event)
	for _, job := range l.Jobs {
		label := job.ID
		if job.Name != "" && job.Name != job.ID {
			label += " (" + job.Name + ")"
		}
		fmt.Printf("\njob: %s\n", label)
		if job.Environment != "" {
			fmt.Printf("  environment: %s\n", job.Environment)
		}
		if len(job.Matrix) > 0 {
			fmt.Printf("  matrix: %s\n", strings.Join(job.Matrix, " | "))
		}
		for _, st := range job.Steps {
			fmt.Printf("  %2d. [%s] %s\n", st.Index, st.Kind, st.Label)
		}
	}
}

// gatherGCPIdentity resolves the dev's ambient GCP credentials for substitution,
// but only when the workflow actually has a google-github-actions/auth step — so a
// non-GCP run never shells out to gcloud. Discovery and token minting are
// best-effort: a missing piece just narrows what we can inject (the core still
// neutralizes the auth step and the TUI reports honestly). Returns nil when there's
// no auth step, or neither a credential file nor a token could be found.
func gatherGCPIdentity(path, credOverride string) *debugger.GCPIdentity {
	if !workflowUses(path, debugger.GCPAuthAction) {
		return nil
	}
	file := findADCFile(credOverride)
	token := gcloudValue("auth", "application-default", "print-access-token")
	if file == "" && token == "" {
		return nil
	}
	return &debugger.GCPIdentity{
		CredentialFile: file,
		AccessToken:    token,
		Account:        gcloudValue("config", "get-value", "account"),
	}
}

// workflowUses reports whether any job in the workflow at path uses action (bare
// or @version) — a cheap pre-scan to keep cloud-CLI invocation lazy. A parse error
// is treated as "no" (it resurfaces later in debugger.New). The match logic lives
// in debugger.StepUses so the action literals have a single home.
func workflowUses(path, action string) bool {
	wf, err := workflow.Load(path, true)
	if err != nil {
		return false // a real parse error surfaces later in debugger.New
	}
	for _, job := range wf.Jobs {
		for _, st := range job.Steps {
			if debugger.StepUses(st, action) {
				return true
			}
		}
	}
	return false
}

// gatherAWSIdentity resolves the dev's ambient AWS credentials for substitution, but
// only when the workflow has an aws-actions/configure-aws-credentials step — so a
// non-AWS run never shells out to the aws CLI. Best-effort, like gatherGCPIdentity: it
// runs `aws configure export-credentials` to resolve whatever the ambient session is
// (static keys, SSO, an already-assumed role) into concrete credentials. Returns nil
// when there's no auth step or no resolvable credentials.
func gatherAWSIdentity(path, profile string) *debugger.AWSIdentity {
	if !workflowUses(path, debugger.AWSAuthAction) {
		return nil
	}
	creds := awsExportCredentials(profile)
	if creds["AWS_ACCESS_KEY_ID"] == "" || creds["AWS_SECRET_ACCESS_KEY"] == "" {
		return nil
	}
	return &debugger.AWSIdentity{
		AccessKeyID:     creds["AWS_ACCESS_KEY_ID"],
		SecretAccessKey: creds["AWS_SECRET_ACCESS_KEY"],
		SessionToken:    creds["AWS_SESSION_TOKEN"],
		Account:         awsValue(profile, "sts", "get-caller-identity", "--query", "Arn", "--output", "text"),
	}
}

// awsExportCredentials runs `aws configure export-credentials --format env-no-export`
// and parses its KEY=VALUE lines (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY /
// AWS_SESSION_TOKEN / …). Returns nil on any error (aws absent, no creds, an older CLI
// without export-credentials).
func awsExportCredentials(profile string) map[string]string {
	out := awsValue(profile, "configure", "export-credentials", "--format", "env-no-export")
	if out == "" {
		return nil
	}
	return parseKeyVals(strings.Split(out, "\n"))
}

// awsValue runs `aws <args...>` (under the given profile, if any) and returns the
// trimmed stdout, or "" on any error — best-effort, like gcloudValue.
func awsValue(profile string, args ...string) string {
	cmd := exec.Command("aws", args...)
	if profile != "" {
		cmd.Env = append(os.Environ(), "AWS_PROFILE="+profile)
	}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// findADCFile locates the Application Default Credentials json, preferring an
// explicit override, then $GOOGLE_APPLICATION_CREDENTIALS, then gcloud's well-known
// path under $CLOUDSDK_CONFIG or $HOME. Returns "" if none exists. Pure (no gcloud),
// so it's unit-testable.
func findADCFile(override string) string {
	const adcName = "application_default_credentials.json"
	candidates := []string{override, os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")}
	if cfg := os.Getenv("CLOUDSDK_CONFIG"); cfg != "" {
		candidates = append(candidates, filepath.Join(cfg, adcName))
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "gcloud", adcName))
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	return ""
}

// gcloudValue runs `gcloud <args...>` and returns the trimmed stdout, or "" on any
// error (gcloud absent, not logged in, etc.) — every caller is best-effort.
func gcloudValue(args ...string) string {
	out, err := exec.Command("gcloud", args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ghValue runs `gh <args...>` and returns the trimmed stdout, or "" on any error
// (gh absent, not logged in, etc.) — best-effort, like gcloudValue.
func ghValue(args ...string) string {
	out, err := exec.Command("gh", args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// resolveGitHubContext fills github.repository/ref/sha: a non-empty override wins,
// otherwise it derives the value from the local git repo at dir (best-effort — a
// missing value is left empty for act to derive). Pure-ish (only shells out to git),
// so the values feed both the run and the transparency line.
func resolveGitHubContext(dir, repoOverride, refOverride, shaOverride string) (repo, ref, sha string) {
	repo = repoOverride
	if repo == "" {
		repo = parseGitRepo(gitValue(dir, "remote", "get-url", "origin"))
	}
	ref = refOverride
	if ref == "" {
		ref = gitValue(dir, "symbolic-ref", "--quiet", "HEAD") // "refs/heads/<branch>"; empty when detached
	}
	sha = shaOverride
	if sha == "" {
		sha = gitValue(dir, "rev-parse", "HEAD")
	}
	return repo, ref, sha
}

// gitValue runs `git -C dir <args...>` and returns trimmed stdout, or "" on error.
func gitValue(dir string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// parseGitRepo turns a git remote URL into "owner/repo", handling the ssh
// (git@host:owner/repo.git) and https (https://host/owner/repo.git) forms. Returns
// "" if it can't extract an owner/repo pair.
func parseGitRepo(url string) string {
	if url == "" {
		return ""
	}
	url = strings.TrimSuffix(url, ".git")
	if i := strings.Index(url, "://"); i >= 0 { // strip scheme + host
		rest := url[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			url = rest[j+1:]
		}
	} else if i := strings.LastIndex(url, ":"); i >= 0 { // scp-like: host:owner/repo
		url = url[i+1:]
	}
	if strings.Count(url, "/") != 1 || strings.HasPrefix(url, "/") || strings.HasSuffix(url, "/") {
		return ""
	}
	return url
}

// setFlags returns the subset of names the user set explicitly on the command line
// (per the flag.Visit set map), preserving the given order — used to report which
// github.* fields are real overrides rather than git-derived defaults.
func setFlags(set map[string]bool, names ...string) []string {
	var out []string
	for _, n := range names {
		if set[n] {
			out = append(out, n)
		}
	}
	return out
}

// firstNonEmpty returns the first non-empty argument, or "" if all are empty.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
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

// parseMatrixSel turns repeated 'KEY=VALUE' matrix flags into act's Config.Matrix
// shape (key→value→true). Repeating a key allows several values for it (e.g.
// -matrix os=ubuntu-latest -matrix os=macos-latest). We use '=' (not act's CLI ':')
// to stay consistent with -secret/-var/-env/-input.
func parseMatrixSel(entries []string) map[string]map[string]bool {
	if len(entries) == 0 {
		return nil
	}
	out := map[string]map[string]bool{}
	for _, e := range entries {
		if k, v, ok := strings.Cut(e, "="); ok {
			if out[k] == nil {
				out[k] = map[string]bool{}
			}
			out[k][v] = true
		}
	}
	return out
}

func run(opts debugger.Options) error {
	sess, err := debugger.New(opts)
	if err != nil {
		// Selection ambiguities (which job / which matrix combo) aren't failures —
		// list the choices and exit 2 so the user re-runs with -job / -matrix.
		var multiJob *debugger.MultipleJobsError
		var multiMatrix *debugger.MultipleMatrixError
		if errors.As(err, &multiJob) || errors.As(err, &multiMatrix) {
			fmt.Fprintf(os.Stderr, "actl: %v\n", err)
			os.Exit(2)
		}
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess.Start(ctx)

	p := tea.NewProgram(tui.New(sess, cancel), tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err = p.Run()
	return err
}
