// Package debugger: see session.go for the package doc.
package debugger

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nektos/act/pkg/model"
	"github.com/nektos/act/pkg/runner"
)

// GCPIdentity is the host-resolved ambient GCP credentials the CLI passes in for the
// opt-in ambient fallback (CLAUDE.md §4). The default identity path is bring-a-credential
// (a service-account key, see Options.GCPKeyJSON); ambient is used only when the dev opts
// in with -gcp-ambient. Locally there is no GitHub OIDC issuer, so a federated
// `google-github-actions/auth` (WIF) can't mint a token; ambient intercepts that step and
// injects the dev's already-present credentials. The core never shells out to gcloud —
// discovery/minting is a host concern (cmd/actl); the core only consumes the data.
type GCPIdentity struct {
	CredentialFile string // host path to the ADC json, bind-mounted ro into the container
	AccessToken    string // ambient ADC access token → CLOUDSDK_AUTH_ACCESS_TOKEN
	Account        string // local identity, for the transparency line (best-effort, may be empty)
}

// AWSIdentity is the host-resolved ambient AWS credentials for the opt-in ambient
// fallback (CLAUDE.md §4), the AWS analog of GCPIdentity. The default is bring-a-credential
// (static keys, see Options.AWSAccessKeyID/AWSSecretAccessKey); ambient is used only with
// -aws-ambient. Unlike GCP these are env-only — no file is mounted — so the core just
// injects them at the step's position. Discovery is a host concern (cmd/actl).
type AWSIdentity struct {
	AccessKeyID     string // → AWS_ACCESS_KEY_ID
	SecretAccessKey string // → AWS_SECRET_ACCESS_KEY
	SessionToken    string // → AWS_SESSION_TOKEN (empty for long-lived creds)
	Account         string // local caller identity (arn), for the transparency line (best-effort)
}

// container path the ambient ADC file is mounted at (matches the env we inject).
const gcpCredsContainerPath = "/actl/gcp/adc.json"

// Reserved secret names the brought-credential substitution loads into Config.Secrets so
// act's valueMasker (runner/logger.go) masks them in logs, then references from the
// rewritten step's `with:` via ${{ secrets.<name> }}. CLAUDE.md §4.
const (
	gcpKeySecret       = "ACTL_GCP_CREDENTIALS_JSON"
	azureCredsSecret   = "ACTL_AZURE_CREDENTIALS"
	awsKeyIDSecret     = "ACTL_AWS_ACCESS_KEY_ID"
	awsSecretKeySecret = "ACTL_AWS_SECRET_ACCESS_KEY"
)

// Auth action refs intercepted for identity handling (§4). Exported so a host-side
// pre-scan (cmd/actl) can keep cloud-CLI invocation lazy without re-spelling the literals.
const (
	GCPAuthAction   = "google-github-actions/auth"
	AWSAuthAction   = "aws-actions/configure-aws-credentials"
	AzureAuthAction = "azure/login"
)

// isDefaultCheckout reports whether a step is `actions/checkout` with no input
// that changes which code or where it lands (ref/repository/path). Only those are
// safe to satisfy from the local working tree; anything else stays a real clone.
func isDefaultCheckout(st *model.Step) bool {
	if st.Uses != "actions/checkout" && !strings.HasPrefix(st.Uses, "actions/checkout@") {
		return false
	}
	for _, k := range []string{"ref", "repository", "path"} {
		if st.With[k] != "" {
			return false
		}
	}
	return true
}

// checkoutWantsSubmodules reports whether an actions/checkout step asked for its
// git submodules, matching the action's own semantics: the `submodules:` input
// defaults to false and only `true` or `recursive` fetch them. Anything else
// (absent, "false", empty) leaves submodules out, so the local workspace copy
// skips them too.
func checkoutWantsSubmodules(st *model.Step) bool {
	switch strings.ToLower(strings.TrimSpace(st.With["submodules"])) {
	case "true", "recursive":
		return true
	default:
		return false
	}
}

// stepInterceptor neutralizes a class of steps that can't run faithfully on a local
// runner and substitutes the real local effect at the step's position. Built once in New;
// the barrier and the container wiring then iterate []stepInterceptor uniformly. Used for
// the checkout substitution and the opt-in ambient cloud-auth fallback (the default
// bring-a-credential identity path rewrites the step in place instead and needs no
// barrier participation — the real action runs).
type stepInterceptor struct {
	name      string                                                       // "checkout", "gcp-auth" — names the non-fatal failure log
	steps     map[int]bool                                                 // indices it rewrote to no-ops, within the debugged job
	inject    func(ctx context.Context, info runner.StepBarrierInfo) error // run at BarrierBefore on an owned index for the target job; nil = nothing to substitute
	container string                                                       // docker flags appended to Config.ContainerOptions; "" = none
}

// No-op scripts the neutralized steps run in place of their real action (so the step
// still shows in logs that actl handled it).
const (
	checkoutNoopMsg = `echo "actl: checkout intercepted — using your local working tree"`
	gcpAuthNoopMsg  = `echo "actl: GCP auth intercepted — no credential, see transparency notice"`
	awsAuthNoopMsg  = `echo "actl: AWS auth intercepted — no credential, see transparency notice"`
	azureNoopMsg    = `echo "actl: Azure auth intercepted — no credential, see transparency notice"`
)

// noopStep neutralizes a step: clear its action and inputs, preserve its label (so the
// rewritten step still shows its origin), and run msg in place. The shared mechanic for
// every step actl renders inert.
func noopStep(st *model.Step, msg string) {
	if st.Name == "" {
		st.Name = st.Uses // keep the original label visible after we clear Uses
	}
	st.Uses = ""
	st.With = nil
	st.Run = msg
}

// interceptSteps neutralizes every step matching `match` (via noopStep) and returns the
// rewritten indices. `capture`, if non-nil, runs on each matched step BEFORE its
// Uses/With are cleared (e.g. to read a step's inputs).
func interceptSteps(steps []*model.Step, match func(*model.Step) bool, msg string, capture func(*model.Step)) map[int]bool {
	out := map[int]bool{}
	for i, st := range steps {
		if !match(st) {
			continue
		}
		if capture != nil {
			capture(st)
		}
		noopStep(st, msg)
		out[i] = true
	}
	return out
}

// StepUses reports whether step st uses action, matching both the bare ref
// ("owner/repo") and a pinned version ("owner/repo@v2"). The one place actl
// decides what "this step uses X" means.
func StepUses(st *model.Step, action string) bool {
	return st.Uses == action || strings.HasPrefix(st.Uses, action+"@")
}

// scanIdentity classifies a cloud's auth steps into declared (secret/key mode — left to
// run untouched, faithful) and federated (need help locally), returning the federated
// step indices and a partially-filled summary (Cloud/Mode set by the caller). This is the
// shared front half of every identity builder.
func scanIdentity(steps []*model.Step, action string, keyMode func(*model.Step) bool, target func(*model.Step) string) ([]int, IdentitySummary) {
	var fed []int
	var sum IdentitySummary
	for i, st := range steps {
		if !StepUses(st, action) {
			continue
		}
		if keyMode(st) {
			sum.Declared = append(sum.Declared, st.String())
			continue
		}
		sum.Steps = append(sum.Steps, st.String())
		sum.Targets = append(sum.Targets, target(st))
		fed = append(fed, i)
	}
	return fed, sum
}

// injectEnv returns a barrier inject that writes env into the live job env map (the same
// map SetEnv mutates), so the credential contract propagates to subsequent step execs.
func injectEnv(env map[string]string) func(context.Context, runner.StepBarrierInfo) error {
	return func(_ context.Context, info runner.StepBarrierInfo) error {
		for k, v := range env {
			info.Env[k] = v
		}
		return nil
	}
}

// indexSet turns a slice of step indices into the set form stepInterceptor.steps uses.
func indexSet(idx []int) map[int]bool {
	m := make(map[int]bool, len(idx))
	for _, i := range idx {
		m[i] = true
	}
	return m
}

// jsonField best-effort extracts a string field from a JSON credential blob, for the
// transparency line (e.g. the SA key's client_email). "" on any failure.
func jsonField(content, key string) string {
	var m map[string]any
	if json.Unmarshal([]byte(content), &m) != nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// --- GCP (google-github-actions/auth) ---------------------------------------------

// gcpKeyMode reports whether an auth step already declares a service-account key
// (credentials_json) — i.e. it authenticates without federation and runs faithfully
// locally, so we leave it alone.
func gcpKeyMode(st *model.Step) bool { return st.With["credentials_json"] != "" }

// gcpTarget describes the federation a WIF auth step declares, for the transparency
// line — read before we clear With. Falls back to a generic label if unset.
func gcpTarget(st *model.Step) string {
	sa := st.With["service_account"]
	wip := st.With["workload_identity_provider"]
	switch {
	case sa != "" && wip != "":
		return sa + " via " + wip
	case sa != "":
		return sa
	case wip != "":
		return wip
	default:
		return "(federated identity)"
	}
}

// rewriteGCPToKey converts a federated (WIF) auth step into service-account-key mode,
// referencing the brought key as a masked secret expression — the real action then runs
// a faithful key auth. CLAUDE.md §4 (bring-a-credential default).
func rewriteGCPToKey(st *model.Step) {
	if st.With == nil {
		st.With = map[string]string{}
	}
	delete(st.With, "workload_identity_provider")
	delete(st.With, "service_account")
	st.With["credentials_json"] = "${{ secrets." + gcpKeySecret + " }}"
}

// gcpAmbientEnv assembles the ambient credential env injected at the auth step's position
// (the opt-in fallback). nil when there's nothing to inject.
func gcpAmbientEnv(id *GCPIdentity) map[string]string {
	env := map[string]string{}
	if id.CredentialFile != "" {
		// Client libraries (Go/Python/terraform's google provider) discover ADC here.
		env["GOOGLE_APPLICATION_CREDENTIALS"] = gcpCredsContainerPath
	}
	if id.AccessToken != "" {
		// Authenticates gcloud/gsutil/bq directly (any ADC type).
		env["CLOUDSDK_AUTH_ACCESS_TOKEN"] = id.AccessToken
	}
	if len(env) == 0 {
		return nil
	}
	return env
}

// gcpBind returns the docker volume option mounting the ambient ADC file read-only into
// the job container (the opt-in fallback). "" when there's no file. This is the path that
// puts a refresh-token-bearing file in the container — hence ambient is opt-in + warned.
func gcpBind(id *GCPIdentity) string {
	if id == nil || id.CredentialFile == "" {
		return ""
	}
	return fmt.Sprintf("-v %s:%s:ro", id.CredentialFile, gcpCredsContainerPath)
}

// buildGCPIdentity classifies the job's google-github-actions/auth steps and handles the
// federated ones by the identity strategy (CLAUDE.md §4): a brought SA key rewrites them
// to key mode so the real action runs (default); else the opt-in ambient fallback no-ops
// them and injects ambient creds; else they're neutralized with an honest summary. Returns
// the (possibly empty) ambient interceptor, the reserved secrets to register, and the
// summary. Key-mode steps are left untouched to run faithfully.
func buildGCPIdentity(steps []*model.Step, keyJSON string, ambient *GCPIdentity) (stepInterceptor, map[string]string, IdentitySummary) {
	fed, sum := scanIdentity(steps, GCPAuthAction, gcpKeyMode, gcpTarget)
	sum.Cloud = "GCP"
	if len(fed) == 0 {
		if len(sum.Declared) > 0 {
			sum.Mode = AuthDeclared
		}
		return stepInterceptor{}, nil, sum
	}
	switch {
	case keyJSON != "":
		for _, i := range fed {
			rewriteGCPToKey(steps[i])
		}
		sum.Mode = AuthSubstituted
		sum.Account = jsonField(keyJSON, "client_email")
		return stepInterceptor{}, map[string]string{gcpKeySecret: keyJSON}, sum
	case ambient != nil:
		for _, i := range fed {
			noopStep(steps[i], gcpAuthNoopMsg)
		}
		itc := stepInterceptor{name: "gcp-auth", steps: indexSet(fed), container: gcpBind(ambient)}
		if env := gcpAmbientEnv(ambient); len(env) > 0 {
			itc.inject = injectEnv(env)
		}
		sum.Mode = AuthAmbient
		sum.Account = ambient.Account
		sum.File = ambient.CredentialFile != ""
		sum.Token = ambient.AccessToken != ""
		return itc, nil, sum
	default:
		for _, i := range fed {
			noopStep(steps[i], gcpAuthNoopMsg)
		}
		sum.Mode = AuthUnsatisfied
		return stepInterceptor{}, nil, sum
	}
}

// --- AWS (aws-actions/configure-aws-credentials) ----------------------------------

// awsKeyMode reports whether an auth step already declares static access keys — it runs
// faithfully locally without federation, so we leave it alone.
func awsKeyMode(st *model.Step) bool {
	return st.With["aws-access-key-id"] != "" && st.With["aws-secret-access-key"] != ""
}

// awsTarget describes the federation a role-to-assume auth step declares (role + region),
// for the transparency line — read before we clear With. Falls back to a generic label.
func awsTarget(st *model.Step) string {
	role := st.With["role-to-assume"]
	region := st.With["aws-region"]
	switch {
	case role != "" && region != "":
		return role + " in " + region
	case role != "":
		return role
	case region != "":
		return "region " + region
	default:
		return "(federated identity)"
	}
}

// firstAWSRegion returns the first literal aws-region declared by a federated auth step
// (expressions skipped — we'd inject the raw unevaluated string otherwise). The action
// exports the region, so reproducing it is faithful.
func firstAWSRegion(steps []*model.Step, fed []int) string {
	for _, i := range fed {
		if r := steps[i].With["aws-region"]; r != "" && !strings.Contains(r, "${{") {
			return r
		}
	}
	return ""
}

// rewriteAWSToKeys converts a federated (role-to-assume) auth step into static-key mode,
// referencing the brought keys as masked secret expressions; the declared aws-region is
// kept so the real action exports it. Direct credentials, no sts:AssumeRole (CLAUDE.md §4).
func rewriteAWSToKeys(st *model.Step) {
	if st.With == nil {
		st.With = map[string]string{}
	}
	for _, k := range []string{"role-to-assume", "web-identity-token-file", "role-chaining", "audience", "role-session-name"} {
		delete(st.With, k)
	}
	st.With["aws-access-key-id"] = "${{ secrets." + awsKeyIDSecret + " }}"
	st.With["aws-secret-access-key"] = "${{ secrets." + awsSecretKeySecret + " }}"
}

// awsAmbientEnv assembles the ambient credential env injected at the auth step's position
// (the opt-in fallback), plus the declared region. nil when there are no credentials.
func awsAmbientEnv(id *AWSIdentity, region string) map[string]string {
	if id.AccessKeyID == "" || id.SecretAccessKey == "" {
		return nil
	}
	env := map[string]string{
		"AWS_ACCESS_KEY_ID":     id.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY": id.SecretAccessKey,
	}
	if id.SessionToken != "" {
		env["AWS_SESSION_TOKEN"] = id.SessionToken // temporary (SSO / assumed-role) creds
	}
	if region != "" {
		env["AWS_REGION"] = region
		env["AWS_DEFAULT_REGION"] = region
	}
	return env
}

// buildAWSIdentity is the AWS analog of buildGCPIdentity. Federated role-to-assume steps
// are handled by: brought static keys → rewrite to key mode (default, real action runs);
// else opt-in ambient → no-op + inject ambient creds; else neutralize. Key-mode steps are
// left untouched.
func buildAWSIdentity(steps []*model.Step, keyID, secretKey string, ambient *AWSIdentity) (stepInterceptor, map[string]string, IdentitySummary) {
	fed, sum := scanIdentity(steps, AWSAuthAction, awsKeyMode, awsTarget)
	sum.Cloud = "AWS"
	if len(fed) == 0 {
		if len(sum.Declared) > 0 {
			sum.Mode = AuthDeclared
		}
		return stepInterceptor{}, nil, sum
	}
	region := firstAWSRegion(steps, fed)
	sum.Region = region
	switch {
	case keyID != "" && secretKey != "":
		for _, i := range fed {
			rewriteAWSToKeys(steps[i])
		}
		sum.Mode = AuthSubstituted
		return stepInterceptor{}, map[string]string{awsKeyIDSecret: keyID, awsSecretKeySecret: secretKey}, sum
	case ambient != nil:
		for _, i := range fed {
			noopStep(steps[i], awsAuthNoopMsg)
		}
		itc := stepInterceptor{name: "aws-auth", steps: indexSet(fed)}
		if env := awsAmbientEnv(ambient, region); len(env) > 0 {
			itc.inject = injectEnv(env)
		}
		sum.Mode = AuthAmbient
		sum.Account = ambient.Account
		return itc, nil, sum
	default:
		for _, i := range fed {
			noopStep(steps[i], awsAuthNoopMsg)
		}
		sum.Mode = AuthUnsatisfied
		return stepInterceptor{}, nil, sum
	}
}

// --- Azure (azure/login) ----------------------------------------------------------

// azureCredsMode reports whether an azure/login step already declares a service-principal
// secret (creds) — it runs faithfully locally without federation, so we leave it alone.
func azureCredsMode(st *model.Step) bool { return st.With["creds"] != "" }

// azureTarget describes the federation an OIDC azure/login step declares, for the
// transparency line — read before we clear With. Falls back to a generic label.
func azureTarget(st *model.Step) string {
	client := st.With["client-id"]
	tenant := st.With["tenant-id"]
	sub := st.With["subscription-id"]
	switch {
	case client != "" && tenant != "":
		return client + " in tenant " + tenant
	case client != "":
		return client
	case sub != "":
		return "subscription " + sub
	default:
		return "(federated identity)"
	}
}

// rewriteAzureToCreds converts a federated (OIDC) azure/login step into
// service-principal-secret mode, referencing the brought creds JSON as a masked secret
// expression; the real action then runs a faithful SP login. CLAUDE.md §4. Azure has no
// ambient fallback (it would mean mounting ~/.azure, a refresh-token-bearing personal
// credential — the worst-blast-radius variant of what this pivot moves away from).
func rewriteAzureToCreds(st *model.Step) {
	if st.With == nil {
		st.With = map[string]string{}
	}
	for _, k := range []string{"client-id", "tenant-id", "subscription-id", "auth-type"} {
		delete(st.With, k)
	}
	st.With["creds"] = "${{ secrets." + azureCredsSecret + " }}"
}

// buildAzureIdentity classifies the job's azure/login steps: creds-mode steps run
// untouched; a brought SP creds JSON rewrites federated steps to creds mode (the real
// action runs); else they're neutralized with an honest summary. There is no Azure ambient
// fallback, so this never returns an interceptor with steps.
func buildAzureIdentity(steps []*model.Step, credsJSON string) (stepInterceptor, map[string]string, IdentitySummary) {
	fed, sum := scanIdentity(steps, AzureAuthAction, azureCredsMode, azureTarget)
	sum.Cloud = "Azure"
	if len(fed) == 0 {
		if len(sum.Declared) > 0 {
			sum.Mode = AuthDeclared
		}
		return stepInterceptor{}, nil, sum
	}
	if credsJSON != "" {
		for _, i := range fed {
			rewriteAzureToCreds(steps[i])
		}
		sum.Mode = AuthSubstituted
		sum.Account = jsonField(credsJSON, "clientId")
		return stepInterceptor{}, map[string]string{azureCredsSecret: credsJSON}, sum
	}
	for _, i := range fed {
		noopStep(steps[i], azureNoopMsg)
	}
	sum.Mode = AuthUnsatisfied
	return stepInterceptor{}, nil, sum
}

// stepLabelsOf returns the labels of the steps whose indices are set in mark, in
// order — used to name intercepted steps (checkout) for transparency.
func stepLabelsOf(steps []*model.Step, mark map[int]bool) []string {
	var out []string
	for i, st := range steps {
		if mark[i] {
			out = append(out, st.String())
		}
	}
	return out
}
