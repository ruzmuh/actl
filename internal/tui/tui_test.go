package tui

import (
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ruzmuh/actl/internal/debugger"
)

// TestNeedsLines locks the isolated-run transparency text: seeded outputs are
// listed sorted, an assumed result is marked, and no outputs reads as empty.
func TestNeedsLines(t *testing.T) {
	lines := needsLines([]debugger.NeedsSummary{
		{Job: "build", Result: "success", Assumed: true, Outputs: map[string]string{"image": "repo/app:abc", "version": "1.4.2"}},
		{Job: "test", Result: "failure", Assumed: false},
	})
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], `needs "build"`) ||
		!strings.Contains(lines[0], "result=success (assumed)") ||
		!strings.Contains(lines[0], "image=repo/app:abc, version=1.4.2") {
		t.Errorf("build line wrong: %q", lines[0])
	}
	if !strings.Contains(lines[1], "result=failure") || strings.Contains(lines[1], "assumed") ||
		!strings.Contains(lines[1], "none") {
		t.Errorf("test line wrong: %q", lines[1])
	}

	// --with-deps: the upstream job runs for real, so the line says "live"
	live := needsLines([]debugger.NeedsSummary{{Job: "build", Live: true}})
	if len(live) != 1 || !strings.Contains(live[0], `needs "build"`) || !strings.Contains(live[0], "live") {
		t.Errorf("live line wrong: %v", live)
	}
}

// TestModelFlow drives the model through synthetic core messages and checks the
// rendered status — no Docker, no TTY. It guards the pause/step/continue wiring
// and that View never panics.
func TestModelFlow(t *testing.T) {
	sess, err := debugger.New(debugger.Options{
		WorkflowPath: "../../testdata/workflows/sample.yml",
		Workdir:      t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	var m tea.Model = New(sess, func() {})

	// initial render: running
	if got := m.View(); !strings.Contains(got, "running") {
		t.Errorf("initial view missing 'running':\n%s", got)
	}

	// a pause before step 1
	m, _ = m.Update(pauseMsg(debugger.PauseEvent{When: debugger.Before, Index: 0}))
	if got := m.View(); !strings.Contains(got, "paused") || !strings.Contains(got, "before step 1") {
		t.Errorf("paused view wrong:\n%s", got)
	}

	// run-to-cursor while the cursor sits on the paused step is a no-op guard
	// (cursor==cur, Before) — it must not resume the run (no act goroutine here).
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	if got := m.View(); !strings.Contains(got, "already stopped before this step") {
		t.Errorf("run-to-cursor guard not hit:\n%s", got)
	}

	// some output
	m, _ = m.Update(logMsg("hello from a step"))
	if got := m.View(); !strings.Contains(got, "hello from a step") {
		t.Errorf("log line not rendered:\n%s", got)
	}

	// move the cursor and arm a breakpoint; the dot should render
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
	if got := m.View(); !strings.Contains(got, "●") {
		t.Errorf("breakpoint dot not rendered after toggle:\n%s", got)
	}

	// toggle the env pane (no live run, so it shows the paused-only hint)
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if got := m.View(); !strings.Contains(got, "job env at step 1") {
		t.Errorf("env pane not shown after toggle:\n%s", got)
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")}) // toggle back

	// env edit parsing (SetEnv is a no-op while not paused, but parsing must work)
	if mm, ok := m.(Model); ok {
		mm.applyEdit(editDoneMsg{kind: editEnv, content: "FOO=bar\n# a comment\nBAZ=qux\n"})
		found := false
		for _, l := range mm.logs {
			if strings.Contains(l, "applied 2 env var(s)") {
				found = true
			}
		}
		if !found {
			t.Errorf("env edit not parsed into 2 vars: %v", mm.logs)
		}
	}

	// completion
	m, _ = m.Update(doneMsg{})
	if got := m.View(); !strings.Contains(got, "run complete") {
		t.Errorf("done view missing 'run complete':\n%s", got)
	}
}

// TestLogScroll drives the scrollable LOGS pane: it follows the tail while at
// the bottom, PgUp parks it on earlier output (no longer at bottom), and End
// returns it to the tail.
func TestLogScroll(t *testing.T) {
	sess, err := debugger.New(debugger.Options{
		WorkflowPath: "../../testdata/workflows/sample.yml",
		Workdir:      t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	var m tea.Model = New(sess, func() {})
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	// More lines than the pane is tall, so the buffer overflows the viewport.
	for i := 0; i < 200; i++ {
		m, _ = m.Update(logMsg(lineLabel(i)))
	}

	// Following the tail: the newest line shows, the oldest does not.
	if got := m.View(); !strings.Contains(got, lineLabel(199)) {
		t.Errorf("tail not followed — newest line missing:\n%s", got)
	}
	if got := m.View(); strings.Contains(got, lineLabel(0)) {
		t.Errorf("viewport should not show the oldest line while tailing:\n%s", got)
	}

	// PgUp scrolls back: now off the bottom and the newest line is out of view.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	if mm := m.(Model); mm.logVP.AtBottom() {
		t.Error("PgUp should leave the viewport off the bottom")
	}
	if got := m.View(); strings.Contains(got, lineLabel(199)) {
		t.Errorf("after PgUp the newest line should be scrolled out:\n%s", got)
	}
	if got := m.View(); !strings.Contains(got, "End to follow") {
		t.Errorf("scrolled-up hint missing:\n%s", got)
	}

	// End returns to the tail.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnd})
	if mm := m.(Model); !mm.logVP.AtBottom() {
		t.Error("End should return the viewport to the bottom")
	}
	if got := m.View(); !strings.Contains(got, lineLabel(199)) {
		t.Errorf("End did not return to the tail:\n%s", got)
	}
}

func lineLabel(i int) string { return "log-line-" + strconv.Itoa(i) }

// TestConfigLine locks the redacted config banner: counts and names appear,
// values never do.
func TestConfigLine(t *testing.T) {
	got := configLine(debugger.ConfigSummary{
		Secrets: []string{"TOKEN"},
		Vars:    []string{"REGION", "STAGE"},
	})
	for _, want := range []string{"1 secret(s): TOKEN", "2 var(s): REGION, STAGE", "values withheld"} {
		if !strings.Contains(got, want) {
			t.Errorf("configLine missing %q:\n%s", want, got)
		}
	}
	if configLine(debugger.ConfigSummary{}) != "" {
		t.Error("empty summary should render no line")
	}
}

// TestRuntimeBannerRender renders the real banner end-to-end (New → noticeLines →
// View) for a workflow_dispatch run with all four §4 runtime-context surfaces
// populated, and confirms each transparency line reaches the rendered View. It also
// logs the banner so a -v run shows what the TUI prints (no Docker/TTY needed — the
// notices render before the container ever starts).
func TestRuntimeBannerRender(t *testing.T) {
	sess, err := debugger.New(debugger.Options{
		WorkflowPath: "../../testdata/workflows/inputs.yml",
		EventName:    "workflow_dispatch",
		Workdir:      t.TempDir(),
		Secrets:      map[string]string{"GITHUB_TOKEN": "ghp_demo"},
		Inputs:       map[string]string{"environment": "production"},
		Repository:      "ruzmuh/actl",
		Ref:             "refs/heads/feature",
		Sha:             "deadbeefcafebabe",
		Actor:           "ruzmuh",
		GitHubOverrides: []string{"repository", "ref", "sha", "actor"},
	})
	if err != nil {
		t.Fatal(err)
	}

	var m tea.Model = New(sess, func() {})
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	got := m.View()
	t.Logf("\n%s", got)

	for _, want := range []string{
		"github.token set (from secret)",
		"provided environment=production",
		"github context: repository=ruzmuh/actl",
		"ref=refs/heads/feature",
		"sha=deadbee",
		"actor=ruzmuh (override)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered banner missing %q", want)
		}
	}
}

// TestRuntimeContextLines locks the §4 transparency banner for the GitHub runtime
// context a clean local runner lacks: token, inputs, event, and github.* context.
func TestRuntimeContextLines(t *testing.T) {
	// token: present names the source + the honest scope note; absent warns
	if got := tokenLine(debugger.TokenSummary{Present: true, Source: "secret"}); !strings.Contains(got, "from secret") || !strings.Contains(got, "broader scope") {
		t.Errorf("token present line wrong: %q", got)
	}
	if got := tokenLine(debugger.TokenSummary{}); !strings.Contains(got, "none set") || !strings.Contains(got, "-github-token") {
		t.Errorf("token absent line wrong: %q", got)
	}

	// inputs: provided values shown, defaulted names counted; empty when undeclared
	in := inputsLine(debugger.InputsSummary{Declared: true, Provided: map[string]string{"env": "prod"}, Defaults: []string{"region", "tier"}})
	if !strings.Contains(in, "provided env=prod") || !strings.Contains(in, "2 using declared default(s): region, tier") {
		t.Errorf("inputs line wrong: %q", in)
	}
	if inputsLine(debugger.InputsSummary{}) != "" {
		t.Error("no declared/provided inputs should render no line")
	}

	// event: only shown for an explicit file
	if got := eventLine(debugger.EventSummary{EventName: "pull_request", Path: "/tmp/e.json"}); !strings.Contains(got, "/tmp/e.json") || !strings.Contains(got, "pull_request") {
		t.Errorf("event line wrong: %q", got)
	}
	if eventLine(debugger.EventSummary{Synthetic: true}) != "" {
		t.Error("synthetic event should render no line")
	}

	// github context: resolved values, override marker, placeholders for the unknowable
	ghc := ghcLine(debugger.GitHubContextSummary{Repository: "o/r", Ref: "refs/heads/main", Sha: "0123456", Actor: "alice", Overridden: []string{"actor"}})
	for _, want := range []string{"repository=o/r", "ref=refs/heads/main", "sha=0123456", "actor=alice (override)", "run_id/number=1 (placeholder)"} {
		if !strings.Contains(ghc, want) {
			t.Errorf("ghc line missing %q: %q", want, ghc)
		}
	}
	// Only actor is overridden — the non-overridden fields must NOT carry the marker.
	if strings.Contains(ghc, "o/r (override)") || strings.Contains(ghc, "refs/heads/main (override)") {
		t.Errorf("non-overridden field marked as override: %q", ghc)
	}
	// empty fields fall back to act-derived / placeholder
	if got := ghcLine(debugger.GitHubContextSummary{}); !strings.Contains(got, "repository=act-derived") || !strings.Contains(got, "actor=nektos/act (placeholder)") {
		t.Errorf("empty ghc line wrong: %q", got)
	}
}

// TestServicesLines locks the services transparency: one line naming the containers
// act will start, and nothing when the job declares no services.
func TestServicesLines(t *testing.T) {
	got := servicesLine(debugger.ServicesSummary{Names: []string{"postgres", "redis"}})
	if !strings.Contains(got, "2 service container(s)") || !strings.Contains(got, "postgres, redis") {
		t.Errorf("services line wrong: %q", got)
	}
	if got := servicesLine(debugger.ServicesSummary{}); got != "" {
		t.Errorf("no services should render nothing, got %q", got)
	}
}

// TestGCPLines locks the identity transparency: the federation target vs the local
// identity, what was injected, and the honest no-credentials case.
func TestGCPLines(t *testing.T) {
	// full substitution: a federation line + the mounted-file/token line
	full := gcpLines(debugger.GCPSummary{
		Steps:   []string{"auth"},
		Targets: []string{"sa@p.iam.gserviceaccount.com via prov"},
		Account: "me@example.com",
		File:    true,
		Token:   true,
	})
	if len(full) != 3 {
		t.Fatalf("want 3 lines, got %d: %v", len(full), full)
	}
	if !strings.Contains(full[0], "would federate as sa@p.iam.gserviceaccount.com via prov") ||
		!strings.Contains(full[0], "running locally as me@example.com") {
		t.Errorf("federation line wrong: %q", full[0])
	}
	if !strings.Contains(full[1], "ADC file + access token") {
		t.Errorf("injection line wrong: %q", full[1])
	}
	if !strings.Contains(full[2], "GOOGLE_CLOUD_PROJECT") {
		t.Errorf("project-hint line wrong: %q", full[2])
	}

	// no credentials: still names the step, but says cloud calls will fail
	none := gcpLines(debugger.GCPSummary{Steps: []string{"auth"}, Targets: []string{"sa via prov"}})
	if len(none) != 2 || !strings.Contains(none[1], "no ambient credentials") {
		t.Errorf("no-creds lines wrong: %v", none)
	}
	if !strings.Contains(none[0], "your ambient gcloud identity") {
		t.Errorf("missing-account fallback wrong: %q", none[0])
	}

	// no auth step: nothing rendered
	if got := gcpLines(debugger.GCPSummary{}); got != nil {
		t.Errorf("no auth step should render nothing, got %v", got)
	}
}
