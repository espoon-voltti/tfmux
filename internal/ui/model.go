// Package ui is the Bubble Tea TUI: a repo→module→workspace tree with
// at-a-glance statuses, a detail viewport for plan logs, and keybindings to
// orchestrate plans (headless, parallel) and applies (tmux windows).
//
// Update is kept I/O-free: every side effect lives in a tea.Cmd (msgs.go) so
// message → state transitions are directly unit-testable.
package ui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/japsu/tfmux/internal/config"
	"github.com/japsu/tfmux/internal/domain"
	"github.com/japsu/tfmux/internal/gitstatus"
	"github.com/japsu/tfmux/internal/runner"
	"github.com/japsu/tfmux/internal/state"
	"github.com/japsu/tfmux/internal/tfexec"
	"github.com/japsu/tfmux/internal/tmuxctl"
)

type focusArea int

const (
	focusTree focusArea = iota
	focusDetail
	focusFilter
)

// Model is the root Bubble Tea model.
type Model struct {
	cfg    *config.Config
	store  *state.Store
	runner *runner.Runner
	git    gitstatus.Client
	tmux   *tmuxctl.Ctl
	tmuxOK bool

	repos   []*domain.Repo
	ignore  state.Ignore
	runs    map[string]*state.RunRecord // workspace key -> latest record
	planFiles    map[string]bool        // workspace key -> plan file on disk
	planning     map[string]bool        // workspace key -> plan in flight
	fingerprints map[string]string      // module path -> current git fingerprint
	applying     map[string]bool        // workspace key -> apply being polled

	rows        []row
	cursor      int
	collapsed   map[string]bool
	marked      map[string]bool
	showIgnored bool
	discovering bool

	focus      focusArea
	detail     viewport.Model
	detailKey  string
	filter     textinput.Model
	filterText string
	spinner    spinner.Model
	help       help.Model
	showHelp   bool
	confirmQuit bool

	status string
	width  int
	height int
}

func NewModel(cfg *config.Config, store *state.Store) *Model {
	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	fi := textinput.New()
	fi.Placeholder = "filter…"
	fi.Prompt = "/"
	return &Model{
		cfg:          cfg,
		store:        store,
		runner:       runner.New(cfg.Parallelism, store),
		git:          gitstatus.CLI{},
		tmux:         tmuxctl.New(cfg.TmuxSession),
		tmuxOK:       tmuxctl.Available(),
		runs:         map[string]*state.RunRecord{},
		planFiles:    map[string]bool{},
		planning:     map[string]bool{},
		fingerprints: map[string]string{},
		applying:     map[string]bool{},
		collapsed:    map[string]bool{},
		marked:       map[string]bool{},
		ignore:       state.Ignore{},
		spinner:      sp,
		filter:       fi,
		help:         help.New(),
		discovering:  true,
	}
}

func (m *Model) Init() tea.Cmd {
	if ig, err := m.store.LoadIgnore(); err == nil {
		m.ignore = ig
	}
	return tea.Batch(
		discoverCmd(m.cfg.Roots),
		waitForEvent(m.runner.Events),
		expirePlansCmd(m.store, m.cfg.PlanTTLDuration()),
		m.spinner.Tick,
	)
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.detail.Width = m.width/2 - 2
		m.detail.Height = m.height - 4
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case tea.KeyMsg:
		return m.updateKey(msg)

	case discoveryMsg:
		return m.updateDiscovery(msg)

	case gitStatusMsg:
		for _, repo := range m.repos {
			if repo.Path == msg.repoPath {
				repo.Git = msg.status
			}
		}
		return m, nil

	case runnerEventMsg:
		cmd := m.updateRunnerEvent(msg.ev)
		return m, tea.Batch(waitForEvent(m.runner.Events), cmd)

	case runsLoadedMsg:
		for k, v := range msg.runs {
			m.runs[k] = v
		}
		for k, v := range msg.planFiles {
			m.planFiles[k] = v
		}
		// resume polling applies that were in flight when tfmux last exited
		var cmds []tea.Cmd
		for k, rec := range msg.runs {
			if rec.Apply != nil && rec.Apply.ExitCode == nil && !rec.Apply.Aborted && !m.applying[k] {
				m.applying[k] = true
			}
		}
		if len(m.applying) > 0 {
			cmds = append(cmds, applyTick())
		}
		m.reflow()
		return m, tea.Batch(cmds...)

	case fingerprintMsg:
		m.fingerprints[msg.modulePath] = msg.fingerprint
		return m, nil

	case planLogMsg:
		if msg.err != nil {
			m.status = "no plan log: " + msg.err.Error()
			return m, nil
		}
		m.detailKey = msg.key
		m.detail.SetContent(colorizePlanLog(msg.content))
		m.detail.GotoTop()
		m.focus = focusDetail
		return m, nil

	case applyLaunchedMsg:
		return m.updateApplyLaunched(msg)

	case applyTickMsg:
		return m, m.pollAppliesCmd()

	case applyPollMsg:
		return m.updateApplyPoll(msg)

	case expiredPlansMsg:
		if msg.n > 0 {
			m.status = fmt.Sprintf("expired %d stale plan file(s)", msg.n)
		}
		return m, nil

	case savedMsg:
		if msg.err != nil {
			m.status = "state save failed: " + msg.err.Error()
		}
		return m, nil

	case statusMsg:
		m.status = msg.text
		return m, nil
	}

	if m.focus == focusDetail {
		var cmd tea.Cmd
		m.detail, cmd = m.detail.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *Model) updateDiscovery(msg discoveryMsg) (tea.Model, tea.Cmd) {
	m.discovering = false
	if msg.err != nil {
		m.status = "discovery failed: " + msg.err.Error()
		return m, nil
	}
	m.repos = msg.repos
	var cmds []tea.Cmd
	for _, repo := range m.repos {
		cmds = append(cmds, gitStatusCmd(m.git, repo.Path))
		for _, mod := range repo.Modules {
			mod.TFBin = m.cfg.BinFor(repo.Path)
			if m.ignore[repo.Path] || m.ignore[mod.Path] {
				continue
			}
			if m.runner.EnqueueEnumerate(mod) {
				mod.WorkspaceState = domain.WorkspacesLoading
			}
			cmds = append(cmds, fingerprintCmd(mod))
		}
	}
	m.reflow()
	return m, tea.Batch(cmds...)
}

func (m *Model) findModule(path string) *domain.Module {
	for _, repo := range m.repos {
		for _, mod := range repo.Modules {
			if mod.Path == path {
				return mod
			}
		}
	}
	return nil
}

func (m *Model) updateRunnerEvent(ev runner.Event) tea.Cmd {
	switch ev := ev.(type) {
	case runner.EnumStarted:
		if mod := m.findModule(ev.ModulePath); mod != nil {
			mod.WorkspaceState = domain.WorkspacesLoading
		}
	case runner.EnumFinished:
		mod := m.findModule(ev.ModulePath)
		if mod == nil {
			return nil
		}
		if ev.Err != "" {
			mod.WorkspaceState = domain.WorkspacesError
			mod.WorkspaceErr = ev.Err
			m.reflow()
			return nil
		}
		mod.WorkspaceState = domain.WorkspacesReady
		mod.WorkspaceErr = ""
		mod.Workspaces = nil
		for _, name := range ev.Workspaces {
			mod.Workspaces = append(mod.Workspaces, &domain.Workspace{Module: mod, Name: name})
		}
		m.reflow()
		return loadRunsCmd(m.store, mod, ev.Workspaces)
	case runner.PlanStarted:
		m.planning[ev.Key] = true
	case runner.PlanFinished:
		delete(m.planning, ev.Key)
		if ev.Err != "" {
			m.status = "plan failed: " + firstLine(ev.Err)
		}
		if ev.Record != nil {
			m.runs[ev.Key] = ev.Record
			m.planFiles[ev.Key] = ev.Record.PlanExitCode == tfexec.PlanChanges
			if mod := m.findModule(ev.Record.ModulePath); mod != nil {
				return fingerprintCmd(mod)
			}
		}
	case runner.InitStarted:
		m.status = "init -upgrade running…"
	case runner.InitFinished:
		if ev.Err != "" {
			m.status = "init -upgrade failed: " + firstLine(ev.Err)
			return nil
		}
		m.status = "init -upgrade done"
		if mod := m.findModule(ev.ModulePath); mod != nil && m.runner.EnqueueEnumerate(mod) {
			mod.WorkspaceState = domain.WorkspacesLoading
		}
	}
	return nil
}

// --- keyboard handling ---

func (m *Model) updateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// modal states first
	if m.confirmQuit {
		switch msg.String() {
		case "y", "Y", "enter":
			return m, tea.Quit
		default:
			m.confirmQuit = false
			return m, nil
		}
	}
	if m.focus == focusFilter {
		switch {
		case key.Matches(msg, keys.Esc):
			m.filter.SetValue("")
			m.filterText = ""
			m.focus = focusTree
			m.reflow()
			return m, nil
		case msg.Type == tea.KeyEnter:
			m.focus = focusTree
			return m, nil
		}
		var cmd tea.Cmd
		m.filter, cmd = m.filter.Update(msg)
		m.filterText = m.filter.Value()
		m.reflow()
		return m, cmd
	}
	if m.focus == focusDetail {
		switch {
		case key.Matches(msg, keys.Esc), key.Matches(msg, keys.Quit):
			m.focus = focusTree
			m.detailKey = ""
			return m, nil
		}
		var cmd tea.Cmd
		m.detail, cmd = m.detail.Update(msg)
		return m, cmd
	}

	switch {
	case key.Matches(msg, keys.Quit):
		if len(m.planning) > 0 {
			m.confirmQuit = true
			return m, nil
		}
		return m, tea.Quit
	case key.Matches(msg, keys.Help):
		m.showHelp = !m.showHelp
		return m, nil
	case key.Matches(msg, keys.Esc):
		m.showHelp = false
		return m, nil
	case key.Matches(msg, keys.Up):
		if m.cursor > 0 {
			m.cursor--
		}
	case key.Matches(msg, keys.Down):
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
	case key.Matches(msg, keys.Left):
		if r, ok := m.currentRow(); ok && r.kind != rowWorkspace {
			m.collapsed[r.nodeKey()] = true
			m.reflow()
		}
	case key.Matches(msg, keys.Right):
		if r, ok := m.currentRow(); ok && r.kind != rowWorkspace {
			delete(m.collapsed, r.nodeKey())
			m.reflow()
		}
	case key.Matches(msg, keys.Mark):
		if r, ok := m.currentRow(); ok && r.kind == rowWorkspace {
			k := r.ws.Key()
			if m.marked[k] {
				delete(m.marked, k)
			} else {
				m.marked[k] = true
			}
			if m.cursor < len(m.rows)-1 {
				m.cursor++
			}
		}
	case key.Matches(msg, keys.Filter):
		m.focus = focusFilter
		m.filter.Focus()
		return m, textinput.Blink
	case key.Matches(msg, keys.Plan):
		return m, m.planSelection()
	case key.Matches(msg, keys.PlanAll):
		return m, m.planAll()
	case key.Matches(msg, keys.Cancel):
		m.cancelCurrent()
	case key.Matches(msg, keys.View):
		if r, ok := m.currentRow(); ok && r.kind == rowWorkspace {
			return m, loadPlanLogCmd(m.store, r.mod.Path, r.ws.Name)
		}
	case key.Matches(msg, keys.Discard):
		if r, ok := m.currentRow(); ok && r.kind == rowWorkspace {
			k := r.ws.Key()
			m.planFiles[k] = false
			m.status = "plan discarded: " + r.ws.Name
			return m, discardPlanCmd(m.store, r.mod.Path, r.ws.Name)
		}
	case key.Matches(msg, keys.Ignore):
		return m, m.toggleIgnore()
	case key.Matches(msg, keys.ShowIgnored):
		m.showIgnored = !m.showIgnored
		m.reflow()
	case key.Matches(msg, keys.InitUpgrade):
		if r, ok := m.currentRow(); ok && r.mod != nil {
			m.runner.EnqueueInitUpgrade(r.mod)
		}
	case key.Matches(msg, keys.Refresh):
		return m, m.refresh()
	case key.Matches(msg, keys.Rediscover):
		m.discovering = true
		return m, discoverCmd(m.cfg.Roots)
	case key.Matches(msg, keys.Apply):
		return m, m.applyCurrent()
	case key.Matches(msg, keys.Attach):
		return m, m.attach()
	}
	return m, nil
}

func (m *Model) currentRow() (row, bool) {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return row{}, false
	}
	return m.rows[m.cursor], true
}

func (m *Model) reflow() {
	m.rows = m.flatten()
	if m.cursor >= len(m.rows) {
		m.cursor = len(m.rows) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

// planSelection plans marked workspaces, or everything under the cursor row.
func (m *Model) planSelection() tea.Cmd {
	var targets []*domain.Workspace
	if len(m.marked) > 0 {
		targets = m.workspacesByKeys(m.marked)
	} else if r, ok := m.currentRow(); ok {
		targets = m.workspacesUnder(r)
	}
	return m.enqueuePlans(targets)
}

func (m *Model) planAll() tea.Cmd {
	var targets []*domain.Workspace
	for _, r := range m.rows {
		if r.kind == rowWorkspace {
			targets = append(targets, r.ws)
		}
	}
	return m.enqueuePlans(targets)
}

func (m *Model) enqueuePlans(targets []*domain.Workspace) tea.Cmd {
	n := 0
	for _, ws := range targets {
		if m.ignore[ws.Key()] {
			continue
		}
		if m.runner.EnqueuePlan(ws) {
			m.planning[ws.Key()] = true
			n++
		}
	}
	if n > 0 {
		m.status = fmt.Sprintf("queued %d plan(s)", n)
	}
	return nil
}

func (m *Model) workspacesByKeys(keys map[string]bool) []*domain.Workspace {
	var out []*domain.Workspace
	for _, repo := range m.repos {
		for _, mod := range repo.Modules {
			for _, ws := range mod.Workspaces {
				if keys[ws.Key()] {
					out = append(out, ws)
				}
			}
		}
	}
	return out
}

func (m *Model) workspacesUnder(r row) []*domain.Workspace {
	switch r.kind {
	case rowWorkspace:
		return []*domain.Workspace{r.ws}
	case rowModule:
		return r.mod.Workspaces
	case rowRepo:
		var out []*domain.Workspace
		for _, mod := range r.repo.Modules {
			if !m.ignore[mod.Path] {
				out = append(out, mod.Workspaces...)
			}
		}
		return out
	}
	return nil
}

func (m *Model) cancelCurrent() {
	r, ok := m.currentRow()
	if !ok {
		return
	}
	switch r.kind {
	case rowWorkspace:
		m.runner.Cancel(r.ws.Key())
	case rowModule:
		m.runner.Cancel(r.mod.Path)
		for _, ws := range r.mod.Workspaces {
			m.runner.Cancel(ws.Key())
		}
	}
}

func (m *Model) toggleIgnore() tea.Cmd {
	r, ok := m.currentRow()
	if !ok {
		return nil
	}
	k := r.nodeKey()
	wasIgnored := m.ignore[k]
	if wasIgnored {
		delete(m.ignore, k)
	} else {
		m.ignore[k] = true
	}
	// re-enable: a module that was never enumerated needs it now
	if wasIgnored && r.kind == rowModule && r.mod.WorkspaceState == domain.WorkspacesUnknown {
		if m.runner.EnqueueEnumerate(r.mod) {
			r.mod.WorkspaceState = domain.WorkspacesLoading
		}
	}
	if wasIgnored && r.kind == rowRepo {
		for _, mod := range r.repo.Modules {
			if !m.ignore[mod.Path] && mod.WorkspaceState == domain.WorkspacesUnknown && m.runner.EnqueueEnumerate(mod) {
				mod.WorkspaceState = domain.WorkspacesLoading
			}
		}
	}
	m.reflow()
	ig := m.ignore
	store := m.store
	return func() tea.Msg {
		// copy under the cmd to avoid racing the model
		snapshot := state.Ignore{}
		for k, v := range ig {
			snapshot[k] = v
		}
		return savedMsg{err: store.SaveIgnore(snapshot)}
	}
}

func (m *Model) refresh() tea.Cmd {
	var cmds []tea.Cmd
	for _, repo := range m.repos {
		cmds = append(cmds, gitStatusCmd(m.git, repo.Path))
		for _, mod := range repo.Modules {
			if !m.ignore[repo.Path] && !m.ignore[mod.Path] {
				cmds = append(cmds, fingerprintCmd(mod))
			}
		}
	}
	cmds = append(cmds, expirePlansCmd(m.store, m.cfg.PlanTTLDuration()))
	m.status = "refreshing…"
	return tea.Batch(cmds...)
}

// --- apply ---

func (m *Model) applyCurrent() tea.Cmd {
	r, ok := m.currentRow()
	if !ok || r.kind != rowWorkspace {
		return nil
	}
	if !m.tmuxOK {
		m.status = "tmux not found — applies run in tmux windows. Install it: brew install tmux"
		return nil
	}
	key := r.ws.Key()
	rec := m.runs[key]
	switch {
	case m.planning[key]:
		m.status = "plan still running"
		return nil
	case rec == nil || rec.PlanExitCode != tfexec.PlanChanges:
		m.status = "nothing to apply — run a plan with changes first"
		return nil
	case !m.planFiles[key]:
		m.status = "plan file expired or discarded — re-plan first"
		return nil
	case rec.Apply != nil && rec.Apply.ExitCode == nil && !rec.Apply.Aborted:
		m.status = "apply already running — press t to attach"
		return nil
	case m.isStale(rec):
		m.status = "plan is STALE (module changed since plan) — re-plan, or attach and apply manually"
		return nil
	}
	mod := r.mod
	ws := r.ws.Name
	tmux := m.tmux
	store := m.store
	bin := mod.TFBin
	plannedVersion := rec.TFBinVersion
	return func() tea.Msg {
		// version guard: plan files aren't portable across binary versions
		if plannedVersion != "" {
			cur, err := (tfexec.TF{Bin: bin, Dir: mod.Path}).Version(context.Background())
			if err == nil && cur != plannedVersion {
				return statusMsg{text: fmt.Sprintf(
					"refusing to apply: plan made with %s %s, current is %s — re-plan",
					bin, plannedVersion, cur)}
			}
		}
		planFile, err := store.PlanFilePath(mod.Path, ws)
		if err != nil {
			return applyLaunchedMsg{key: key, err: err}
		}
		exitFile, err := store.ApplyExitPath(mod.Path, ws)
		if err != nil {
			return applyLaunchedMsg{key: key, err: err}
		}
		windowID, err := tmux.LaunchApply(tmuxctl.ApplySpec{
			ModuleDir: mod.Path,
			Workspace: ws,
			TFBin:     bin,
			PlanFile:  planFile,
			ExitFile:  exitFile,
			Name:      mod.Repo.Name + "/" + ws,
		})
		return applyLaunchedMsg{key: key, windowID: windowID, err: err}
	}
}

func (m *Model) updateApplyLaunched(msg applyLaunchedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.status = "apply launch failed: " + msg.err.Error()
		return m, nil
	}
	rec := m.runs[msg.key]
	if rec == nil {
		return m, nil
	}
	rec.Apply = &state.ApplyRecord{Started: time.Now(), WindowID: msg.windowID}
	m.applying[msg.key] = true
	m.status = "apply launched in tmux — press t to attach"
	return m, tea.Batch(saveRunCmd(m.store, rec), applyTick())
}

// pollAppliesCmd stats exit files and cross-checks live windows.
func (m *Model) pollAppliesCmd() tea.Cmd {
	if len(m.applying) == 0 {
		return nil
	}
	type target struct {
		key, modulePath, ws, windowID string
	}
	var targets []target
	for k := range m.applying {
		rec := m.runs[k]
		if rec == nil || rec.Apply == nil {
			continue
		}
		targets = append(targets, target{key: k, modulePath: rec.ModulePath, ws: rec.Workspace, windowID: rec.Apply.WindowID})
	}
	store := m.store
	tmux := m.tmux
	return func() tea.Msg {
		windows, werr := tmux.ListWindowIDs()
		var msg applyPollMsg
		msg.err = werr
		for _, t := range targets {
			exitFile, err := store.ApplyExitPath(t.modulePath, t.ws)
			if err != nil {
				continue
			}
			data, err := os.ReadFile(exitFile)
			if err == nil {
				var code int
				fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &code)
				msg.results = append(msg.results, applyPollResult{key: t.key, exitCode: &code})
				continue
			}
			if werr == nil && t.windowID != "" && !windows[t.windowID] {
				msg.results = append(msg.results, applyPollResult{key: t.key, vanished: true})
			}
		}
		return msg
	}
}

func (m *Model) updateApplyPoll(msg applyPollMsg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	for _, res := range msg.results {
		rec := m.runs[res.key]
		if rec == nil || rec.Apply == nil {
			delete(m.applying, res.key)
			continue
		}
		now := time.Now()
		switch {
		case res.exitCode != nil:
			rec.Apply.ExitCode = res.exitCode
			rec.Apply.Finished = &now
			if *res.exitCode == 0 {
				m.planFiles[res.key] = false
				m.status = fmt.Sprintf("applied %s//%s ✓", rec.ModulePath, rec.Workspace)
				cmds = append(cmds, discardPlanCmd(m.store, rec.ModulePath, rec.Workspace))
			} else {
				m.status = fmt.Sprintf("apply failed (exit %d) — press t to inspect the tmux window", *res.exitCode)
			}
		case res.vanished:
			rec.Apply.Aborted = true
			rec.Apply.Finished = &now
			m.status = "apply window closed without finishing — state unknown, re-plan"
		}
		delete(m.applying, res.key)
		cmds = append(cmds, saveRunCmd(m.store, rec))
	}
	if len(m.applying) > 0 {
		cmds = append(cmds, applyTick())
	}
	return m, tea.Batch(cmds...)
}

func (m *Model) attach() tea.Cmd {
	if !m.tmuxOK {
		m.status = "tmux not found — install it: brew install tmux"
		return nil
	}
	windowID := ""
	if r, ok := m.currentRow(); ok && r.kind == rowWorkspace {
		if rec := m.runs[r.ws.Key()]; rec != nil && rec.Apply != nil {
			windowID = rec.Apply.WindowID
		}
	}
	return tea.ExecProcess(m.tmux.AttachCmd(windowID), func(err error) tea.Msg {
		if err != nil {
			return statusMsg{text: "tmux attach: " + err.Error()}
		}
		return statusMsg{text: ""}
	})
}

// --- view ---

func (m *Model) View() string {
	if m.width == 0 {
		return "loading…"
	}
	title := styleTitle.Render("tfmux")
	meta := []string{}
	if m.discovering {
		meta = append(meta, m.spinner.View()+" discovering")
	}
	if n := len(m.planning); n > 0 {
		meta = append(meta, fmt.Sprintf("%s %d planning", m.spinner.View(), n))
	}
	if n := len(m.applying); n > 0 {
		meta = append(meta, fmt.Sprintf("%d applying", n))
	}
	if !m.tmuxOK {
		meta = append(meta, styleError.Render("tmux: unavailable"))
	}
	header := title + "  " + styleDim.Render(strings.Join(meta, "  "))

	bodyHeight := m.height - 3
	var body string
	if m.showHelp {
		body = m.help.FullHelpView(keys.FullHelp())
	} else {
		tree := m.renderTree(bodyHeight)
		if m.focus == focusDetail {
			treeW := m.width / 2
			m.detail.Width = m.width - treeW - 1
			m.detail.Height = bodyHeight
			body = lipgloss.JoinHorizontal(lipgloss.Top,
				lipgloss.NewStyle().Width(treeW).MaxWidth(treeW).Render(tree),
				m.detail.View(),
			)
		} else {
			body = tree
		}
	}

	statusLeft := m.status
	if m.confirmQuit {
		statusLeft = styleChanges.Render("plans still running — quit anyway? (y/N)")
	}
	var bottom string
	if m.focus == focusFilter {
		bottom = m.filter.View()
	} else {
		bottom = styleHelpLine.Render(m.help.ShortHelpView(keys.ShortHelp()))
	}
	statusBar := styleStatusBar.Width(m.width).Render(statusLeft)

	return lipgloss.JoinVertical(lipgloss.Left,
		header,
		lipgloss.NewStyle().Height(bodyHeight).MaxHeight(bodyHeight).Render(body),
		statusBar,
		bottom,
	)
}

// renderTree renders visible rows with a scroll window around the cursor.
func (m *Model) renderTree(height int) string {
	if len(m.rows) == 0 {
		if m.discovering {
			return styleDim.Render("  discovering repos…")
		}
		if len(m.cfg.Roots) == 0 {
			return styleDim.Render("  no roots configured — add roots = [\"~/path/to/iac\"] to config.toml")
		}
		return styleDim.Render("  nothing found under configured roots")
	}
	treeWidth := m.width
	if m.focus == focusDetail {
		treeWidth = m.width / 2
	}
	start := 0
	if m.cursor >= height {
		start = m.cursor - height + 1
	}
	end := start + height
	if end > len(m.rows) {
		end = len(m.rows)
	}
	var b strings.Builder
	for i := start; i < end; i++ {
		b.WriteString(m.renderRow(m.rows[i], i == m.cursor && m.focus == focusTree, treeWidth))
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}
