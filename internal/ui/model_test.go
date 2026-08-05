// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/espoon-voltti/tfmux/internal/config"
	"github.com/espoon-voltti/tfmux/internal/domain"
	"github.com/espoon-voltti/tfmux/internal/runner"
	"github.com/espoon-voltti/tfmux/internal/state"
	"github.com/espoon-voltti/tfmux/internal/tfexec"
)

// fixtureModel builds a model with one repo / one module, sized, discovered.
func fixtureModel(t *testing.T) (*Model, *domain.Module) {
	t.Helper()
	cfg := config.Default()
	m := NewModel(cfg, state.New(t.TempDir()))
	m.width, m.height = 100, 30
	// Represent a live, settled model: discovery finished and the spinner's tick
	// loop is already running (as it would be after Init), so tickSpinner is a
	// no-op and key handlers return their cmd unwrapped.
	m.discovering = false
	m.spinning = true

	repo := &domain.Repo{Path: "/iac/repo1", Name: "repo1", Git: domain.GitStatus{Branch: "main"}}
	mod := &domain.Module{Repo: repo, Path: "/iac/repo1/envs/prod", RelPath: "envs/prod", TFBin: "terraform"}
	repo.Modules = []*domain.Module{mod}
	m.repos = []*domain.Repo{repo}
	m.reflow()
	return m, mod
}

func enumerated(t *testing.T, m *Model, mod *domain.Module, names ...string) {
	t.Helper()
	m.updateRunnerEvent(runner.Event{
		Kind: runner.KindEnumerate, Key: mod.Path, ModulePath: mod.Path,
		Phase: runner.PhaseDone, Workspaces: names,
	})
}

// planTask marks a plan task in flight (queued or running), as the runner's
// events would.
func planTask(m *Model, key string, running bool) {
	m.tasks[runner.TaskID(runner.KindPlan, key)] = &taskState{kind: runner.KindPlan, running: running}
}

func countTasks(m *Model, kind runner.Kind) int {
	n := 0
	for _, ts := range m.tasks {
		if ts.kind == kind {
			n++
		}
	}
	return n
}

func keyPress(m *Model, k string) tea.Cmd {
	var msg tea.KeyMsg
	switch k {
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case " ":
		msg = tea.KeyMsg{Type: tea.KeySpace}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	}
	_, cmd := m.Update(msg)
	return cmd
}

func TestEnumFinishedPopulatesWorkspaces(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "default", "prod")
	if mod.WorkspaceState != domain.WorkspacesReady || len(mod.Workspaces) != 2 {
		t.Fatalf("module: %+v", mod)
	}
	// rows: repo, module, ws, ws
	if len(m.rows) != 4 {
		t.Errorf("rows = %d, want 4", len(m.rows))
	}
}

func TestEnumErrorShownOnModule(t *testing.T) {
	m, mod := fixtureModel(t)
	m.updateRunnerEvent(runner.Event{
		Kind: runner.KindEnumerate, Key: mod.Path, ModulePath: mod.Path,
		Phase: runner.PhaseFailed, Err: "Error: no credentials",
	})
	if mod.WorkspaceState != domain.WorkspacesError {
		t.Fatalf("state = %v", mod.WorkspaceState)
	}
	view := m.View()
	if !strings.Contains(view, "no credentials") {
		t.Error("error not rendered")
	}
}

// drainPlanFinished waits until n queued plan jobs completed, so async
// runner goroutines stop touching the store before t.TempDir() cleanup.
func drainPlanFinished(t *testing.T, m *Model, n int) {
	t.Helper()
	timeout := time.After(15 * time.Second)
	for n > 0 {
		select {
		case ev := <-m.runner.Events:
			if ev.Kind == runner.KindPlan && ev.Phase.Terminal() {
				n--
			}
		case <-timeout:
			t.Fatal("timed out draining plan events")
		}
	}
}

func TestPlanKeyQueuesCursorWorkspace(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	m.cursor = 2 // workspace row
	keyPress(m, "p")
	key := mod.Path + "//prod"
	if !m.hasTask(runner.KindPlan, key) {
		t.Error("plan not queued for cursor workspace")
	}
	drainPlanFinished(t, m, 1)
}

func TestPlanKeyOnRepoQueuesAllWorkspaces(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "default", "prod")
	m.cursor = 0 // repo row
	keyPress(m, "p")
	if n := countTasks(m, runner.KindPlan); n != 2 {
		t.Errorf("plan tasks = %d, want 2", n)
	}
	drainPlanFinished(t, m, 2)
}

// drainOutputFinished waits until n queued output jobs completed, so async
// runner goroutines stop touching the store before t.TempDir() cleanup.
func drainOutputFinished(t *testing.T, m *Model, n int) {
	t.Helper()
	timeout := time.After(15 * time.Second)
	for n > 0 {
		select {
		case ev := <-m.runner.Events:
			if ev.Kind == runner.KindOutput && ev.Phase.Terminal() {
				n--
			}
		case <-timeout:
			t.Fatal("timed out draining output events")
		}
	}
}

func TestOutputKeyQueuesCursorWorkspace(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	m.cursor = 2 // workspace row
	keyPress(m, "o")
	key := mod.Path + "//prod"
	if !m.hasTask(runner.KindOutput, key) {
		t.Error("output not queued for cursor workspace")
	}
	drainOutputFinished(t, m, 1)
}

func TestOutputKeyIgnoredOnNonWorkspaceRow(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	m.cursor = 1 // module row
	keyPress(m, "o")
	if countTasks(m, runner.KindOutput) != 0 {
		t.Error("output queued for a non-workspace row")
	}
}

func TestOutputFinishedUpdatesStatusOnFailure(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	key := mod.Path + "//prod"
	m.tasks[runner.TaskID(runner.KindOutput, key)] = &taskState{kind: runner.KindOutput, running: true}
	m.updateRunnerEvent(runner.Event{
		Kind: runner.KindOutput, Key: key, ModulePath: mod.Path,
		Phase: runner.PhaseFailed, Err: "Error: no state",
	})
	if m.hasTask(runner.KindOutput, key) {
		t.Error("output task not cleared")
	}
	if !strings.Contains(m.status, "output failed") {
		t.Errorf("status = %q", m.status)
	}
}

func TestPlanFinishedUpdatesStatusAndBadge(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	key := mod.Path + "//prod"
	planTask(m, key, true)
	rec := &state.RunRecord{
		ModulePath: mod.Path, Workspace: "prod",
		PlanFinished: time.Now(), PlanExitCode: tfexec.PlanChanges,
		Summary: state.ChangeSummary{Add: 2, Change: 1},
	}
	m.updateRunnerEvent(runner.Event{
		Kind: runner.KindPlan, Key: key, ModulePath: mod.Path,
		Phase: runner.PhaseDone, Record: rec,
	})
	if m.hasTask(runner.KindPlan, key) {
		t.Error("plan task not cleared")
	}
	if !m.planFiles[key] {
		t.Error("plan file flag not set for changes")
	}
	view := m.View()
	if !strings.Contains(view, "+2 ~1 -0") {
		t.Errorf("changes badge missing from view")
	}
}

func TestStaleBadge(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	key := mod.Path + "//prod"
	m.runs[key] = &state.RunRecord{
		ModulePath: mod.Path, Workspace: "prod",
		PlanFinished: time.Now(), PlanExitCode: tfexec.PlanChanges,
		GitHead: "aaa", DirtyHash: "bbb",
	}
	m.planFiles[key] = true
	m.fingerprints[mod.Path] = "aaa|bbb"
	if strings.Contains(m.View(), "STALE") {
		t.Error("fresh plan flagged stale")
	}
	m.fingerprints[mod.Path] = "ccc|ddd"
	if !strings.Contains(m.View(), "STALE") {
		t.Error("stale plan not flagged")
	}
}

func TestPlanErrorKindShownInStatus(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	key := mod.Path + "//prod"
	m.runs[key] = &state.RunRecord{
		ModulePath: mod.Path, Workspace: "prod",
		PlanFinished: time.Now(), PlanExitCode: tfexec.PlanError,
		PlanErrorKind: string(tfexec.PlanErrorStateLocked),
	}
	if !strings.Contains(m.View(), "plan error (state locked)") {
		t.Error("state-locked plan error kind not rendered")
	}
}

func TestFooterHelpIsContextSensitive(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	key := mod.Path + "//prod"

	view := m.View() // tree focus, the default
	if !strings.Contains(view, "plan marked/cursor") {
		t.Error("tree footer missing plan hint")
	}
	if strings.Contains(view, "cancel all queued") {
		t.Error("tree footer shows a task-pane-only hint")
	}

	planTask(m, key, true) // give the task pane something to show
	m.focus = focusTasks
	view = m.View()
	if !strings.Contains(view, "cancel/kill task") || !strings.Contains(view, "cancel all queued") {
		t.Error("task pane footer missing cancel hints")
	}
	if strings.Contains(view, "toggle ignore") {
		t.Error("task pane footer shows a tree-only hint")
	}
	if strings.Contains(view, "enter view/attach · x cancel/kill · X cancel all queued · esc/T close") {
		t.Error("old inline task-pane hint text still rendered in the pane body")
	}

	m.focus = focusDetail
	view = m.View()
	if !strings.Contains(view, "page up") {
		t.Error("detail footer missing scroll hint")
	}
	if strings.Contains(view, "plan marked/cursor") {
		t.Error("detail footer shows a tree-only hint")
	}
}

func TestIgnoreToggleHidesAndPersists(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	m.cursor = 1 // module row
	cmd := keyPress(m, "i")
	if cmd == nil {
		t.Fatal("expected save cmd")
	}
	if msg := cmd(); msg.(savedMsg).err != nil {
		t.Fatal(msg.(savedMsg).err)
	}
	if len(m.rows) != 1 { // repo only
		t.Errorf("rows after ignore = %d", len(m.rows))
	}
	ig, err := m.store.LoadIgnore()
	if err != nil || !ig[mod.Path] {
		t.Errorf("ignore not persisted: %v %v", ig, err)
	}
	// Z reveals it again
	keyPress(m, "Z")
	if len(m.rows) != 2 {
		t.Errorf("rows with showIgnored = %d", len(m.rows))
	}
}

func TestApplyGuards(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	m.cursor = 2
	m.tmuxOK = false
	keyPress(m, "A")
	if !strings.Contains(m.status, "tmux not found") {
		t.Errorf("status = %q", m.status)
	}
	m.tmuxOK = true
	keyPress(m, "A")
	if !strings.Contains(m.status, "nothing to apply") {
		t.Errorf("status = %q", m.status)
	}
	key := mod.Path + "//prod"
	m.runs[key] = &state.RunRecord{
		ModulePath: mod.Path, Workspace: "prod",
		PlanFinished: time.Now(), PlanExitCode: tfexec.PlanChanges,
		GitHead: "aaa", DirtyHash: "bbb",
	}
	m.planFiles[key] = false
	keyPress(m, "A")
	if !strings.Contains(m.status, "expired or discarded") {
		t.Errorf("status = %q", m.status)
	}
	m.planFiles[key] = true
	m.fingerprints[mod.Path] = "zzz|zzz"
	keyPress(m, "A")
	if !strings.Contains(m.status, "STALE") {
		t.Errorf("status = %q", m.status)
	}
}

// changedPlan records a fresh plan with outstanding changes and a plan file on
// disk, so the workspace is applyable.
func changedPlan(m *Model, mod *domain.Module, ws string) {
	key := mod.Path + "//" + ws
	m.runs[key] = &state.RunRecord{
		ModulePath: mod.Path, Workspace: ws,
		PlanFinished: time.Now(), PlanExitCode: tfexec.PlanChanges,
		GitHead: "aaa", DirtyHash: "bbb",
	}
	m.planFiles[key] = true
	m.fingerprints[mod.Path] = "aaa|bbb"
}

func TestMassApplyConfirmsThenSkipsIneligible(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "default", "prod", "staging")
	changedPlan(m, mod, "prod")
	changedPlan(m, mod, "staging")
	// "default" has no plan with changes — it must be excluded.

	m.cursor = 1 // module row
	keyPress(m, "A")
	if len(m.confirmApply) != 2 {
		t.Fatalf("confirmApply targets = %d, want 2 (prod, staging)", len(m.confirmApply))
	}
	if !strings.Contains(m.View(), "apply 2 plan(s) with changes") {
		t.Errorf("confirmation prompt missing from view")
	}
	// Confirmation gates the launch: no apply task yet.
	if countTasks(m, runner.KindApply) != 0 {
		t.Error("mass apply should not enqueue before confirmation")
	}
}

func TestMassApplyCanceledLeavesNothingQueued(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod", "staging")
	changedPlan(m, mod, "prod")
	changedPlan(m, mod, "staging")

	m.cursor = 0 // repo row
	keyPress(m, "A")
	if len(m.confirmApply) != 2 {
		t.Fatalf("confirmApply targets = %d, want 2", len(m.confirmApply))
	}
	keyPress(m, "n")
	if len(m.confirmApply) != 0 {
		t.Error("n should clear the pending mass apply")
	}
	if countTasks(m, runner.KindApply) != 0 {
		t.Error("canceling must not enqueue any apply")
	}
	if !strings.Contains(m.status, "canceled") {
		t.Errorf("status = %q", m.status)
	}
}

func TestMassApplyNothingEligible(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	// prod was planned but is clean — nothing to apply.
	m.runs[mod.Path+"//prod"] = &state.RunRecord{
		ModulePath: mod.Path, Workspace: "prod",
		PlanFinished: time.Now(), PlanExitCode: tfexec.PlanClean,
	}
	m.cursor = 1 // module row
	keyPress(m, "A")
	if len(m.confirmApply) != 0 {
		t.Error("clean plans should not stage a mass apply")
	}
	if !strings.Contains(m.status, "nothing to apply") {
		t.Errorf("status = %q", m.status)
	}
}

func TestApplyDoneSuccessDiscardsPlan(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	key := mod.Path + "//prod"
	m.runs[key] = &state.RunRecord{
		ModulePath: mod.Path, Workspace: "prod",
		PlanExitCode: tfexec.PlanChanges,
		Apply:        &state.ApplyRecord{Started: time.Now(), WindowID: "@1"},
	}
	m.planFiles[key] = true
	m.tasks[runner.TaskID(runner.KindApply, key)] = &taskState{kind: runner.KindApply, running: true}
	zero := 0
	m.updateRunnerEvent(runner.Event{
		Kind: runner.KindApply, Key: key, ModulePath: mod.Path,
		Phase: runner.PhaseDone, ApplyExit: &zero,
	})
	rec := m.runs[key]
	if rec.Apply.ExitCode == nil || *rec.Apply.ExitCode != 0 || rec.Apply.Finished == nil {
		t.Errorf("apply record: %+v", rec.Apply)
	}
	if m.planFiles[key] {
		t.Error("plan file flag should clear after successful apply")
	}
	if m.hasTask(runner.KindApply, key) {
		t.Error("apply task not cleared")
	}
}

func TestApplyDoneVanishedWindow(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	key := mod.Path + "//prod"
	m.runs[key] = &state.RunRecord{
		ModulePath: mod.Path, Workspace: "prod",
		PlanExitCode: tfexec.PlanChanges,
		Apply:        &state.ApplyRecord{Started: time.Now(), WindowID: "@1"},
	}
	m.tasks[runner.TaskID(runner.KindApply, key)] = &taskState{kind: runner.KindApply, running: true}
	m.updateRunnerEvent(runner.Event{
		Kind: runner.KindApply, Key: key, ModulePath: mod.Path,
		Phase: runner.PhaseDone, Aborted: true,
	})
	if !m.runs[key].Apply.Aborted {
		t.Error("vanished window should mark apply aborted")
	}
}

func TestQuitConfirmWhilePlanning(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	planTask(m, mod.Path+"//prod", true)
	keyPress(m, "q")
	if !m.confirmQuit {
		t.Fatal("expected quit confirmation")
	}
	keyPress(m, "n")
	if m.confirmQuit {
		t.Error("n should cancel quit")
	}
	keyPress(m, "q")
	cmd := keyPress(m, "y")
	if cmd == nil {
		t.Fatal("y should quit")
	}
}

func TestQAndEscAreInterchangeableForBack(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	key := mod.Path + "//prod"

	for _, back := range []string{"q", "esc"} {
		planTask(m, key, true)
		m.focus = focusTasks
		if cmd := keyPress(m, back); cmd != nil {
			t.Errorf("%q in task pane should not quit", back)
		}
		if m.focus != focusTree {
			t.Errorf("%q in task pane should return to tree, got focus %v", back, m.focus)
		}

		m.focus = focusDetail
		if cmd := keyPress(m, back); cmd != nil {
			t.Errorf("%q in detail view should not quit", back)
		}
		if m.focus != focusTree {
			t.Errorf("%q in detail view should return to tree, got focus %v", back, m.focus)
		}
	}
}

func TestQClosesHelpInsteadOfQuitting(t *testing.T) {
	m, _ := fixtureModel(t)
	keyPress(m, "?")
	if !m.showHelp {
		t.Fatal("expected help overlay to open")
	}
	cmd := keyPress(m, "q")
	if cmd != nil {
		t.Fatal("q should close the help overlay, not quit")
	}
	if m.showHelp {
		t.Error("expected help overlay to close")
	}
	if m.confirmQuit {
		t.Error("q should not trigger quit confirmation while help is open")
	}
}

func TestFilter(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "production", "staging")
	keyPress(m, "/")
	if m.focus != focusFilter {
		t.Fatal("/ should focus filter")
	}
	for _, r := range "stag" {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	// repo + module + staging only
	if len(m.rows) != 3 {
		t.Errorf("filtered rows = %d: %v", len(m.rows), m.filterText)
	}
}
