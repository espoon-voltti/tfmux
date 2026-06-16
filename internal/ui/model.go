// Package ui is the Bubble Tea TUI: a repo→module→workspace tree with
// at-a-glance statuses, a detail viewport for plan logs, and keybindings to
// orchestrate plans (headless, parallel) and applies (tmux windows).
//
// Update is kept I/O-free: every side effect lives in a tea.Cmd (msgs.go) so
// message → state transitions are directly unit-testable.
package ui

import (
	"fmt"
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

// taskState mirrors an in-flight runner task for display. A task is created
// (queued) when the UI enqueues it, flipped to running on the runner's
// PhaseRunning event, and removed on any terminal event.
type taskState struct {
	kind     runner.Kind
	running  bool   // false: queued waiting for a slot; true: executing
	windowID string // apply: the tmux window to attach to
}

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

	repos        []*domain.Repo
	ignore       state.Ignore
	runs         map[string]*state.RunRecord // workspace key -> latest record
	planFiles    map[string]bool             // workspace key -> plan file on disk
	tasks        map[string]*taskState       // runner.TaskID -> in-flight task
	fingerprints map[string]string           // module path -> current git fingerprint

	rows        []row
	cursor      int
	top         int // index of the first visible row (scroll offset)
	collapsed   map[string]bool
	marked      map[string]bool
	showIgnored bool
	discovering bool

	focus       focusArea
	detail      viewport.Model
	detailKey   string
	filter      textinput.Model
	filterText  string
	spinner     spinner.Model
	help        help.Model
	showHelp    bool
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
	tmux := tmuxctl.New(cfg.TmuxSession)
	return &Model{
		cfg:          cfg,
		store:        store,
		runner:       runner.New(cfg.Parallelism, store, tmux),
		git:          gitstatus.CLI{},
		tmux:         tmux,
		tmuxOK:       tmuxctl.Available(),
		runs:         map[string]*state.RunRecord{},
		planFiles:    map[string]bool{},
		tasks:        map[string]*taskState{},
		fingerprints: map[string]string{},
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
		discoverCmd(m.cfg.Roots, false),
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
		m.ensureVisible()
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
		// re-adopt applies that were still running in tmux when tfmux exited
		for _, rec := range msg.runs {
			if rec.Apply != nil && rec.Apply.ExitCode == nil && !rec.Apply.Aborted {
				if m.runner.EnqueueApplyPoll(rec.ModulePath, rec.Workspace, rec.Apply.WindowID) {
					m.addTask(runner.KindApply, rec.ModulePath+"//"+rec.Workspace)
				}
			}
		}
		m.reflow()
		return m, nil

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
			// Prefer the cached enumeration: listing workspaces hits the
			// backend and is slow/rate-limited. Refresh is explicit (w / R).
			if cache, ok := m.store.LoadWorkspaces(mod.Path); ok && !msg.force {
				m.applyWorkspaces(mod, cache.Workspaces)
				cmds = append(cmds, loadRunsCmd(m.store, mod, cache.Workspaces))
			} else if m.runner.EnqueueEnumerate(mod) {
				m.addTask(runner.KindEnumerate, mod.Path)
			}
			cmds = append(cmds, fingerprintCmd(mod))
		}
	}
	m.reflow()
	return m, tea.Batch(cmds...)
}

// applyWorkspaces sets a module's workspace list (from cache or a fresh
// enumeration) and marks it ready.
func (m *Model) applyWorkspaces(mod *domain.Module, names []string) {
	mod.WorkspaceState = domain.WorkspacesReady
	mod.WorkspaceErr = ""
	mod.Workspaces = nil
	for _, name := range names {
		mod.Workspaces = append(mod.Workspaces, &domain.Workspace{Module: mod, Name: name})
	}
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

// --- task store ---

func (m *Model) addTask(kind runner.Kind, key string) {
	m.tasks[runner.TaskID(kind, key)] = &taskState{kind: kind}
}

func (m *Model) task(kind runner.Kind, key string) *taskState {
	return m.tasks[runner.TaskID(kind, key)]
}

func (m *Model) hasTask(kind runner.Kind, key string) bool {
	return m.tasks[runner.TaskID(kind, key)] != nil
}

func (m *Model) anyTask(kind runner.Kind) bool {
	for _, ts := range m.tasks {
		if ts.kind == kind {
			return true
		}
	}
	return false
}

// updateRunnerEvent folds a task lifecycle transition into the model: running
// flips the task's display state, terminal events remove it and apply the
// kind-specific result.
func (m *Model) updateRunnerEvent(ev runner.Event) tea.Cmd {
	id := ev.TaskID()
	if ev.Phase == runner.PhaseRunning {
		ts := m.tasks[id]
		if ts == nil {
			ts = &taskState{kind: ev.Kind}
			m.tasks[id] = ts
		}
		ts.running = true
		return m.taskRunning(ev, ts)
	}
	delete(m.tasks, id)
	return m.taskTerminal(ev)
}

func (m *Model) taskRunning(ev runner.Event, ts *taskState) tea.Cmd {
	switch ev.Kind {
	case runner.KindInit:
		m.status = "init -upgrade running…"
	case runner.KindApply:
		if ev.WindowID != "" {
			ts.windowID = ev.WindowID
			if rec := m.runs[ev.Key]; rec != nil {
				rec.Apply = &state.ApplyRecord{Started: time.Now(), WindowID: ev.WindowID}
				m.status = "apply launched in tmux — press t to attach"
				return saveRunCmd(m.store, rec)
			}
		}
	}
	return nil
}

func (m *Model) taskTerminal(ev runner.Event) tea.Cmd {
	switch ev.Kind {
	case runner.KindEnumerate:
		return m.enumerateDone(ev)
	case runner.KindInit:
		return m.initDone(ev)
	case runner.KindPlan:
		return m.planDone(ev)
	case runner.KindApply:
		return m.applyDone(ev)
	}
	return nil
}

func (m *Model) enumerateDone(ev runner.Event) tea.Cmd {
	mod := m.findModule(ev.ModulePath)
	if mod == nil {
		return nil
	}
	switch ev.Phase {
	case runner.PhaseFailed:
		mod.WorkspaceState = domain.WorkspacesError
		mod.WorkspaceErr = ev.Err
		m.reflow()
	case runner.PhaseDone:
		m.applyWorkspaces(mod, ev.Workspaces)
		m.reflow()
		return loadRunsCmd(m.store, mod, ev.Workspaces)
	case runner.PhaseCanceled:
		m.reflow()
	}
	return nil
}

func (m *Model) initDone(ev runner.Event) tea.Cmd {
	switch ev.Phase {
	case runner.PhaseFailed:
		m.status = "init -upgrade failed: " + firstLine(ev.Err)
	case runner.PhaseDone:
		m.status = "init -upgrade done"
		if mod := m.findModule(ev.ModulePath); mod != nil && m.runner.EnqueueEnumerate(mod) {
			m.addTask(runner.KindEnumerate, mod.Path)
		}
	}
	return nil
}

func (m *Model) planDone(ev runner.Event) tea.Cmd {
	if ev.Phase == runner.PhaseCanceled {
		return nil
	}
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
	return nil
}

func (m *Model) applyDone(ev runner.Event) tea.Cmd {
	switch ev.Phase {
	case runner.PhaseFailed:
		m.status = firstLine(ev.Err)
		return nil
	case runner.PhaseCanceled:
		m.status = "apply canceled before launch"
		return nil
	}
	rec := m.runs[ev.Key]
	if rec == nil || rec.Apply == nil {
		return nil
	}
	now := time.Now()
	rec.Apply.Finished = &now
	switch {
	case ev.Aborted:
		rec.Apply.Aborted = true
		m.status = "apply window closed without finishing — state unknown, re-plan"
	case ev.ApplyExit != nil:
		rec.Apply.ExitCode = ev.ApplyExit
		if *ev.ApplyExit == 0 {
			m.planFiles[ev.Key] = false
			m.status = fmt.Sprintf("applied %s//%s ✓", rec.ModulePath, rec.Workspace)
			return tea.Batch(saveRunCmd(m.store, rec), discardPlanCmd(m.store, rec.ModulePath, rec.Workspace))
		}
		m.status = fmt.Sprintf("apply failed (exit %d) — press t to inspect the tmux window", *ev.ApplyExit)
	}
	return saveRunCmd(m.store, rec)
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
		if m.anyTask(runner.KindPlan) {
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
		m.ensureVisible()
	case key.Matches(msg, keys.Down):
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
		m.ensureVisible()
	case key.Matches(msg, keys.PageUp):
		m.pageUp()
	case key.Matches(msg, keys.PageDown):
		m.pageDown()
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
			m.ensureVisible()
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
		if r, ok := m.currentRow(); ok {
			switch {
			case r.kind == rowWorkspace:
				return m, loadPlanLogCmd(m.store, r.mod.Path, r.ws.Name)
			case r.kind == rowModule && r.mod.WorkspaceState == domain.WorkspacesError:
				m.detailKey = r.mod.Path
				m.detail.SetContent(colorizePlanLog(r.mod.WorkspaceErr))
				m.detail.GotoTop()
				m.focus = focusDetail
				return m, nil
			}
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
		if r, ok := m.currentRow(); ok && r.mod != nil && m.runner.EnqueueInitUpgrade(r.mod) {
			m.addTask(runner.KindInit, r.mod.Path)
		}
	case key.Matches(msg, keys.Refresh):
		return m, m.refresh()
	case key.Matches(msg, keys.RefreshWorkspaces):
		m.refreshWorkspaces()
	case key.Matches(msg, keys.Rediscover):
		m.discovering = true
		return m, discoverCmd(m.cfg.Roots, true)
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
	m.ensureVisible()
}

// visibleHeight is the number of tree rows on screen, matching the bodyHeight
// the View hands to renderTree.
func (m *Model) visibleHeight() int {
	h := m.height - 3
	if h < 1 {
		h = 1
	}
	return h
}

// ensureVisible scrolls the window minimally so the cursor stays on screen.
// Because the scroll offset is stored (not derived from the cursor), moving up
// from the bottom of the list walks the cursor up the screen rather than
// scrolling the whole list.
func (m *Model) ensureVisible() {
	h := m.visibleHeight()
	if m.cursor < m.top {
		m.top = m.cursor
	}
	if m.cursor >= m.top+h {
		m.top = m.cursor - h + 1
	}
	if maxTop := len(m.rows) - h; m.top > maxTop {
		m.top = maxTop
	}
	if m.top < 0 {
		m.top = 0
	}
}

// pageDown moves first to the bottom of the visible screen, then a screenful
// at a time (keeping one row of context overlap).
func (m *Model) pageDown() {
	h := m.visibleHeight()
	bottom := m.top + h - 1
	if last := len(m.rows) - 1; bottom > last {
		bottom = last
	}
	if m.cursor < bottom {
		m.cursor = bottom
	} else {
		m.cursor += h - 1
		if last := len(m.rows) - 1; m.cursor > last {
			m.cursor = last
		}
	}
	m.ensureVisible()
}

// pageUp mirrors pageDown: first to the top of the screen, then a screenful up.
func (m *Model) pageUp() {
	h := m.visibleHeight()
	if m.cursor > m.top {
		m.cursor = m.top
	} else {
		m.cursor -= h - 1
		if m.cursor < 0 {
			m.cursor = 0
		}
	}
	m.ensureVisible()
}

// refreshWorkspaces re-enumerates workspaces for the module(s) under the cursor,
// overwriting the on-disk cache once each enumeration completes.
func (m *Model) refreshWorkspaces() {
	r, ok := m.currentRow()
	if !ok {
		return
	}
	var mods []*domain.Module
	switch r.kind {
	case rowRepo:
		for _, mod := range r.repo.Modules {
			if !m.ignore[mod.Path] {
				mods = append(mods, mod)
			}
		}
	case rowModule, rowWorkspace:
		mods = append(mods, r.mod)
	}
	n := 0
	for _, mod := range mods {
		if m.runner.EnqueueEnumerate(mod) {
			m.addTask(runner.KindEnumerate, mod.Path)
			n++
		}
	}
	if n > 0 {
		m.status = fmt.Sprintf("re-enumerating workspaces for %d module(s)…", n)
		m.reflow()
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
			m.addTask(runner.KindPlan, ws.Key())
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
	var keys []string
	switch r.kind {
	case rowWorkspace:
		keys = append(keys, r.ws.Key())
	case rowModule:
		keys = append(keys, r.mod.Path) // enumeration / init, if any
		for _, ws := range r.mod.Workspaces {
			keys = append(keys, ws.Key())
		}
	}
	for _, k := range keys {
		m.runner.Cancel(k)
		m.forgetTasks(k)
	}
}

// forgetTasks optimistically drops a key's in-flight tasks so the UI reacts to
// a cancel immediately (the runner's Canceled event is then a no-op). A running
// apply is kept — it lives on in tmux and is left attached.
func (m *Model) forgetTasks(key string) {
	for _, kind := range []runner.Kind{runner.KindEnumerate, runner.KindInit, runner.KindPlan, runner.KindApply} {
		if ts := m.task(kind, key); ts != nil {
			if kind == runner.KindApply && ts.running {
				continue
			}
			delete(m.tasks, runner.TaskID(kind, key))
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
			m.addTask(runner.KindEnumerate, r.mod.Path)
		}
	}
	if wasIgnored && r.kind == rowRepo {
		for _, mod := range r.repo.Modules {
			if !m.ignore[mod.Path] && mod.WorkspaceState == domain.WorkspacesUnknown && m.runner.EnqueueEnumerate(mod) {
				m.addTask(runner.KindEnumerate, mod.Path)
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
	case m.hasTask(runner.KindPlan, key):
		m.status = "plan still running"
		return nil
	case rec == nil || rec.PlanExitCode != tfexec.PlanChanges:
		m.status = "nothing to apply — run a plan with changes first"
		return nil
	case !m.planFiles[key]:
		m.status = "plan file expired or discarded — re-plan first"
		return nil
	case m.hasTask(runner.KindApply, key):
		m.status = "apply already queued/running — press t to attach"
		return nil
	case m.isStale(rec):
		m.status = "plan is STALE (module changed since plan) — re-plan, or attach and apply manually"
		return nil
	}
	// The runner launches the tmux window, guards the binary version, and
	// watches the apply to completion while holding a pool slot.
	if m.runner.EnqueueApply(r.ws, rec.TFBinVersion) {
		m.addTask(runner.KindApply, key)
		m.status = "apply queued"
	}
	return nil
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
	var running, queued int
	for _, ts := range m.tasks {
		if ts.running {
			running++
		} else {
			queued++
		}
	}
	if running+queued > 0 {
		var parts []string
		if running > 0 {
			parts = append(parts, fmt.Sprintf("%d running", running))
		}
		if queued > 0 {
			parts = append(parts, fmt.Sprintf("%d queued", queued))
		}
		meta = append(meta, m.spinner.View()+" "+strings.Join(parts, ", "))
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
	start := m.top
	if maxStart := len(m.rows) - height; start > maxStart {
		start = maxStart
	}
	if start < 0 {
		start = 0
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
