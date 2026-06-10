# actl

A **TUI-first, interactive step-debugger for GitHub Actions workflows** that runs them
locally — pause before any step, inspect the environment, drop into the job container,
re-run a step — with **faithful `uses:` execution**, because it stands on
[`nektos/act`](https://github.com/nektos/act) instead of reimplementing the Actions engine.

https://github.com/user-attachments/assets/586972ef-c879-4aff-b102-151f455c3263

## What you get

`actl` runs a real GitHub Actions job locally through act's engine and lets you **debug it
like code** — instead of the push-and-pray loop of editing YAML and waiting on CI:

- ⏸  **Pause before or after any step** — set breakpoints and run-to-cursor, or single-step
  the whole job. Stop *before* anything runs, just like you can't on GitHub.
- 🐚  **Drop into the live job container** mid-run — a real shell in the same container act
  execs steps into, with the workspace and env exactly as the next step will see them.
- 🔍  **Inspect the environment** at any pause — the job-scoped env the next step runs with.
- ✏️  **Edit a step's command or env and re-run it in place** — no commit, no push, no
  re-trigger.
- 🎯  **Faithful `uses:`** — docker / composite / node actions run through act's *real*
  engine, not a partial reimplementation.
- 📥  **Faithful `actions/checkout`** — intercepted to copy your working tree (including
  uncommitted changes) at the checkout step's position, so steps before it see an empty
  workspace and steps after see your code, exactly as on GitHub.
- ☁️  **Cloud identity, locally** — federated `gcp` and `aws` logins run under your ambient
  `gcloud` / `aws` session so the auth step just works (Azure next).
- 🧩  **Real-workflow ergonomics** — job selection, matrix pinning, `services:`, secrets /
  vars / env, `needs` seeding, GitHub runtime-context seeding, per-`environment:` overlays,
  a committable `.actl.yml`, and a Docker-free `-list` inventory.

Every substitution actl makes prints a **transparency line** — it tells you exactly where
local execution differs from CI (whose identity you're running as, what it seeded, what it
mounted), so you're never silently testing something other than your workflow.

## Install

All three routes need **Docker** at runtime — `actl` starts a real job container via act.

**Homebrew (macOS):**

```sh
brew install ruzmuh/actl/actl
```

(A Homebrew *cask* — macOS only. On Linux, use a prebuilt binary or build from source.)

**Prebuilt binary (macOS & Linux, amd64/arm64):** grab the archive for your platform from
the [latest release](https://github.com/ruzmuh/actl/releases/latest), then:

```sh
tar -xzf actl_*_$(uname -s)_$(uname -m).tar.gz
sudo mv actl /usr/local/bin/      # or anywhere on your PATH
```

**From source:** the act fork is a git submodule wired in via a filesystem `replace` in
`go.mod`, so `go install github.com/ruzmuh/actl/...@latest` **won't work** (it can't resolve
the local replace). Clone with the submodule first, then install from the checkout:

```sh
git clone --recurse-submodules https://github.com/ruzmuh/actl
cd actl
go install ./cmd/actl             # builds with the submodule on disk; binary lands in $GOBIN
```

Requires Go (the module pins the toolchain to match act; `go` auto-fetches it).

## Quick start

Run `actl` **from inside your repo**. With a single `.github/workflows/*.yml` it's picked up
automatically; with several, pass the path. `actl` debugs **one job at a time**.

```sh
# look inside first — jobs, steps, matrix, environments (no Docker, no network):
actl -list
actl -list .github/workflows/ci.yml   # if you have more than one workflow

# debug a job — pauses before the first step:
actl -job build
actl -job build .github/workflows/ci.yml
```

In the TUI: `s` step · `c` continue · `d` drop into the container shell · `e` env pane ·
`r` re-run the step · `q` quit ([full key list](#tui-keys)).

**Need inputs, an event, secrets, or a pinned image?** Drop a committable **`.actl.yml`**
next to your workflow instead of a wall of flags — `actl` auto-discovers it:

```yaml
# .actl.yml
job: build
event: workflow_dispatch
inputs:                       # for workflow_dispatch / workflow_call
  version: "1.2.3"
secret-file: .secrets         # gitignored file; secrets can't be inlined here
vars:
  REGION: us-east-1
```

Now a bare `actl` uses it; any CLI flag still wins for a one-off override.

**A few common scenarios:**

```sh
# a deploy job that depends on others — seed what upstream produced…
actl -job deploy -need 'build.outputs.image=ghcr.io/acme/app:1.4.2'
# …or run the upstream jobs for real, then debug deploy:
actl -job deploy --with-deps

# a job that logs into the cloud — your ambient gcloud / aws session is used automatically:
actl -job deploy            # google-github-actions/auth & configure-aws-credentials just work

# a matrix job — pin one combination (-list shows them):
actl -job test -matrix 'os=ubuntu-latest' -matrix 'go=1.22'
```

Full detail on every flag and `.actl.yml` key is in [Usage](#usage) below.

## Usage

> Examples use the installed `actl` binary. Working from a clone instead? Swap `actl` for
> `go run ./cmd/actl` (see [Develop](#develop)).

### TUI keys

When paused: `s`/`enter` step · `c` continue · `g` run-to-cursor · `↑↓`/`jk` move cursor ·
`b` toggle breakpoint · `e` env pane · `i` edit step command · `E` edit job env ·
`r` re-run the step in the live container · `d` drop into a shell in the container · `q` quit.
The log pane scrolls any time (paused or running): `PgUp`/`PgDn` page, `ctrl+u`/`ctrl+d`
half-page, `home`/`end` jump to top/bottom, mouse wheel. The run halts before the first
step; break-on-error halts after a failing step.

### Listing the workflow (`-list`)

`-list` inventories a workflow — its jobs, each job's steps, any matrix combinations, and
the deployment `environment:` — and exits **without** running Docker or shelling out for
identity. Use it to discover the job and step names you'll target.

```sh
actl -list testdata/workflows/pipeline.yml
```

### Selecting a job & seeding `needs`

`actl` debugs one job at a time, in isolation — the job's upstream `needs` jobs are **not**
run. Pick the job, and seed the upstream outputs/results you want it to see; the same flags
on the command line mean a re-run reproduces the exact state.

```sh
actl testdata/workflows/pipeline.yml        # lists jobs if there's more than one
actl -job deploy testdata/workflows/pipeline.yml

# seed what the upstream job would have produced (paths mirror the needs.* context):
actl -job deploy \
  -need 'build.outputs.image=ghcr.io/acme/app:1.4.2' \
  -need 'build.result=success' \
  -env  'STAGE=prod' \
  testdata/workflows/pipeline.yml
```

Unseeded outputs resolve to empty (exactly as a non-existent output does in GitHub); an
unseeded `result` defaults to `success`. The TUI prints a transparency line per need so you
see precisely what the isolated run stands on.

Prefer to exercise the real dependencies instead of seeding them? `--with-deps` runs the
upstream jobs for real to completion first, then pauses only on the target job's steps — so
`needs.*` are genuine and there's nothing to seed (upstream output streams to the log pane):

```sh
actl -job deploy --with-deps testdata/workflows/pipeline.yml
```

### Matrix

A job whose `matrix` expands to more than one combination must be pinned to exactly one —
`-list` shows the combinations, and `-matrix KEY=VALUE` (repeatable) selects it:

```sh
actl -job test \
  -matrix 'os=ubuntu-latest' -matrix 'go=1.22' \
  testdata/workflows/matrix.yml
```

The single `-image` default maps `ubuntu-latest`; to map other runner labels to images use
`-platform LABEL=IMAGE` (repeatable, act's `-P`; overrides the `images:` map in `.actl.yml`).
When a job declares `services:`, act starts those service containers and the TUI prints a
line naming them.

### Secrets, vars & env

`actl` reads act's dotenv triple from the working dir — `.secrets` → `secrets.*`,
`.vars` → `vars.*`, `.env` → env vars — so `${{ secrets.X }}`, `${{ vars.X }}` and `$X`
resolve as on GitHub. These files are **gitignored**; keep them out of commits. Override
individual keys with repeatable `-secret`/`-var`/`-env KEY=VALUE` (these win over the
files), or point at a file outside the repo with `-secret-file`/`-var-file`/`-env-file`.

```sh
printf 'TOKEN=s3cr3t\n'     > .secrets   # gitignored
printf 'REGION=eu-west-1\n' > .vars
printf 'STAGE=dev\n'        > .env
actl testdata/workflows/config.yml

# keep secrets outside the repo and override one key for this run:
actl -secret-file ~/.config/actl/demo.secrets \
  -var 'REGION=us-east-1' testdata/workflows/config.yml
```

The TUI prints a **redacted** transparency line naming what loaded — counts and names
only, never values — and act masks secret values in the step logs.

### Configuration (`.actl.yml`)

For a real workflow, stash the debug slice in a committable **`.actl.yml`** instead of a
flag soup. It's auto-discovered as `.actl.yml` in the working dir (point elsewhere with
`-config FILE`). Precedence is **CLI flag > `.actl.yml` > built-in default**, and unknown
keys are rejected so typos surface immediately.

```yaml
# .actl.yml — every key optional
workflow: .github/workflows/deploy.yml   # a path arg still wins
job: deploy
event: push
matrix:                                  # pin one combination
  os: ubuntu-latest
with-deps: false                         # true = run upstream needs for real first

images:                                  # act's -P: runner label -> docker image
  ubuntu-latest: catthehacker/ubuntu:act-latest
  ubuntu-22.04: catthehacker/ubuntu:act-22.04

breakpoints:                             # step index OR step name
  - 0
  - "Build"

# workdir: .          # bind-mount this dir (writable) so local 'uses: ./' resolve
# source: .           # tree a default actions/checkout copies from

secret-file: .secrets                    # secrets are FILE-ONLY here (see below)
vars:
  REGION: us-east-1
env:
  LOG_LEVEL: debug

inputs:                                  # workflow_dispatch / workflow_call
  version: "1.2.3"
# needs:                                 # seed upstream needs for isolated debugging
#   lint:
#     result: success
#     outputs: { sha: abc123 }
```

Because `.actl.yml` is committable, **secrets can't be inlined** — an inline `secrets:`
map (top level or under an environment) is a hard error; reference a gitignored dotenv via
`secret-file:` instead. `vars`/`env` are not sensitive and may be inlined. A copy with
inline comments lives in [`.actl.yml.sample`](./.actl.yml.sample).

#### Per-environment overlays

GitHub scopes `secrets.*`/`vars.*` by deployment `environment:`. When the debugged job
targets one, the matching block under `environments:` **overlays** the flat `secret-file`/
`vars` defaults (a CLI `-secret`/`-var` still wins). The TUI prints which overlay loaded
(counts only):

```yaml
environments:
  production:
    secret-file: .secrets.prod
    vars: { REGION: us-west-2 }
  staging:
    vars: { REGION: eu-west-1 }
```

### GitHub & runtime context

GitHub injects context a clean local runner lacks; `actl` seeds it and prints a
transparency line for each:

- **`github.token` / `secrets.GITHUB_TOKEN`** — from `-github-token`, else a `GITHUB_TOKEN`
  in `.secrets`, else ambient `gh auth token`; the two stay equal as on GitHub. It is **not**
  auto-exported as `$GITHUB_TOKEN` (faithful — map it via `env:`). Heads-up: your token's
  scope differs from CI's ephemeral, repo-scoped one.
- **Workflow inputs** — `-input NAME=VALUE` (repeatable) for `workflow_dispatch` /
  `workflow_call`; act applies the declared `default:` and boolean typing itself, so you
  only supply the values you want to override.
- **Event payload** — `-event-file PATH.json` sets `github.event.*`.
- **`github.*` context** — `-repository`/`-ref`/`-sha`/`-actor` override the respective
  fields; otherwise repository/ref/sha are derived from your local git (`origin` remote,
  HEAD). `actor`, `run_id`, and `run_number` are honest placeholders.

### Workspace

By default the job runs with an **empty workspace** (the repo is kept out of the container),
so remote `uses:` actions work but local `uses: ./…` actions and `actions/checkout` of the
working repo won't find any files — the TUI flags this when it spots local actions. That's the
common case; reach for `-workdir` only when you actually have local actions to debug.

`-workdir DIR` **bind-mounts** `DIR` as the workspace so local actions resolve. Note the
tradeoff: a mounted workspace is **writable**, so steps running in the container can change
your working tree (build artifacts, generated files). The TUI shows a transparency line when
a workspace is mounted.

```sh
actl -workdir . path/to/workflow.yml
```

### Checkout

A default `actions/checkout` (no `ref`/`repository`/`path`) would clone a remote over the
workspace — losing your local changes. `actl` **intercepts** it: it copies your working tree
(current dir, or `-source DIR`) into the workspace at the checkout step's position, honouring
`.gitignore` and without mounting (no host writes). Steps before checkout still see an empty
workspace, exactly as on GitHub; steps after see your code, including uncommitted changes.
Git submodules follow the step's `submodules:` input (off by default, as on GitHub; `true`/
`recursive` copies them in). A checkout pinned to another repo/ref/path is left as a real clone.

```sh
actl testdata/workflows/checkout.yml
```

### Cloud identity (GCP & AWS)

In real CI, a login action (`google-github-actions/auth`, `aws-actions/configure-aws-credentials`)
plus `id-token: write` mints a GitHub-signed OIDC token and exchanges it for short-lived
cloud credentials. **Locally there is no GitHub OIDC issuer**, so that step can't federate —
it would fail and kill the job. `actl` **intercepts** it: rewrites it to a no-op and injects
your *ambient* credentials, so later steps run as **you**. The TUI prints a transparency line
— what the step *would* federate as vs the local identity it runs as — because you're testing
under your own permissions, not the workflow's federated scope.

**GCP** — `actl` injects your gcloud Application Default Credentials so later steps
(`gcloud`/`gsutil`, `setup-gcloud`, client libraries, terraform) run as you:

```sh
gcloud auth application-default login        # once, so the ADC file exists
actl testdata/workflows/gcp-auth.yml
```

It mounts your ADC file read-only into the container and sets
`GOOGLE_APPLICATION_CREDENTIALS` (so client libraries and terraform discover it) plus
`CLOUDSDK_AUTH_ACCESS_TOKEN` (so gcloud/gsutil and `setup-gcloud` authenticate). Point
at a specific credential file with `-gcp-credentials FILE`, or leave the auth step
untouched (real federation) with `-gcp-identity=false`. The access token lives ~1h; the
credential file keeps client libraries working past that. `actl` doesn't inject a
**project** — that's the workflow's concern, exactly as on GitHub (the command's
`--project`, or a `GOOGLE_CLOUD_PROJECT` you pass via `.env` / `-env`). A `gcloud` call
with no project resolved fails the same way it would in CI (*"Project id: 0 is invalid"*).

**AWS** — same shape: `actl` injects your ambient AWS credentials (env-only, no file
mount), resolved from the default profile / environment or `-aws-profile NAME`; the region
is injected when known. Disable with `-aws-identity=false` to leave the step untouched.

```sh
aws sso login        # or any way your ambient credentials are established
actl -aws-profile dev testdata/workflows/aws-auth.yml
```

GCP and AWS are done; **Azure** is the remaining provider on the roadmap.

## How it works

`act` already runs Actions workflows locally — but as a batch runner: no breakpoints, no
pause-before-step, no drop-into-shell. `actl` adds exactly that debug layer, then stays out
of the way of act's engine:

- **Reuse, don't rebuild.** `act/pkg/model` parses workflows; `act/pkg/exprparser`
  evaluates `${{ }}`. Imported as-is.
- **Soft fork for the pause hook.** act's per-step machinery is unexported, so a tiny patch
  interleaves a *barrier* `common.Executor` between steps and exposes a resume channel on
  `runner.Config`. The fork lives in [`third_party/act`](./third_party/act), wired in via a
  `replace` directive in `go.mod`, pinned to a release. We keep the diff tiny and aim to
  upstream the hook.
- **Frontend-agnostic core.** The debug engine (`internal/debugger`) owns no terminal and
  imports no frontend; the TUI is one consumer, with headless/agent and DAP front-ends as
  future peers behind the same API.

### Layout

```
cmd/actl/           TUI entry point
internal/debugger/  the pause-barrier core: Session, pause/step/continue, log capture
internal/tui/       Bubble Tea front-end over the core
internal/config/    loads .actl.yml (the debug slice: job/matrix/breakpoints/images/…)
internal/workflow/  thin wrapper over act/pkg/model
third_party/act/    soft fork of act — git submodule → ruzmuh/act (branch actl), pinned by SHA
testdata/workflows/ sample workflows
```

## Develop

Requires Go (the module pins the toolchain to match act; `go` auto-fetches it) and **Docker**
(act starts a real job container and execs each step into it). The act fork is a git
submodule, so clone with `--recurse-submodules` (or run `git submodule update --init`):

```sh
git clone --recurse-submodules https://github.com/ruzmuh/actl
cd actl
go run ./cmd/actl                          # debug the sample workflow in the TUI
go run ./cmd/actl path/to/workflow.yml     # your own workflow
go test ./...                              # run the tests (no Docker needed)
```

## License

MIT, like `act`.
