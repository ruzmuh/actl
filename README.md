# actl

A **TUI-first, interactive step-debugger for GitHub Actions workflows** that runs them
locally — pause before any step, inspect the environment, drop into the job container,
re-run a step — with **faithful `uses:` execution**, because it stands on
[`nektos/act`](https://github.com/nektos/act) instead of reimplementing the Actions engine.

> Status: pre-v0.1, under active scaffolding. Go. MIT. FOSS, no monetization.
> See [CLAUDE.md](./CLAUDE.md) for the full design.

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
cmd/actl/           entry point (currently the Spike-1 library probe)
internal/workflow/  thin wrapper over act/pkg/model
internal/expr/      thin wrapper over act/pkg/exprparser
internal/debugger/  the pause-barrier core            (coming)
internal/tui/       Bubble Tea front-end               (coming)
third_party/act/    soft fork of act — git submodule → ruzmuh/act (branch actl), pinned by SHA
testdata/workflows/ sample workflows
```

## Develop

Requires Go (the module pins the toolchain to match act; `go` auto-fetches it). Docker is
needed once real execution lands.

The act fork lives in a submodule, so clone with `--recurse-submodules` (or run
`git submodule update --init` afterwards):

```sh
git clone --recurse-submodules https://github.com/ruzmuh/actl
go run ./cmd/actl                              # parse the sample workflow + eval expressions
go run ./cmd/actl path/to/your/workflow.yml    # try your own
```

## Roadmap

See the *First tasks* and *Scope — v0.1* sections of [CLAUDE.md](./CLAUDE.md). In short:
library spike (done) → fork + pause barrier → minimal TUI → `uses:` verification →
ambient identity substitution → upstream the hook.

## License

MIT, like `act`.
