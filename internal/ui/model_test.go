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
	"github.com/espoon-voltti/tfmux/internal/deps"
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

// drain waits until n tasks of kind reached a terminal event, so async
// runner goroutines stop touching the store before t.TempDir() cleanup.
func drain(t *testing.T, m *Model, kind runner.Kind, n int) {
	t.Helper()
	timeout := time.After(15 * time.Second)
	for n > 0 {
		select {
		case ev := <-m.runner.Events:
			if ev.Kind == kind && ev.Phase.Terminal() {
				n--
			}
		case <-timeout:
			t.Fatalf("timed out draining %s events", kind)
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
	drain(t, m, runner.KindPlan, 1)
}

func TestPlanKeyOnRepoQueuesAllWorkspaces(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "default", "prod")
	m.cursor = 0 // repo row
	keyPress(m, "p")
	if n := countTasks(m, runner.KindPlan); n != 2 {
		t.Errorf("plan tasks = %d, want 2", n)
	}
	drain(t, m, runner.KindPlan, 2)
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
	drain(t, m, runner.KindOutput, 1)
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

// The title bar's "need plan" count must track plan results and fingerprints
// as they arrive, not only tree rebuilds.
func TestNeedPlanCountFollowsPlanResults(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	key := mod.Path + "//prod"
	if m.nNeedPlan != 1 {
		t.Fatalf("nNeedPlan = %d before any plan, want 1", m.nNeedPlan)
	}
	rec := &state.RunRecord{ModulePath: mod.Path, Workspace: "prod", PlanFinished: time.Now(), PlanExitCode: tfexec.PlanClean, Fingerprint: "f1"}
	m.updateRunnerEvent(runner.Event{Kind: runner.KindPlan, Key: key, ModulePath: mod.Path, Phase: runner.PhaseDone, Record: rec})
	if m.nNeedPlan != 0 {
		t.Errorf("nNeedPlan = %d after a clean plan, want 0", m.nNeedPlan)
	}
	m.Update(fingerprintMsg{modulePath: mod.Path, fingerprint: "f2"})
	if m.nNeedPlan != 1 {
		t.Errorf("nNeedPlan = %d after the module changed, want 1", m.nNeedPlan)
	}
	m.Update(fingerprintMsg{modulePath: mod.Path, fingerprint: "f1"})
	rec = &state.RunRecord{ModulePath: mod.Path, Workspace: "prod", PlanFinished: time.Now(), PlanExitCode: tfexec.PlanChanges, Fingerprint: "f1"}
	m.updateRunnerEvent(runner.Event{Kind: runner.KindPlan, Key: key, ModulePath: mod.Path, Phase: runner.PhaseDone, Record: rec})
	if m.nNeedPlan != 0 {
		t.Errorf("nNeedPlan = %d with a ready plan, want 0", m.nNeedPlan)
	}
	m.cursor = 2
	keyPress(m, "d")
	if m.nNeedPlan != 1 {
		t.Errorf("nNeedPlan = %d after discarding the plan, want 1", m.nNeedPlan)
	}
}

func TestStaleBadge(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	key := mod.Path + "//prod"
	m.runs[key] = &state.RunRecord{
		ModulePath: mod.Path, Workspace: "prod",
		PlanFinished: time.Now(), PlanExitCode: tfexec.PlanChanges,
		Fingerprint: "aaa|bbb",
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
		Fingerprint: "aaa|bbb",
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
		Fingerprint: "aaa|bbb",
	}
	m.planFiles[key] = true
	m.fingerprints[mod.Path] = "aaa|bbb"
}

func TestMassApplyConfirmsThenSkipsIneligible(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "default", "prod", "staging")
	changedPlan(m, mod, "prod")
	changedPlan(m, mod, "staging")
	// "default" has no plan with changes: it must be excluded.

	m.cursor = 1 // module row
	keyPress(m, "A")
	if len(m.confirmApply) != 2 {
		t.Fatalf("confirmApply targets = %d, want 2 (prod, staging)", len(m.confirmApply))
	}
	if !strings.Contains(m.View(), "apply 2 plan(s) with changes") {
		t.Errorf("confirmation prompt missing from view")
	}
	if countTasks(m, runner.KindApply) != 0 {
		t.Error("mass apply should not enqueue before confirmation")
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

// A background workspace enumeration for one module inserts rows above a
// cursor sitting in a different module — reflow must keep the cursor on the
// node it was on, not merely clamp its index to the (now larger) row count.
// This is the tree half of the tfmux focus-mismatch bug: attaching to what
// looked like the selected workspace could silently attach to a different
// repo's row instead.
func TestReflowPreservesCursorAcrossBackgroundRowChange(t *testing.T) {
	m, mod1 := fixtureModel(t)
	repo := mod1.Repo
	mod2 := &domain.Module{Repo: repo, Path: "/iac/repo1/envs/staging", RelPath: "envs/staging", TFBin: "terraform"}
	repo.Modules = append(repo.Modules, mod2)
	enumerated(t, m, mod2, "staging")

	target := workspaceRowID(mod2.Path + "//staging")
	m.focusNode(target)
	if r, ok := m.currentRow(); !ok || r.rowID() != target {
		t.Fatalf("setup: cursor not on %s (got %+v, ok=%v)", target, r, ok)
	}

	// mod1 sorts before mod2 in the tree; finishing its enumeration inserts
	// rows above the cursor exactly the way a real background completion
	// would (this goes through enumerateDone -> reflow, not a bare reflow
	// call, so it exercises the real code path).
	enumerated(t, m, mod1, "dev", "prod")

	if r, ok := m.currentRow(); !ok || r.rowID() != target {
		t.Errorf("cursor drifted off %s to %+v (ok=%v)", target, r, ok)
	}
}

// A repo whose root is itself a root module (RelPath ".") gives its repo row
// and its module row the same nodeKey, so re-anchoring the cursor by nodeKey
// finds the repo row first and walks the cursor up off the module row on every
// reflow.
func TestReflowKeepsCursorOnRepoRootModuleRow(t *testing.T) {
	m, _ := fixtureModel(t)
	repo := &domain.Repo{Path: "/iac/repo2", Name: "repo2", Git: domain.GitStatus{Branch: "main"}}
	mod := &domain.Module{Repo: repo, Path: repo.Path, RelPath: ".", TFBin: "terraform"}
	repo.Modules = []*domain.Module{mod}
	m.repos = append(m.repos, repo)
	m.reflow()

	m.focusNode(moduleRowID(mod.Path))
	if r, ok := m.currentRow(); !ok || r.kind != rowModule {
		t.Fatalf("setup: cursor not on the module row (got %+v, ok=%v)", r, ok)
	}

	enumerated(t, m, mod, "prod")

	if r, ok := m.currentRow(); !ok || r.kind != rowModule {
		t.Errorf("cursor left the repo-root module row for %+v (ok=%v)", r, ok)
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

// setFilter applies a `/` filter as if typed and confirmed with enter.
func setFilter(m *Model, text string) {
	keyPress(m, "/")
	for _, r := range text {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	keyPress(m, "enter")
}

// p on a module or repo row plans only the workspaces the filter leaves
// visible; a filter matching the module itself keeps all of its workspaces.
func TestPlanKeyHonorsFilter(t *testing.T) {
	cases := []struct {
		name   string
		filter string
		cursor int
		want   []string
	}{
		{"module row, workspace filter", "blue", 1, []string{"blue"}},
		{"repo row, workspace filter", "gre", 0, []string{"green"}},
		{"module row, module filter", "envs/", 1, []string{"default", "blue", "green"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, mod := fixtureModel(t)
			enumerated(t, m, mod, "default", "blue", "green")
			setFilter(m, c.filter)
			m.cursor = c.cursor
			keyPress(m, "p")
			if n := countTasks(m, runner.KindPlan); n != len(c.want) {
				t.Errorf("plan tasks = %d, want %d: %v", n, len(c.want), m.tasks)
			}
			for _, ws := range c.want {
				if !m.hasTask(runner.KindPlan, mod.Path+"//"+ws) {
					t.Errorf("plan for %s not queued", ws)
				}
			}
			drain(t, m, runner.KindPlan, len(c.want))
		})
	}
}

func TestMassApplyOnlyTargetsFilteredWorkspaces(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "blue", "green")
	changedPlan(m, mod, "blue")
	changedPlan(m, mod, "green")
	setFilter(m, "blue")
	m.cursor = 0 // repo row
	keyPress(m, "A")
	if len(m.confirmApply) != 1 || m.confirmApply[0].Name != "blue" {
		t.Fatalf("confirmApply = %v, want only blue", m.confirmApply)
	}
	if !strings.Contains(m.View(), `matching "blue"`) {
		t.Error("confirmation prompt should mention the active filter")
	}
}

func TestCleanPlanNeedsPlanAfterChange(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	key := mod.Path + "//prod"
	m.runs[key] = &state.RunRecord{
		ModulePath: mod.Path, Workspace: "prod",
		PlanFinished: time.Now(), PlanExitCode: tfexec.PlanClean,
		Fingerprint: "aaa|bbb",
	}
	m.fingerprints[mod.Path] = "aaa|bbb"
	m.reflow()
	if v := m.View(); strings.Contains(v, "needs plan") || strings.Contains(v, "need plan") {
		t.Error("current clean plan flagged as needing a plan")
	}
	m.fingerprints[mod.Path] = "ccc|ddd"
	m.reflow()
	v := m.View()
	if !strings.Contains(v, "✓ clean") || !strings.Contains(v, "needs plan") {
		t.Errorf("changed clean plan should keep its result and add the badge, got:\n%s", v)
	}
	if !strings.Contains(v, "1 need plan") {
		t.Errorf("title bar should count it, got:\n%s", v)
	}
}

func TestModuleBehindBadgeAndRefreshJoin(t *testing.T) {
	m, mod := fixtureModel(t)
	mod.Deps = &domain.Deps{Git: []domain.GitRef{{Repo: "github.com/org/shared", Subdir: "modules/waf", Ref: "v16", Via: "waf"}}}
	enumerated(t, m, mod, "prod")
	shared := &domain.Repo{Path: "/iac/shared", Name: "shared"}
	m.repos = append(m.repos, shared)
	m.reflow()

	// The shared repo's git status arrives with a remote the index knows:
	// a pin check is emitted and a live refresh waits for it.
	m.refreshGen, m.refreshLeft = 7, 1
	_, cmd := m.Update(gitStatusMsg{repoPath: shared.Path, status: domain.GitStatus{Branch: "main", RemoteURL: "git@github.com:org/shared.git"}, gen: 7})
	if cmd == nil {
		t.Fatal("expected a pin check command")
	}
	if m.refreshLeft != 1 {
		t.Errorf("refreshLeft = %d: the check must replace the git-status slot it was spawned from", m.refreshLeft)
	}
	_, cmd = m.Update(gitStatusMsg{repoPath: shared.Path, status: domain.GitStatus{Branch: "main", RemoteURL: "https://github.com/org/unrelated"}, gen: 0})
	if cmd != nil {
		t.Error("no pins point at that repo: no command expected")
	}

	m.Update(behindMsg{repoPath: shared.Path, behind: map[string]deps.Behind{"github.com/org/shared//modules/waf@v16": {Ref: "v16", NewerTag: "v18"}}, gen: 7})
	v := m.View()
	if !strings.Contains(v, "↑ waf v16→v18") {
		t.Errorf("module row should show the behind badge:\n%s", v)
	}
	if !strings.Contains(v, "1 modules behind") {
		t.Errorf("title bar should count it:\n%s", v)
	}
	if m.refreshLeft != 0 || m.status != "refreshed" {
		t.Errorf("refresh should complete after the check: left=%d status=%q", m.refreshLeft, m.status)
	}
}

// stateFixture wires base (holding a backend) and service (reading it in the
// same workspace) into m, with clean run records for service's prod and
// staging.
func stateFixture(t *testing.T, m *Model, base *domain.Module) (service *domain.Module) {
	t.Helper()
	addr := domain.StateAddress{Backend: "s3", Bucket: "tf", Key: "base.tfstate"}
	base.Deps = &domain.Deps{Backend: &addr}
	enumerated(t, m, base, "prod", "staging")
	service = &domain.Module{Repo: base.Repo, Path: "/iac/repo1/envs/service", RelPath: "envs/service", TFBin: "terraform"}
	service.Deps = &domain.Deps{RemoteStates: []domain.RemoteStateRef{{Name: "base", StateAddress: addr, WorkspaceKind: domain.WorkspaceSame}}}
	base.Repo.Modules = append(base.Repo.Modules, service)
	enumerated(t, m, service, "prod", "staging")
	for _, ws := range []string{"prod", "staging"} {
		key := service.Path + "//" + ws
		m.runs[key] = &state.RunRecord{ModulePath: service.Path, Workspace: ws, PlanFinished: time.Now(), PlanExitCode: tfexec.PlanClean, Fingerprint: "f"}
	}
	m.fingerprints[service.Path] = "f"
	m.reflow()
	return service
}

// applied finishes an apply of ws whose plan changed outputs.
func applied(m *Model, mod *domain.Module, ws string) {
	key := mod.Path + "//" + ws
	m.runs[key] = &state.RunRecord{ModulePath: mod.Path, Workspace: ws, PlanExitCode: tfexec.PlanChanges, OutputsChanged: true, Apply: &state.ApplyRecord{Started: time.Now()}}
	exit := 0
	m.updateRunnerEvent(runner.Event{Kind: runner.KindApply, Key: key, ModulePath: mod.Path, Phase: runner.PhaseDone, ApplyExit: &exit})
}

func TestUpstreamApplyInvalidatesMappedDownstream(t *testing.T) {
	m, base := fixtureModel(t)
	service := stateFixture(t, m, base)
	applied(m, base, "prod")
	if m.runs[service.Path+"//prod"].Invalidated == nil {
		t.Error("service//prod reads base//prod and should be invalidated")
	}
	if m.runs[service.Path+"//staging"].Invalidated != nil {
		t.Error("service//staging reads base//staging and must be left alone")
	}
	if !strings.Contains(m.status, "1 downstream workspace(s) need plan") {
		t.Errorf("status = %q", m.status)
	}
	m.reflow()
	v := m.View()
	// 2: service//prod plus base//staging, which was never planned.
	if !strings.Contains(v, "needs plan") || !strings.Contains(v, "2 need plan") {
		t.Errorf("downstream should show as needing a plan:\n%s", v)
	}
	// Re-applying does not stack a second invalidation.
	first := m.runs[service.Path+"//prod"].Invalidated
	applied(m, base, "prod")
	if m.runs[service.Path+"//prod"].Invalidated != first {
		t.Error("an already-invalidated record must not be re-marked")
	}
}

func TestInvalidatedPlanIsNotApplyable(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	changedPlan(m, mod, "prod")
	m.runs[mod.Path+"//prod"].Invalidated = &state.Invalidation{By: "repo1/envs/base//prod", At: time.Now()}
	m.reflow()
	keyPress(m, "j")
	keyPress(m, "j")
	keyPress(m, "A")
	if !strings.Contains(m.status, "STALE") {
		t.Errorf("status = %q", m.status)
	}
}

// A pin edited in the sources since discovery (v16 → v18 by hand) is picked
// up by r: the rescan replaces the module's deps, the index resolves the new
// ref, and the badge computed against the old ref goes.
func TestRefreshRescansEditedPins(t *testing.T) {
	m, mod := fixtureModel(t)
	mod.Deps = &domain.Deps{Git: []domain.GitRef{{Repo: "github.com/org/shared", Subdir: "modules/waf", Ref: "v16", Via: "waf"}}}
	enumerated(t, m, mod, "prod")
	m.reflow()
	m.behind["github.com/org/shared//modules/waf@v16"] = deps.Behind{Ref: "v16", NewerTag: "v18"}
	m.recountTree()
	if !strings.Contains(m.View(), "↑ waf v16→v18") {
		t.Fatal("precondition: badge shown for the old pin")
	}

	m.refresh()
	m.Update(depsScannedMsg{gen: m.refreshGen, deps: map[string]*domain.Deps{
		mod.Path: {Git: []domain.GitRef{{Repo: "github.com/org/shared", Subdir: "modules/waf", Ref: "v18", Via: "waf"}}},
	}})

	refs := m.index.RefsTo("github.com/org/shared")
	if len(refs) != 1 || refs[0].Ref != "v18" {
		t.Errorf("index should resolve the rescanned ref: %+v", refs)
	}
	if v := m.View(); strings.Contains(v, "v16→v18") {
		t.Errorf("badge for the old pin should be gone:\n%s", v)
	}
}
