package tui

import (
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
