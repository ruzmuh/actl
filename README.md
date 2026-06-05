# actl

A **TUI-first, interactive step-debugger for GitHub Actions workflows** that runs them
locally — pause before any step, inspect the environment, drop into the job container,
re-run a step — with **faithful `uses:` execution**, because it stands on
[`nektos/act`](https://github.com/nektos/act) instead of reimplementing the Actions engine.

> Status: pre-v0.1, under active development. Go. MIT. FOSS, no monetization.
> See [CLAUDE.md](./CLAUDE.md) for the full design.
>
> Working today: single-job debugging through act's real engine — pause before/after
> every step, env inspector, drop into the live job container, edit a step's command or
> env and re-run it in place, breakpoints + run-to-cursor, job selection, and isolated-run
> `needs` seeding with a transparency line.

## Why

`act` runs Actions workflows locally and faithfully — but as a batch runner: it has no
breakpoints, no pause-before-step, no drop-into-shell. `actl` adds that debug layer on top
of `act`, the way `lazygit` sits on `git`. Nobody else combines interactive step-debugging
with faithful `uses:` execution (docker / composite / node actions).

## Architecture (short version)

- **Reuse, don't rebuild.** `act/pkg/model` parses workflows; `act/pkg/exprparser`
  evaluates `${{ }}`. Imported as-is.
- **Soft fork for the pause hook.** act's per-step machinery is unexported, so a tiny patch
  interleaves a *barrier* `common.Executor` between steps and exposes a resume channel on
  `runner.Config`. The fork lives in [`third_party/act`](./third_party/act), wired in via a
  `replace` directive in `go.mod`, pinned to a release. We keep the diff tiny and aim to
  upstream the hook.

## Layout

```
cmd/actl/           TUI entry point
cmd/spike-barrier/  line-based driver over the core (dev/debug aid)
internal/debugger/  the pause-barrier core: Session, pause/step/continue, log capture
internal/tui/       Bubble Tea front-end over the core
internal/workflow/  thin wrapper over act/pkg/model
internal/expr/      thin wrapper over act/pkg/exprparser
third_party/act/    soft fork of act — git submodule → ruzmuh/act (branch actl), pinned by SHA
testdata/workflows/ sample workflows
```

## Develop

Requires Go (the module pins the toolchain to match act; `go` auto-fetches it) and **Docker**
(act starts a real job container and execs each step into it).

The act fork lives in a submodule, so clone with `--recurse-submodules` (or run
`git submodule update --init` afterwards):

```sh
git clone --recurse-submodules https://github.com/ruzmuh/actl
go run ./cmd/actl                          # debug the sample workflow in the TUI
go run ./cmd/actl path/to/workflow.yml     # your own workflow
go run ./cmd/actl -image node:20-bullseye-slim   # smaller image for quick run-only workflows
```

### TUI keys

When paused: `s`/`enter` step · `c` continue · `g` run-to-cursor · `↑↓`/`jk` move cursor ·
`b` toggle breakpoint · `e` env pane · `i` edit step command · `E` edit job env ·
`r` re-run the step in the live container · `d` drop into a shell in the container · `q` quit.
The run halts before the first step; break-on-error halts after a failing step.

### Selecting a job & seeding `needs`

`actl` debugs one job at a time, in isolation — the job's upstream `needs` jobs are **not**
run. Pick the job, and seed the upstream outputs/results you want it to see; the same flags
on the command line mean a re-run reproduces the exact state.

```sh
go run ./cmd/actl testdata/workflows/pipeline.yml        # lists jobs if there's more than one
go run ./cmd/actl -job deploy testdata/workflows/pipeline.yml

# seed what the upstream job would have produced (paths mirror the needs.* context):
go run ./cmd/actl -job deploy \
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
go run ./cmd/actl -job deploy --with-deps testdata/workflows/pipeline.yml
```

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
go run ./cmd/actl -workdir . path/to/workflow.yml
```

### Checkout

A default `actions/checkout` (no `ref`/`repository`/`path`) would clone a remote over the
workspace — losing your local changes. `actl` **intercepts** it: it copies your working tree
(current dir, or `-source DIR`) into the workspace at the checkout step's position, honouring
`.gitignore` and without mounting (no host writes). Steps before checkout still see an empty
workspace, exactly as on GitHub; steps after see your code, including uncommitted changes. A
checkout pinned to another repo/ref/path is left as a real clone.

```sh
go run ./cmd/actl testdata/workflows/checkout.yml
```

`go test ./...` runs the tests (no Docker needed).

## Roadmap

See the *First tasks* and *Scope — v0.1* sections of [CLAUDE.md](./CLAUDE.md). Done so far:
library spike ✓ → fork + pause barrier ✓ → frontend-agnostic core ✓ → TUI (step/inspect/shell/
edit/re-run/breakpoints/run-to-cursor) ✓ → job selection + isolated `needs` seeding ✓ →
run-dependencies-then-debug (`--with-deps`) ✓ → remote `uses:` (node / docker / composite) ✓ →
workspace mount for local actions (`-workdir`) ✓ → faithful `actions/checkout` (copies your
local working tree) ✓.
Next: ambient identity substitution → full multi-job graph → upstream the hook(s).

## License

MIT, like `act`.
