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
