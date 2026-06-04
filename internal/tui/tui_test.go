package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ruzmuh/actl/internal/debugger"
)

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

	// some output
	m, _ = m.Update(logMsg("hello from a step"))
	if got := m.View(); !strings.Contains(got, "hello from a step") {
		t.Errorf("log line not rendered:\n%s", got)
	}

	// completion
	m, _ = m.Update(doneMsg{})
	if got := m.View(); !strings.Contains(got, "run complete") {
		t.Errorf("done view missing 'run complete':\n%s", got)
	}
}
