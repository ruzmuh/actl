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

	"github.com/ruzmuh/actl/internal/debugger"
	"github.com/ruzmuh/actl/internal/tui"
	"github.com/ruzmuh/actl/internal/workflow"
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
	gcpIdentity := flag.Bool("gcp-identity", true, "substitute ambient gcloud ADC for a federated google-github-actions/auth step (=false leaves it untouched)")
	gcpCreds := flag.String("gcp-credentials", "", "path to the ADC json to inject (default: discover gcloud's application_default_credentials.json)")
	awsIdentity := flag.Bool("aws-identity", true, "substitute ambient AWS credentials for a federated aws-actions/configure-aws-credentials step (=false leaves it untouched)")
	awsProfile := flag.String("aws-profile", "", "AWS profile to resolve ambient credentials from (default: the default profile / environment)")
	githubToken := flag.String("github-token", "", "value for github.token / secrets.GITHUB_TOKEN (default: GITHUB_TOKEN from .secrets, else ambient 'gh auth token')")
	eventFile := flag.String("event-file", "", "path to a github.event payload JSON (sets github.event.*)")
	repository := flag.String("repository", "", "override github.repository (default: derive owner/repo from the local git 'origin' remote)")
	ref := flag.String("ref", "", "override github.ref, e.g. refs/heads/main (default: derive from the local git HEAD)")
	sha := flag.String("sha", "", "override github.sha (default: derive from the local git HEAD)")
	actor := flag.String("actor", "", "override github.actor (default: act's 'nektos/act' placeholder)")
	var needs, envs, secrets, vars, inputs, matrix stringSlice
	flag.Var(&matrix, "matrix", "pin a matrix combination: 'KEY=VALUE' (repeatable; required only if the job's matrix has more than one combination)")
	flag.Var(&needs, "need", "seed an upstream needs value: 'JOB.outputs.NAME=VALUE' or 'JOB.result=VALUE' (repeatable)")
	flag.Var(&envs, "env", "set an env var for the run: 'KEY=VALUE' (repeatable, overrides -env-file)")
	flag.Var(&secrets, "secret", "set a secret for the run: 'KEY=VALUE' (repeatable, overrides -secret-file)")
	flag.Var(&vars, "var", "set a var for the run: 'KEY=VALUE' (repeatable, overrides -var-file)")
	flag.Var(&inputs, "input", "set a workflow_dispatch/workflow_call input: 'NAME=VALUE' (repeatable)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: actl [flags] [workflow.yml]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	path, err := resolveWorkflowPath(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "actl:", err)
		os.Exit(2)
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

	// Ambient GCP identity: only resolved (and gcloud invoked) when the workflow
	// actually has a google-github-actions/auth step — kept lazy so non-GCP runs
	// never shell out. Host concern: the core consumes the resolved data.
	var gcp *debugger.GCPIdentity
	if *gcpIdentity {
		gcp = gatherGCPIdentity(path, *gcpCreds)
	}

	// Ambient AWS identity: same laziness — only resolved (and the aws CLI invoked)
	// when the workflow actually has an aws-actions/configure-aws-credentials step.
	var aws *debugger.AWSIdentity
	if *awsIdentity {
		aws = gatherAWSIdentity(path, *awsProfile)
	}

	// github.token: explicit flag wins; the core falls back to a GITHUB_TOKEN in
	// .secrets; failing both, try the dev's ambient `gh` login (best-effort, like
	// gcloud for GCP). The transparency line makes the substitution loud.
	token := *githubToken
	if token == "" && secretMap["GITHUB_TOKEN"] == "" {
		token = ghValue("auth", "token")
	}

	// github.* runtime context: each flag wins, otherwise derive from the local git
	// repo (the source tree being debugged) so the transparency line shows real
	// values; empty leaves it for act to derive.
	gitDir := firstNonEmpty(*source, *workdir, ".")
	repo, gref, gsha := resolveGitHubContext(gitDir, *repository, *ref, *sha)

	opts := debugger.Options{
		WorkflowPath: path,
		EventName:    *event,
		JobID:        *job,
		Matrix:       parseMatrixSel(matrix),
		WithDeps:     *withDeps,
		Workdir:      *workdir,
		Source:       *source,
		Image:        *image,
		BreakOnError: *breakOnError,
		Needs:        needsMap,
		Secrets:      secretMap,
		Vars:         varMap,
		Env:          envMap,
		GCP:          gcp,
		AWS:          aws,
		GitHubToken:  token,
		Inputs:       parseKeyVals(inputs),
		EventPath:    *eventFile,
		Repository:   repo,
		Ref:          gref,
		Sha:          gsha,
		Actor:        *actor,
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
func resolveWorkflowPath(arg string) (string, error) {
	if arg != "" {
		return arg, nil
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

// gatherGCPIdentity resolves the dev's ambient GCP credentials for substitution,
// but only when the workflow actually has a google-github-actions/auth step — so a
// non-GCP run never shells out to gcloud. Discovery and token minting are
// best-effort: a missing piece just narrows what we can inject (the core still
// neutralizes the auth step and the TUI reports honestly). Returns nil when there's
// no auth step, or neither a credential file nor a token could be found.
func gatherGCPIdentity(path, credOverride string) *debugger.GCPIdentity {
	if !workflowHasGCPAuth(path) {
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

// workflowHasGCPAuth reports whether any job in the workflow uses
// google-github-actions/auth (a cheap pre-scan to keep gcloud invocation lazy).
func workflowHasGCPAuth(path string) bool {
	wf, err := workflow.Load(path, true)
	if err != nil {
		return false // a real parse error surfaces later in debugger.New
	}
	for _, job := range wf.Jobs {
		for _, st := range job.Steps {
			if st.Uses == "google-github-actions/auth" || strings.HasPrefix(st.Uses, "google-github-actions/auth@") {
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
	if !workflowHasAWSAuth(path) {
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

// workflowHasAWSAuth reports whether any job uses aws-actions/configure-aws-credentials
// (a cheap pre-scan to keep the aws CLI invocation lazy).
func workflowHasAWSAuth(path string) bool {
	wf, err := workflow.Load(path, true)
	if err != nil {
		return false // a real parse error surfaces later in debugger.New
	}
	for _, job := range wf.Jobs {
		for _, st := range job.Steps {
			if st.Uses == "aws-actions/configure-aws-credentials" || strings.HasPrefix(st.Uses, "aws-actions/configure-aws-credentials@") {
				return true
			}
		}
	}
	return false
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
