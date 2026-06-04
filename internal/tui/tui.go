// Package tui is the Bubble Tea front-end for actl. It is one consumer of the
// debugger core (internal/debugger) and holds no debug logic of its own: it
// renders pause state + logs and translates keypresses into core commands
// (Step/Continue/Abort). Per CLAUDE.md §5 the dependency arrow points one way —
// tui imports debugger, never the reverse.
package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ruzmuh/actl/internal/debugger"
)

type runState int

const (
	stateRunning runState = iota
	statePaused
	stateFinished
)

// messages bridging the core's channels into the Bubble Tea event loop.
type (
	pauseMsg     debugger.PauseEvent
	logMsg       string
	logsDoneMsg  struct{}
	doneMsg      struct{ err error }
	shellDoneMsg struct{ err error }
	rerunDoneMsg struct{ err error }
	editDoneMsg  struct {
		kind    editKind
		content string
		err     error
	}
)

type editKind int

const (
	editRun editKind = iota // the step's run: script
	editEnv                 // the job env (KEY=VALUE lines)
)

// Model is the actl TUI state.
type Model struct {
	sess   *debugger.Session
	cancel context.CancelFunc

	title  string
	labels []string // step labels, in order
	needs  []string // transparency lines for the job's needs (isolated-run notices)

	state  runState
	cur    int // index of the step at the current/last pause (-1 before first)
	curAt  debugger.When
	curErr error

	logs    []string
	runErr  error
	showEnv bool // env pane instead of log pane

	cursor      int          // selected step, for arming breakpoints
	breakpoints map[int]bool // mirror of the core's breakpoints, for rendering
	tempBreak   int          // one-shot breakpoint armed by run-to-cursor (-1 = none)

	width, height int
}

// New builds the model. cancel stops the underlying run when the user quits.
func New(sess *debugger.Session, cancel context.CancelFunc) Model {
	labels := make([]string, 0, len(sess.Steps()))
	for _, st := range sess.Steps() {
		labels = append(labels, st.String())
	}
	return Model{
		sess:        sess,
		cancel:      cancel,
		title:       fmt.Sprintf("job %q", sess.JobID()),
		labels:      labels,
		needs:       needsLines(sess.NeedsSummary()),
		state:       stateRunning,
		cur:         -1,
		breakpoints: make(map[int]bool),
		tempBreak:   -1,
	}
}

// needsLines renders one transparency line per need: which upstream job is being
// faked locally, the assumed/seeded result, and any seeded outputs. This honest
// notice is itself a feature — you see exactly what the isolated run stands on.
func needsLines(summaries []debugger.NeedsSummary) []string {
	lines := make([]string, 0, len(summaries))
	for _, n := range summaries {
		if n.Live {
			lines = append(lines, fmt.Sprintf("needs %q runs live before this job (real outputs)", n.Job))
			continue
		}
		result := n.Result
		if n.Assumed {
			result += " (assumed)"
		}
		outs := "outputs: (none — unseeded resolve empty)"
		if len(n.Outputs) > 0 {
			keys := make([]string, 0, len(n.Outputs))
			for k := range n.Outputs {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			pairs := make([]string, 0, len(keys))
			for _, k := range keys {
				pairs = append(pairs, k+"="+n.Outputs[k])
			}
			outs = "outputs: " + strings.Join(pairs, ", ")
		}
		lines = append(lines, fmt.Sprintf("needs %q isolated → result=%s · %s", n.Job, result, outs))
	}
	return lines
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.waitPause, m.waitLog, m.waitDone)
}

// --- commands (read the core's channels, return a Msg) ---

func (m Model) waitPause() tea.Msg {
	select {
	case ev := <-m.sess.Pauses():
		return pauseMsg(ev)
	case <-m.sess.Done():
		return nil // run ended; no more pauses
	}
}

func (m Model) waitLog() tea.Msg {
	// Prefer draining a buffered log line over observing completion, so the tail
	// of the output is never dropped when the run finishes.
	select {
	case line := <-m.sess.Logs():
		return logMsg(line)
	default:
	}
	select {
	case line := <-m.sess.Logs():
		return logMsg(line)
	case <-m.sess.Done():
		return logsDoneMsg{}
	}
}

func (m Model) waitDone() tea.Msg {
	<-m.sess.Done()
	return doneMsg{err: m.sess.Err()}
}

// --- update ---

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.cancel()
			return m, tea.Quit
		case "s", "enter":
			if m.state == statePaused {
				m.sess.Step()
				m.state = stateRunning
				return m, m.waitPause
			}
		case "c":
			if m.state == statePaused {
				m.sess.Continue()
				m.state = stateRunning
				return m, m.waitPause
			}
		case "g":
			if m.state == statePaused {
				return m.runToCursor()
			}
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
			return m, nil
		case "down", "j":
			if m.cursor < len(m.labels)-1 {
				m.cursor++
			}
			return m, nil
		case "b":
			on := !m.breakpoints[m.cursor]
			if on {
				m.breakpoints[m.cursor] = true
			} else {
				delete(m.breakpoints, m.cursor)
			}
			m.sess.SetBreakpoint(m.cursor, on) // safe any time; consulted in Continue mode
			return m, nil
		case "e":
			m.showEnv = !m.showEnv
			return m, nil
		case "i":
			if m.state == statePaused {
				if m.sess.CurrentRun() == "" {
					m.logs = append(m.logs, "edit: only run: steps have a command to edit")
					return m, nil
				}
				return m, m.editCmd(editRun, m.sess.CurrentRun())
			}
		case "E":
			if m.state == statePaused {
				return m, m.editCmd(editEnv, m.envText())
			}
		case "r":
			if m.state == statePaused {
				if !m.sess.CanRerun() {
					m.logs = append(m.logs, "rerun: available only after a step has run (step onto it first)")
					return m, nil
				}
				m.logs = append(m.logs, fmt.Sprintf("re-running step %d…", m.cur+1))
				return m, m.rerunCmd()
			}
		case "d":
			if m.state == statePaused {
				if name := m.sess.ContainerName(); name != "" {
					// Hand the terminal to an interactive shell in the live
					// container, then resume the TUI. The core stays out of the
					// terminal; this is the frontend's job (CLAUDE.md §5).
					return m, tea.ExecProcess(m.shellCmd(name), func(err error) tea.Msg { return shellDoneMsg{err} })
				}
			}
		}
		return m, nil

	case pauseMsg:
		// A run-to-cursor temp breakpoint is one-shot: any pause consumes it.
		// Restore the core to the user's own breakpoint state at that index.
		if m.tempBreak >= 0 {
			m.sess.SetBreakpoint(m.tempBreak, m.breakpoints[m.tempBreak])
			m.tempBreak = -1
		}
		m.state = statePaused
		m.cur = msg.Index
		m.curAt = msg.When
		m.curErr = msg.Err
		m.cursor = msg.Index // focus the step we stopped at; edit/env/rerun target it
		return m, nil

	case logMsg:
		m.logs = append(m.logs, string(msg))
		if len(m.logs) > 2000 {
			m.logs = m.logs[len(m.logs)-2000:]
		}
		return m, m.waitLog

	case logsDoneMsg:
		return m, nil

	case shellDoneMsg:
		if msg.err != nil {
			m.logs = append(m.logs, "shell: "+msg.err.Error())
		}
		return m, nil

	case rerunDoneMsg:
		if msg.err != nil {
			m.logs = append(m.logs, "rerun: "+msg.err.Error())
		}
		return m, nil

	case editDoneMsg:
		m.applyEdit(msg)
		return m, nil

	case doneMsg:
		m.state = stateFinished
		m.runErr = msg.err
		return m, nil
	}
	return m, nil
}

// --- view ---

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("13"))
	paneStyle   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	curStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	okStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	keyStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	bpStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	statusStyle = lipgloss.NewStyle().Bold(true)
)

func (m Model) View() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("actl · "+m.title) + "\n")
	for _, line := range m.needs {
		b.WriteString(dimStyle.Render("⚠ "+line) + "\n")
	}
	b.WriteString("\n")

	// step list: [breakpoint] [cursor] N. label (state)
	var steps strings.Builder
	for i, label := range m.labels {
		bp := "  "
		if m.breakpoints[i] {
			bp = bpStyle.Render(" ●")
		}
		caret := "  "
		if i == m.cursor {
			caret = "❯ "
		}
		text := fmt.Sprintf("%d. %s", i+1, label)
		switch {
		case i == m.cur && m.state == statePaused:
			text = curStyle.Render(text) + dimStyle.Render("  ("+m.curAt.String()+")")
		case i == m.cur && m.state == stateRunning:
			text = text + dimStyle.Render("  (running)")
		case i < m.cur || (i == m.cur && m.state == stateFinished):
			text = dimStyle.Render(text)
		}
		steps.WriteString(bp + caret + text + "\n")
	}
	b.WriteString(paneStyle.Width(m.paneWidth()).Render("STEPS\n"+strings.TrimRight(steps.String(), "\n")) + "\n")

	// bottom pane: env (while inspecting) or log tail
	if m.showEnv {
		title := "ENV · job-scoped"
		if m.cur >= 0 {
			title = fmt.Sprintf("ENV · job env at step %d %q", m.cur+1, m.stepLabel(m.cur))
		}
		b.WriteString(paneStyle.Width(m.paneWidth()).Render(title+"\n"+m.envPane()) + "\n")
	} else {
		b.WriteString(paneStyle.Width(m.paneWidth()).Render("LOGS\n"+m.logTail()) + "\n")
	}

	b.WriteString(m.statusLine())
	return b.String()
}

// shellCmd builds a `docker exec -it` into the live container, injecting the
// job env we captured so the shell matches the ENV pane (act passes step env
// per-exec, so a plain exec would otherwise miss it — e.g. GREETING).
func (m Model) shellCmd(name string) *exec.Cmd {
	args := []string{"exec", "-it"}
	env := m.sess.Env()
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-e", k+"="+env[k])
	}
	args = append(args, name, "sh", "-c", "[ -x /bin/bash ] && exec /bin/bash || exec /bin/sh")
	return exec.Command("docker", args...)
}

// runToCursor resumes and halts before the step under the cursor, using a
// one-shot breakpoint cleared on the next pause (gdb's `advance`). It only runs
// forward: the cursor must be ahead of the current execution point, since act
// can't replay a before-barrier it has already passed (use reload to restart).
func (m Model) runToCursor() (tea.Model, tea.Cmd) {
	target := m.cursor
	switch {
	case target == m.cur && m.curAt == debugger.Before:
		m.logs = append(m.logs, "run-to-cursor: already stopped before this step")
		return m, nil
	case target <= m.cur:
		m.logs = append(m.logs, "run-to-cursor: target is behind the current step — can't run backward")
		return m, nil
	}
	// Arm a temp breakpoint unless the user already has a real one there (which
	// we must not clobber when the pause clears it).
	if !m.breakpoints[target] {
		m.tempBreak = target
		m.sess.SetBreakpoint(target, true)
	}
	m.logs = append(m.logs, fmt.Sprintf("running to step %d: %s", target+1, m.stepLabel(target)))
	m.sess.Continue()
	m.state = stateRunning
	return m, m.waitPause
}

// rerunCmd re-executes the paused step in the live container (picking up edits).
func (m Model) rerunCmd() tea.Cmd {
	return func() tea.Msg { return rerunDoneMsg{err: m.sess.Rerun()} }
}

// editCmd hands the terminal to $EDITOR on a temp file seeded with initial, then
// returns the edited content. Like the shell, editing is the frontend's job —
// the core only applies the result (CLAUDE.md §5).
func (m Model) editCmd(kind editKind, initial string) tea.Cmd {
	ext := ".sh"
	if kind == editEnv {
		ext = ".env"
	}
	f, err := os.CreateTemp("", "actl-edit-*"+ext)
	if err != nil {
		return func() tea.Msg { return editDoneMsg{kind: kind, err: err} }
	}
	_, _ = f.WriteString(initial)
	_ = f.Close()

	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	c := exec.Command(editor, f.Name()) //nolint:gosec // editor is the user's own $EDITOR
	return tea.ExecProcess(c, func(runErr error) tea.Msg {
		defer os.Remove(f.Name())
		if runErr != nil {
			return editDoneMsg{kind: kind, err: runErr}
		}
		b, rerr := os.ReadFile(f.Name())
		return editDoneMsg{kind: kind, content: string(b), err: rerr}
	})
}

func (m *Model) applyEdit(msg editDoneMsg) {
	if msg.err != nil {
		m.logs = append(m.logs, "edit: "+msg.err.Error())
		return
	}
	switch msg.kind {
	case editRun:
		m.sess.SetRun(strings.TrimRight(msg.content, "\n"))
		m.logs = append(m.logs, "edited step command (in memory) — press r to rerun")
	case editEnv:
		n := 0
		for _, line := range strings.Split(msg.content, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if k, v, ok := strings.Cut(line, "="); ok {
				m.sess.SetEnv(strings.TrimSpace(k), v)
				n++
			}
		}
		m.logs = append(m.logs, fmt.Sprintf("applied %d env var(s) (in memory) — press r to rerun", n))
	}
}

func (m Model) stepLabel(i int) string {
	if i >= 0 && i < len(m.labels) {
		return m.labels[i]
	}
	return ""
}

// envText renders the current job env as editable KEY=VALUE lines.
func (m Model) envText() string {
	env := m.sess.Env()
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# edit job env — KEY=VALUE per line; '#' comments ignored\n")
	for _, k := range keys {
		b.WriteString(k + "=" + env[k] + "\n")
	}
	return b.String()
}

func (m Model) envPane() string {
	env := m.sess.Env()
	if len(env) == 0 {
		return dimStyle.Render("(environment is available only while paused)")
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	limit := m.logHeight()
	var b strings.Builder
	for i, k := range keys {
		if i >= limit {
			b.WriteString(dimStyle.Render(fmt.Sprintf("… %d more", len(keys)-limit)))
			break
		}
		b.WriteString(keyStyle.Render(k) + dimStyle.Render("=") + env[k] + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m Model) paneWidth() int {
	if m.width > 4 {
		return m.width - 4
	}
	return 76
}

func (m Model) logTail() string {
	n := m.logHeight()
	logs := m.logs
	if len(logs) > n {
		logs = logs[len(logs)-n:]
	}
	if len(logs) == 0 {
		return dimStyle.Render("(no output yet)")
	}
	return strings.Join(logs, "\n")
}

func (m Model) logHeight() int {
	// total height minus title, two pane borders, step list, status
	used := len(m.labels) + 9
	h := m.height - used
	if h < 5 {
		return 8
	}
	return h
}

func (m Model) statusLine() string {
	switch m.state {
	case statePaused:
		where := fmt.Sprintf("%s step %d: %s", m.curAt.String(), m.cur+1, m.stepLabel(m.cur))
		if m.curErr != nil {
			where += errStyle.Render(" — step failed: " + m.curErr.Error())
		}
		return statusStyle.Render("⏸  paused "+where) +
			dimStyle.Render("\n   ↑↓ nav · b break · s step · c cont · g to-cursor · i edit-cmd · E edit-env · r rerun · e env · d shell · q quit")
	case stateRunning:
		return statusStyle.Render("▶  running") + dimStyle.Render("   ·  ↑↓ nav · b break · q quit")
	default:
		if m.runErr != nil {
			return errStyle.Render("✖  run failed: "+m.runErr.Error()) + dimStyle.Render("   ·  [q]uit")
		}
		return okStyle.Render("✓  run complete") + dimStyle.Render("   ·  [q]uit")
	}
}
