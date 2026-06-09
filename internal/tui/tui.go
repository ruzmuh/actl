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

	"github.com/charmbracelet/bubbles/viewport"
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

	title   string
	labels  []string // step labels, in order
	notices []string // transparency banner: isolated-run needs + workspace caveats

	state  runState
	cur    int // index of the step at the current/last pause (-1 before first)
	curAt  debugger.When
	curErr error

	logs    []string
	logVP   viewport.Model // scrollable LOGS pane; m.logs is its backing buffer
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
	// Disable the viewport's built-in keymap: its defaults (d, u, b, f, pgup…)
	// would collide with actl's keys (d=shell, b=break, …). We drive scrolling
	// explicitly and let it keep only mouse-wheel handling. Seed it with the same
	// fallback size paneWidth/logViewportHeight use so it renders before the first
	// WindowSizeMsg; real dimensions arrive with that message.
	vp := viewport.New(76, 8)
	vp.KeyMap = viewport.KeyMap{}
	return Model{
		sess:        sess,
		cancel:      cancel,
		title:       fmt.Sprintf("job %q", sess.JobID()),
		labels:      labels,
		notices:     noticeLines(sess),
		state:       stateRunning,
		cur:         -1,
		logVP:       vp,
		breakpoints: make(map[int]bool),
		tempBreak:   -1,
	}
}

// noticeLines builds the transparency banner: how the job's needs are satisfied
// (seeded or live), plus a workspace caveat when running with an empty workspace
// and the job has local actions that therefore won't resolve.
func noticeLines(sess *debugger.Session) []string {
	lines := needsLines(sess.NeedsSummary())
	if ws := sess.Workspace(); ws != "" {
		lines = append(lines, fmt.Sprintf("workspace %s is mounted — steps run in the container can write to it", ws))
	}
	if sess.WorkspaceIsolated() {
		if locals := sess.LocalUsesSteps(); len(locals) > 0 {
			lines = append(lines, fmt.Sprintf("empty workspace — %d local action(s) won't resolve; mount your repo with -workdir . : %s",
				len(locals), strings.Join(locals, ", ")))
		}
	}
	if cfg := configLine(sess.ConfigSummary()); cfg != "" {
		lines = append(lines, cfg)
	}
	lines = append(lines, tokenLine(sess.TokenSummary()))
	if in := inputsLine(sess.InputsSummary()); in != "" {
		lines = append(lines, in)
	}
	if ev := eventLine(sess.EventSummary()); ev != "" {
		lines = append(lines, ev)
	}
	lines = append(lines, ghcLine(sess.GitHubContextSummary()))
	if steps := sess.CheckoutSteps(); len(steps) > 0 {
		if src := sess.CheckoutSource(); src != "" {
			lines = append(lines, fmt.Sprintf("checkout intercepted (%s) — copies your working tree %s (.gitignore respected)",
				strings.Join(steps, ", "), src))
		} else {
			lines = append(lines, fmt.Sprintf("checkout intercepted (%s) — using the mounted workspace", strings.Join(steps, ", ")))
		}
	}
	if svc := servicesLine(sess.ServicesSummary()); svc != "" {
		lines = append(lines, svc)
	}
	lines = append(lines, gcpLines(sess.GCPSummary())...)
	lines = append(lines, awsLines(sess.AWSSummary())...)
	return lines
}

// servicesLine renders the job's `services:` containers. act starts them natively
// when the job runs; we just note they will start so their appearance isn't a
// surprise. Empty when the job declares no services.
func servicesLine(s debugger.ServicesSummary) string {
	if len(s.Names) == 0 {
		return ""
	}
	return fmt.Sprintf("services: act will start %d service container(s): %s",
		len(s.Names), strings.Join(s.Names, ", "))
}

// gcpLines renders the GCP identity substitution: the federation each auth step
// would have used in real CI vs the local identity we run as, and what we injected.
// This honest notice is the point of ambient substitution (CLAUDE.md §4). Empty
// when the job has no google-github-actions/auth step.
func gcpLines(g debugger.GCPSummary) []string {
	if len(g.Steps) == 0 {
		return nil
	}
	local := g.Account
	if local == "" {
		local = "your ambient gcloud identity"
	}
	var lines []string
	for i, step := range g.Steps {
		target := "(federated identity)"
		if i < len(g.Targets) {
			target = g.Targets[i]
		}
		lines = append(lines, fmt.Sprintf("gcp identity (%s): would federate as %s → running locally as %s", step, target, local))
	}
	switch {
	case g.File && g.Token:
		lines = append(lines, "gcp identity: mounted ambient ADC file + access token into the job")
	case g.File:
		lines = append(lines, "gcp identity: mounted ambient ADC file into the job")
	case g.Token:
		lines = append(lines, "gcp identity: injected an ambient access token into the job")
	default:
		lines = append(lines, "gcp identity: no ambient credentials found — cloud calls will fail (run: gcloud auth application-default login)")
	}
	// We authenticate but never set a project (that's the workflow's concern, as on
	// GitHub) — surface it so a later project-less gcloud/SDK call's failure isn't a
	// surprise. Only when creds were actually injected; the no-creds case above already
	// has its own actionable message.
	if g.File || g.Token {
		lines = append(lines, "gcp identity: no project set by actl — pass GOOGLE_CLOUD_PROJECT via -env if a step needs one")
	}
	return lines
}

// awsLines renders the AWS identity substitution: the role+region each auth step
// would have federated as in real CI vs the local identity we run as, and what we
// injected. The AWS analog of gcpLines (CLAUDE.md §4). Empty when the job has no
// aws-actions/configure-aws-credentials step.
func awsLines(a debugger.AWSSummary) []string {
	if len(a.Steps) == 0 {
		return nil
	}
	local := a.Account
	if local == "" {
		local = "your ambient AWS identity"
	}
	var lines []string
	for i, step := range a.Steps {
		target := "(federated identity)"
		if i < len(a.Targets) {
			target = a.Targets[i]
		}
		lines = append(lines, fmt.Sprintf("aws identity (%s): would federate as %s → running locally as %s", step, target, local))
	}
	switch {
	case a.Creds && a.RegionSet:
		lines = append(lines, fmt.Sprintf("aws identity: injected ambient credentials + region %s into the job", a.Region))
	case a.Creds:
		lines = append(lines, "aws identity: injected ambient credentials into the job — no region set; pass AWS_REGION via -env if a step needs one")
	default:
		lines = append(lines, "aws identity: no ambient credentials found — cloud calls will fail (run: aws sso login, or set -aws-profile)")
	}
	return lines
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

// configLine renders a redacted summary of supplied secrets/vars/env: counts
// and names, never values. Empty when nothing was supplied.
func configLine(cfg debugger.ConfigSummary) string {
	var parts []string
	if n := len(cfg.Secrets); n > 0 {
		parts = append(parts, fmt.Sprintf("%d secret(s): %s", n, strings.Join(cfg.Secrets, ", ")))
	}
	if n := len(cfg.Vars); n > 0 {
		parts = append(parts, fmt.Sprintf("%d var(s): %s", n, strings.Join(cfg.Vars, ", ")))
	}
	if n := len(cfg.Env); n > 0 {
		parts = append(parts, fmt.Sprintf("%d env: %s", n, strings.Join(cfg.Env, ", ")))
	}
	if len(parts) == 0 {
		return ""
	}
	return "loaded " + strings.Join(parts, " · ") + " (names only — values withheld)"
}

// tokenLine renders the github.token substitution: in real CI it's an ephemeral,
// repo-scoped token; locally we run as the dev's own token (or none). Like identity
// (§4), the honest note is the point — your token's scope differs from CI's.
func tokenLine(t debugger.TokenSummary) string {
	if !t.Present {
		return "github token: none set — github.token is empty; gh, API calls, and checkout with a ref will fail (pass -github-token, set GITHUB_TOKEN, or 'gh auth login')"
	}
	return fmt.Sprintf("github token: github.token set (from %s) — running as your token, broader scope than CI's ephemeral repo-scoped GITHUB_TOKEN", t.Source)
}

// inputsLine renders which declared workflow inputs were supplied vs left to their
// declared default (act fills the defaults itself). Empty when the event takes no
// inputs and none were supplied.
func inputsLine(s debugger.InputsSummary) string {
	if !s.Declared && len(s.Provided) == 0 {
		return ""
	}
	var parts []string
	if len(s.Provided) > 0 {
		keys := make([]string, 0, len(s.Provided))
		for k := range s.Provided {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		pairs := make([]string, 0, len(keys))
		for _, k := range keys {
			pairs = append(pairs, k+"="+s.Provided[k])
		}
		parts = append(parts, "provided "+strings.Join(pairs, ", "))
	}
	if len(s.Defaults) > 0 {
		parts = append(parts, fmt.Sprintf("%d using declared default(s): %s", len(s.Defaults), strings.Join(s.Defaults, ", ")))
	}
	if len(parts) == 0 {
		return "inputs: declared, none provided (act applies declared defaults)"
	}
	return "inputs: " + strings.Join(parts, " · ")
}

// eventLine renders the github.event payload source. Only shown when the user
// supplied an event file; a synthesized empty payload is the unremarkable default.
func eventLine(e debugger.EventSummary) string {
	if e.Synthetic {
		return ""
	}
	return fmt.Sprintf("event payload: %s (event_name=%s)", e.Path, e.EventName)
}

// ghcLine renders the synthesized github.* runtime context (CLAUDE.md §4): the
// repository/ref/sha actl resolved from local git (or you overrode), and the
// placeholders a clean local runner can't know (actor, run ids). Honest visibility
// into what the workflow stands on locally.
func ghcLine(g debugger.GitHubContextSummary) string {
	ov := map[string]bool{}
	for _, o := range g.Overridden {
		ov[o] = true
	}
	field := func(name, val string) string {
		if val == "" {
			val = "act-derived"
		}
		if ov[name] {
			val += " (override)"
		}
		return val
	}
	actor := g.Actor
	switch {
	case actor == "":
		actor = "nektos/act (placeholder)"
	case ov["actor"]:
		actor += " (override)"
	}
	return fmt.Sprintf("github context: repository=%s · ref=%s · sha=%s · actor=%s · run_id/number=1 (placeholder)",
		field("repository", g.Repository), field("ref", g.Ref), field("sha", g.Sha), actor)
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
		m.logVP.Width = m.paneWidth()
		m.logVP.Height = m.logViewportHeight()
		m.setLogContent()
		return m, nil

	case tea.MouseMsg:
		// Wheel scrolls the logs, but only while the LOGS pane is showing.
		if !m.showEnv {
			var cmd tea.Cmd
			m.logVP, cmd = m.logVP.Update(msg)
			return m, cmd
		}
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
		case "pgup":
			m.logVP.PageUp()
			return m, nil
		case "pgdown":
			m.logVP.PageDown()
			return m, nil
		case "ctrl+u":
			m.logVP.HalfPageUp()
			return m, nil
		case "ctrl+d":
			m.logVP.HalfPageDown()
			return m, nil
		case "home":
			m.logVP.GotoTop()
			return m, nil
		case "end":
			m.logVP.GotoBottom()
			return m, nil
		case "i":
			if m.state == statePaused {
				if m.sess.CurrentRun() == "" {
					m.appendLog("edit: only run: steps have a command to edit")
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
					m.appendLog("rerun: available only after a step has run (step onto it first)")
					return m, nil
				}
				m.appendLog(fmt.Sprintf("re-running step %d…", m.cur+1))
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
		m.appendLog(string(msg))
		return m, m.waitLog

	case logsDoneMsg:
		return m, nil

	case shellDoneMsg:
		if msg.err != nil {
			m.appendLog("shell: " + msg.err.Error())
		}
		return m, nil

	case rerunDoneMsg:
		if msg.err != nil {
			m.appendLog("rerun: " + msg.err.Error())
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
	for _, line := range m.notices {
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
		title := "LOGS"
		if !m.logVP.AtBottom() {
			title += dimStyle.Render("  ↑ scrolled · End to follow")
		}
		body := m.logVP.View()
		if len(m.logs) == 0 {
			body = dimStyle.Render("(no output yet)")
		}
		b.WriteString(paneStyle.Width(m.paneWidth()).Render(title+"\n"+body) + "\n")
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
		m.appendLog("run-to-cursor: already stopped before this step")
		return m, nil
	case target <= m.cur:
		m.appendLog("run-to-cursor: target is behind the current step — can't run backward")
		return m, nil
	}
	// Arm a temp breakpoint unless the user already has a real one there (which
	// we must not clobber when the pause clears it).
	if !m.breakpoints[target] {
		m.tempBreak = target
		m.sess.SetBreakpoint(target, true)
	}
	m.appendLog(fmt.Sprintf("running to step %d: %s", target+1, m.stepLabel(target)))
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
		m.appendLog("edit: " + msg.err.Error())
		return
	}
	switch msg.kind {
	case editRun:
		m.sess.SetRun(strings.TrimRight(msg.content, "\n"))
		m.appendLog("edited step command (in memory) — press r to rerun")
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
		m.appendLog(fmt.Sprintf("applied %d env var(s) (in memory) — press r to rerun", n))
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

	limit := m.logViewportHeight()
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

// appendLog adds one line to the log buffer (capped) and refreshes the viewport.
// All log writes — streamed core output and the TUI's own status notes — go
// through here so the scrollable view stays in sync.
func (m *Model) appendLog(line string) {
	m.logs = append(m.logs, line)
	if len(m.logs) > 2000 {
		m.logs = m.logs[len(m.logs)-2000:]
	}
	m.setLogContent()
}

// setLogContent pushes the buffer into the viewport, following the tail unless
// the user has scrolled up — then their position is preserved as lines arrive.
func (m *Model) setLogContent() {
	follow := m.logVP.AtBottom()
	m.logVP.SetContent(strings.Join(m.logs, "\n"))
	if follow {
		m.logVP.GotoBottom()
	}
}

// logViewportHeight is the row budget for the bottom pane's scrollable area:
// total height minus the title, transparency notices, the blank spacer, the
// STEPS pane (border + title + one row per step), the LOGS pane chrome
// (border + title), and the two-line status block.
func (m Model) logViewportHeight() int {
	used := 1 + len(m.notices) + 1 + (len(m.labels) + 3) + 3 + 2
	h := m.height - used
	if h < 3 {
		return 3
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
			dimStyle.Render("\n   ↑↓ nav · b break · s step · c cont · g to-cursor · i edit-cmd · E edit-env · r rerun · e env · d shell · PgUp/PgDn scroll · q quit")
	case stateRunning:
		return statusStyle.Render("▶  running") + dimStyle.Render("   ·  ↑↓ nav · b break · PgUp/PgDn scroll · q quit")
	default:
		if m.runErr != nil {
			return errStyle.Render("✖  run failed: "+m.runErr.Error()) + dimStyle.Render("   ·  [q]uit")
		}
		return okStyle.Render("✓  run complete") + dimStyle.Render("   ·  [q]uit")
	}
}
