// Package debugger: see session.go for the package doc.
package debugger

import (
	"context"
	"fmt"
	"strings"

	"github.com/nektos/act/pkg/model"
	"github.com/nektos/act/pkg/runner"
)

// GCPIdentity is the host-resolved ambient GCP credentials the CLI passes in for
// identity substitution (CLAUDE.md §4). Locally there is no GitHub OIDC issuer, so
// `google-github-actions/auth` can't federate; instead we intercept that step and
// inject the dev's already-present credentials. The core never shells out to gcloud
// — discovery/minting is a host concern (cmd/actl); the core only consumes the data.
type GCPIdentity struct {
	CredentialFile string // host path to the ADC json, bind-mounted ro into the container
	AccessToken    string // ambient ADC access token → CLOUDSDK_AUTH_ACCESS_TOKEN
	Account        string // local identity, for the transparency line (best-effort, may be empty)
}

// AWSIdentity is the host-resolved ambient AWS credentials the CLI passes in for
// identity substitution (CLAUDE.md §4), the AWS analog of GCPIdentity. Locally there
// is no GitHub OIDC issuer, so `aws-actions/configure-aws-credentials` can't federate
// a role; instead we intercept that step and inject the dev's already-resolved
// session credentials (e.g. from `aws configure export-credentials`). Unlike GCP these
// are env-only — no file is mounted — so the core just injects them at the step's
// position. Discovery is a host concern (cmd/actl); the core only consumes the data.
type AWSIdentity struct {
	AccessKeyID     string // → AWS_ACCESS_KEY_ID
	SecretAccessKey string // → AWS_SECRET_ACCESS_KEY
	SessionToken    string // → AWS_SESSION_TOKEN (empty for long-lived creds)
	Account         string // local caller identity (arn), for the transparency line (best-effort)
}

// container path the ambient ADC file is mounted at (matches the env we inject).
const gcpCredsContainerPath = "/actl/gcp/adc.json"

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
// runner (a default actions/checkout, a federated cloud-auth) and substitutes the
// real local effect at the step's position. Built once in New; the barrier and the
// container wiring then iterate []stepInterceptor uniformly, so AWS/Azure are one
// more builder + append — no new barrier code and no new Session fields.
type stepInterceptor struct {
	name      string                                                       // "checkout", "gcp-auth" — names the non-fatal failure log
	steps     map[int]bool                                                 // indices it rewrote to no-ops, within the debugged job
	inject    func(ctx context.Context, info runner.StepBarrierInfo) error // run at BarrierBefore on an owned index for the target job; nil = nothing to substitute
	container string                                                       // docker flags appended to Config.ContainerOptions; "" = none
}

// No-op scripts the rewritten steps run in place of their real action (so the step
// still shows in logs that actl handled it).
const (
	checkoutNoopMsg = `echo "actl: checkout intercepted — using your local working tree"`
	gcpAuthNoopMsg  = `echo "actl: GCP auth intercepted — using ambient identity"`
	awsAuthNoopMsg  = `echo "actl: AWS auth intercepted — using ambient identity"`
)

// interceptSteps rewrites every step matching `match` to a no-op echoing `msg`,
// preserving its label (so the rewritten step still shows its origin), and returns
// the rewritten indices. `capture`, if non-nil, runs on each matched step BEFORE its
// Uses/With are cleared (e.g. to read a cloud-auth step's federation target). This is
// the shared mechanic every interceptor's scan/rewrite is built from.
func interceptSteps(steps []*model.Step, match func(*model.Step) bool, msg string, capture func(*model.Step)) map[int]bool {
	out := map[int]bool{}
	for i, st := range steps {
		if !match(st) {
			continue
		}
		if capture != nil {
			capture(st)
		}
		if st.Name == "" {
			st.Name = st.Uses // keep the original label visible after we clear Uses
		}
		st.Uses = ""
		st.With = nil
		st.Run = msg
		out[i] = true
	}
	return out
}

// Auth action refs intercepted for ambient-identity substitution (§4). Exported
// so a host-side pre-scan (cmd/actl) can keep cloud-CLI invocation lazy without
// re-spelling the literals.
const (
	GCPAuthAction = "google-github-actions/auth"
	AWSAuthAction = "aws-actions/configure-aws-credentials"
)

// StepUses reports whether step st uses action, matching both the bare ref
// ("owner/repo") and a pinned version ("owner/repo@v2"). The one place actl
// decides what "this step uses X" means.
func StepUses(st *model.Step, action string) bool {
	return st.Uses == action || strings.HasPrefix(st.Uses, action+"@")
}

// isGCPAuth reports whether a step uses google-github-actions/auth (any version).
func isGCPAuth(st *model.Step) bool { return StepUses(st, GCPAuthAction) }

// buildGCPInterceptor scans and rewrites each google-github-actions/auth step to a
// no-op (so it doesn't try to federate against a GitHub OIDC issuer that doesn't
// exist locally), then assembles the substitution: the ambient credential env to
// inject at the step's position and the read-only ADC volume mount. Returns the
// interceptor plus a redacted GCPSummary for the transparency line. With no auth step
// the interceptor owns nothing (and isn't attached); with a step but no identity it
// still neutralizes the step so the job survives, injecting nothing.
func buildGCPInterceptor(id *GCPIdentity, steps []*model.Step) (stepInterceptor, GCPSummary) {
	var targets []string
	gcpSteps := interceptSteps(steps, isGCPAuth, gcpAuthNoopMsg, func(st *model.Step) {
		targets = append(targets, gcpTarget(st))
	})
	env, summary := buildGCP(id, gcpSteps, targets)
	summary.Steps = stepLabelsOf(steps, gcpSteps)

	itc := stepInterceptor{name: "gcp-auth", steps: gcpSteps, container: gcpBind(id, len(gcpSteps) > 0)}
	if len(env) > 0 {
		// info.Env is the live job env map (the same one SetEnv mutates), so writing
		// here propagates the credential contract to subsequent step execs.
		itc.inject = func(_ context.Context, info runner.StepBarrierInfo) error {
			for k, v := range env {
				info.Env[k] = v
			}
			return nil
		}
	}
	return itc, summary
}

// gcpTarget describes the federation an auth step declares, for the transparency
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

// buildGCP assembles the credential env injected at the auth step's position and a
// redacted summary for the transparency line. With no identity (id == nil) it
// returns no env but still summarizes the intercepted steps, so the UI can say the
// steps were neutralized but cloud calls will fail.
func buildGCP(id *GCPIdentity, steps map[int]bool, targets []string) (map[string]string, GCPSummary) {
	summary := GCPSummary{Targets: targets}
	if len(steps) == 0 {
		return nil, GCPSummary{} // no auth step → nothing to report
	}
	if id == nil {
		return nil, summary
	}
	env := map[string]string{}
	if id.CredentialFile != "" {
		// Client libraries (Go/Python/terraform's google provider) discover ADC here.
		// We deliberately do NOT set GOOGLE_GHA_CREDS_PATH: setup-gcloud consumes it to
		// run `gcloud auth login --cred-file`, which rejects the authorized_user ADC
		// that `gcloud auth application-default login` produces ("only external/service
		// account JSON supported"). The access token below authenticates gcloud
		// universally instead, so that path is both unnecessary and harmful here.
		env["GOOGLE_APPLICATION_CREDENTIALS"] = gcpCredsContainerPath
		summary.File = true
	}
	if id.AccessToken != "" {
		// Authenticates gcloud/gsutil/bq directly (any ADC type), and satisfies
		// setup-gcloud's own `isAuthenticated()` check so it doesn't warn.
		env["CLOUDSDK_AUTH_ACCESS_TOKEN"] = id.AccessToken
		summary.Token = true
	}
	// We deliberately do NOT inject a project. In real CI the project comes from the
	// workflow (the auth step's project_id input, or an explicit --project / env on
	// the command); fabricating one from the host's gcloud config wouldn't exist on
	// GitHub. If a run needs GOOGLE_CLOUD_PROJECT, supply it via the generic env path
	// (.env / -env), same as any other env var.
	summary.Account = id.Account
	if len(env) == 0 {
		env = nil
	}
	return env, summary
}

// gcpBind returns the docker volume option that mounts the ambient ADC file
// read-only into the job container, or "" when there's no file to mount or no auth
// step to satisfy. The mount path matches the GOOGLE_APPLICATION_CREDENTIALS env.
func gcpBind(id *GCPIdentity, hasAuthSteps bool) string {
	if id == nil || id.CredentialFile == "" || !hasAuthSteps {
		return ""
	}
	return fmt.Sprintf("-v %s:%s:ro", id.CredentialFile, gcpCredsContainerPath)
}

// isAWSAuth reports whether a step uses aws-actions/configure-aws-credentials (any version).
func isAWSAuth(st *model.Step) bool { return StepUses(st, AWSAuthAction) }

// awsTarget describes the federation an auth step declares (role + region), for the
// transparency line — read before we clear With. Falls back to a generic label.
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
		return "(static credentials)"
	}
}

// buildAWSInterceptor scans and rewrites each aws-actions/configure-aws-credentials
// step to a no-op (so it doesn't try to assume a role via a GitHub OIDC token that
// can't be minted locally), then assembles the substitution: the ambient session
// credentials to inject at the step's position plus the region the step declared.
// Returns the interceptor and a redacted AWSSummary. AWS is env-only — no file mount
// — so unlike GCP the interceptor sets no container option.
func buildAWSInterceptor(id *AWSIdentity, steps []*model.Step) (stepInterceptor, AWSSummary) {
	var targets []string
	var region string
	awsSteps := interceptSteps(steps, isAWSAuth, awsAuthNoopMsg, func(st *model.Step) {
		targets = append(targets, awsTarget(st))
		// Capture the first declared literal region; the action exports it, so
		// reproducing it is faithful (unlike GCP's project, region is a declared input
		// here). Skip expressions — we'd inject the raw unevaluated string otherwise.
		if region == "" {
			if r := st.With["aws-region"]; r != "" && !strings.Contains(r, "${{") {
				region = r
			}
		}
	})
	env, summary := buildAWS(id, awsSteps, targets, region)
	summary.Steps = stepLabelsOf(steps, awsSteps)

	itc := stepInterceptor{name: "aws-auth", steps: awsSteps}
	if len(env) > 0 {
		// info.Env is the live job env map (the same one SetEnv mutates), so writing
		// here propagates the credential contract to subsequent step execs.
		itc.inject = func(_ context.Context, info runner.StepBarrierInfo) error {
			for k, v := range env {
				info.Env[k] = v
			}
			return nil
		}
	}
	return itc, summary
}

// buildAWS assembles the credential env injected at the auth step's position and a
// redacted summary for the transparency line. With no identity (id == nil) it returns
// no env but still summarizes the intercepted steps, so the UI can say the steps were
// neutralized but cloud calls will fail. The region (from the step's declared
// aws-region) is injected only alongside credentials — region without creds is useless.
func buildAWS(id *AWSIdentity, steps map[int]bool, targets []string, region string) (map[string]string, AWSSummary) {
	summary := AWSSummary{Targets: targets, Region: region}
	if len(steps) == 0 {
		return nil, AWSSummary{} // no auth step → nothing to report
	}
	if id == nil {
		return nil, summary
	}
	env := map[string]string{}
	if id.AccessKeyID != "" && id.SecretAccessKey != "" {
		env["AWS_ACCESS_KEY_ID"] = id.AccessKeyID
		env["AWS_SECRET_ACCESS_KEY"] = id.SecretAccessKey
		summary.Creds = true
		if id.SessionToken != "" {
			env["AWS_SESSION_TOKEN"] = id.SessionToken // temporary (SSO / assumed-role) creds
		}
		if region != "" {
			env["AWS_REGION"] = region
			env["AWS_DEFAULT_REGION"] = region
			summary.RegionSet = true
		}
	}
	summary.Account = id.Account
	if len(env) == 0 {
		env = nil
	}
	return env, summary
}

// stepLabelsOf returns the labels of the steps whose indices are set in mark, in
// order — used to name intercepted steps (checkout, GCP auth) for transparency.
func stepLabelsOf(steps []*model.Step, mark map[int]bool) []string {
	var out []string
	for i, st := range steps {
		if mark[i] {
			out = append(out, st.String())
		}
	}
	return out
}
