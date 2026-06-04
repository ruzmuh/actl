// Package tui is the Bubble Tea front-end for actl. It is one consumer of the
// debugger core (internal/debugger) and holds no debug logic of its own: it
// renders pause state + logs and translates keypresses into core commands
// (Step/Continue/Abort). Per CLAUDE.md §5 the dependency arrow points one way —
// tui imports debugger, never the reverse.
package tui

import (
	"context"
	"fmt"
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
)

// Model is the actl TUI state.
type Model struct {
	sess   *debugger.Session
	cancel context.CancelFunc

	title  string
	labels []string // step labels, in order

	state  runState
	cur    int // index of the step at the current/last pause (-1 before first)
	curAt  debugger.When
	curErr error

	logs    []string
	runErr  error
	showEnv bool // env pane instead of log pane

	cursor      int          // selected step, for arming breakpoints
	breakpoints map[int]bool // mirror of the core's breakpoints, for rendering

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
		state:       stateRunning,
		cur:         -1,
		breakpoints: make(map[int]bool),
	}
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
		m.state = statePaused
		m.cur = msg.Index
		m.curAt = msg.When
		m.curErr = msg.Err
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
	b.WriteString(titleStyle.Render("actl · "+m.title) + "\n\n")

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
		b.WriteString(paneStyle.Width(m.paneWidth()).Render("ENV (job-scoped)\n"+m.envPane()) + "\n")
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
		where := fmt.Sprintf("%s step %d", m.curAt.String(), m.cur+1)
		if m.curErr != nil {
			where += errStyle.Render(" — step failed: "+m.curErr.Error())
		}
		return statusStyle.Render("⏸  paused "+where) + dimStyle.Render("   ·  [↑↓]move  [b]reak  [s]tep  [c]ontinue  [e]nv  [d]shell  [q]uit")
	case stateRunning:
		return statusStyle.Render("▶  running") + dimStyle.Render("   ·  [↑↓]move  [b]reak  [q]uit")
	default:
		if m.runErr != nil {
			return errStyle.Render("✖  run failed: "+m.runErr.Error()) + dimStyle.Render("   ·  [q]uit")
		}
		return okStyle.Render("✓  run complete") + dimStyle.Render("   ·  [q]uit")
	}
}
