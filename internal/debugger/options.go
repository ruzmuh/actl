// Package debugger: see session.go for the package doc.
package debugger

// EnvOverlay is a per-`environment:` overlay of secrets/vars applied when the debugged
// job targets that deployment environment (GHA scopes secrets/vars by `environment:`).
// The host (cmd/actl) resolves each environment's secret-file/inline-vars into these
// flat maps; the core merges the matching one over the flat defaults (CLI overrides
// still win — see New).
type EnvOverlay struct {
	Secrets map[string]string
	Vars    map[string]string
}

// Options configures a debug Session. Only WorkflowPath is required.
type Options struct {
	WorkflowPath    string                     // path to the workflow file
	EventName       string                     // event to plan for (default "push")
	JobID           string                     // which job to debug; required only if the event plans more than one
	Matrix          map[string]map[string]bool // matrix combination to pin (act's Config.Matrix shape: key→value→true); required only if the job's matrix has more than one combination
	WithDeps        bool                       // run the job's upstream needs for real (to completion) before debugging it, instead of isolating
	Image           string                     // docker image mapped to ubuntu-latest when Images is empty (default catthehacker); back-compat sugar for Images
	Images          map[string]string          // runner label → docker image (act's -P/Platforms map); empty falls back to {ubuntu-latest: Image}
	Workdir         string                     // workspace bind-mounted into the container so local 'uses: ./' actions resolve; empty = an isolated empty temp dir (steps can't write to your tree). NOTE: a set workdir is mounted, so steps can write to it
	Source          string                     // working tree a default actions/checkout copies into the workspace (no host mutation); empty = current dir. Ignored when Workdir is set
	Secrets         map[string]string          // secrets.* base (flat defaults, e.g. from secret-file); the env overlay and SecretOverrides layer on top
	Vars            map[string]string          // vars.* base (flat defaults); the env overlay and VarOverrides layer on top
	Env             map[string]string          // extra env for containers
	Environments    map[string]EnvOverlay      // per-`environment:` secrets/vars overlays, keyed by environment name; the one matching the debugged job's `environment:` is merged over Secrets/Vars
	SecretOverrides map[string]string          // secrets.* applied last (after the env overlay) so an explicit CLI -secret wins; nil for library callers
	VarOverrides    map[string]string          // vars.* applied last (after the env overlay) so an explicit CLI -var wins; nil for library callers
	// Cloud identity. The default is bring-a-credential: a scoped
	// non-personal credential rewrites a federated auth step to its secret/key mode so
	// the real action runs. Ambient personal login is an opt-in fallback (GCP/AWS only;
	// Azure has none) — non-nil GCP/AWS means -…-ambient was set and the creds resolved.
	GCPKeyJSON         string                // service-account key JSON content → rewrites a federated google-github-actions/auth to credentials_json mode
	AzureCredsJSON     string                // service-principal creds JSON content → rewrites a federated azure/login to creds mode
	AWSAccessKeyID     string                // brought static access key id → rewrites a federated aws-actions/configure-aws-credentials to static-key mode
	AWSSecretAccessKey string                // brought static secret access key (paired with AWSAccessKeyID)
	GCP                *GCPIdentity          // ambient GCP creds for the opt-in fallback (nil unless -gcp-ambient)
	AWS                *AWSIdentity          // ambient AWS creds for the opt-in fallback (nil unless -aws-ambient)
	Needs              map[string]NeedsInput // seeded needs.<job>.* for isolated debugging, keyed by upstream job id (ignored with WithDeps)
	BreakOnError       bool                  // in Continue mode, halt after a step that errored
	Breakpoints        []int                 // zero-based step indices to halt before, in Continue mode
	BreakpointNames    []string              // step names to halt before, resolved to indices against the job's steps in New (a name with no matching step is an error)

	// Runtime context GitHub injects in real CI that a clean local runner lacks
	// — all seed-and-be-honest surfaces, each with a transparency line.
	GitHubToken string            // GITHUB_TOKEN → github.token (and mirrored into secrets.GITHUB_TOKEN); empty falls back to Secrets["GITHUB_TOKEN"]
	Inputs      map[string]string // workflow_dispatch/workflow_call inputs.* (user values; act applies declared defaults + typing on top)
	EventPath   string            // path to a github.event payload JSON; empty = "{}" (plus any Inputs)
	Repository  string            // override github.repository (env GITHUB_REPOSITORY); empty = act derives from local git
	Ref         string            // override github.ref (env GITHUB_REF); empty = act derives from local git
	Sha         string            // override github.sha (env SHA_REF, act's read key); empty = act derives from local git
	Actor       string            // override github.actor (Config.Actor); empty = act's "nektos/act" placeholder
	// GitHubOverrides names the github.* fields the user set explicitly by flag (any of
	// "repository"/"ref"/"sha"/"actor"), so the transparency line marks those as overrides.
	// The values above may also be filled from local git for an honest display — only an
	// entry here means the user overrode it, not merely that the value is non-empty.
	GitHubOverrides []string
}
