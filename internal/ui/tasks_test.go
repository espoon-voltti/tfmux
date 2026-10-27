// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/espoon-voltti/tfmux/internal/domain"
	"github.com/espoon-voltti/tfmux/internal/runner"
	"github.com/espoon-voltti/tfmux/internal/state"
)

func runningApplyTask(m *Model, key string) {
	m.tasks[runner.TaskID(runner.KindApply, key)] = &taskState{
		kind: runner.KindApply, key: key, running: true,
	}
}

func TestCancelAllQueuedKeepsRunningApply(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "a", "b")
	m.addTask(runner.KindPlan, mod.Path+"//a")
	m.addTask(runner.KindPlan, mod.Path+"//b")
	runningApplyTask(m, mod.Path+"//a")

	m.cancelQueuedTasks()

	if n := countTasks(m, runner.KindPlan); n != 0 {
		t.Errorf("queued plans not cleared: %d remain", n)
	}
	if !m.hasTask(runner.KindApply, mod.Path+"//a") {
		t.Error("a running apply must survive bulk cancel of queued tasks")
	}
}

func TestInterruptRunningApplyNeedsConfirm(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	key := mod.Path + "//prod"
	runningApplyTask(m, key)

	keyPress(m, "T")
	keyPress(m, "x") // selects the running apply
	if m.confirmKill == "" {
		t.Fatal("interrupting a running apply should ask for confirmation")
	}
	if !strings.Contains(m.View(), "interrupt running apply?") {
		t.Error("confirmation prompt not shown")
	}

	keyPress(m, "y") // confirm
	if m.confirmKill != "" {
		t.Error("confirmation should clear after y")
	}
	if !strings.Contains(m.status, "interrupting apply") {
		t.Errorf("status = %q", m.status)
	}
	if !m.hasTask(runner.KindApply, key) {
		t.Error("the apply must stay listed until the runner reports it stopped")
	}

	// The runner reports the interrupted apply as canceled with its record
	// marked aborted; the pane must not file that as a harmless cancel.
	m.updateRunnerEvent(runner.Event{
		Kind: runner.KindApply, Key: key, ModulePath: mod.Path, Phase: runner.PhaseCanceled,
		Record: &state.RunRecord{ModulePath: mod.Path, Workspace: "prod", Apply: &state.ApplyRecord{Aborted: true}},
	})
	done := m.done[runner.TaskID(runner.KindApply, key)]
	if done == nil || done.outcome != "✗ interrupted" || !done.failed {
		t.Errorf("done entry = %+v, want a failed \"✗ interrupted\" outcome", done)
	}
	if !strings.Contains(m.status, "interrupted") {
		t.Errorf("status = %q", m.status)
	}
}

// x on a tree row cancels plans but never a running apply: that takes the
// task pane's confirmation.
func TestTreeCancelLeavesRunningApplyAlone(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	key := mod.Path + "//prod"
	runningApplyTask(m, key)
	planTask(m, key, false)
	m.cursor = 2 // workspace row

	keyPress(m, "x")
	if !m.hasTask(runner.KindApply, key) {
		t.Error("running apply canceled from the tree")
	}
	if m.hasTask(runner.KindPlan, key) {
		t.Error("queued plan not canceled")
	}
	if !strings.Contains(m.status, "left alone") {
		t.Errorf("status = %q", m.status)
	}
}

// viewKind follows the latest activity: an apply in flight, else a plan in
// flight, else the recorded apply, else the plan.
func TestViewKindPicksLatestActivity(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	key := mod.Path + "//prod"

	if k := m.viewKind(key); k != runner.KindPlan {
		t.Errorf("nothing yet → %v, want plan", k)
	}
	runningApplyTask(m, key)
	if k := m.viewKind(key); k != runner.KindApply {
		t.Errorf("running apply → %v, want apply", k)
	}
	delete(m.tasks, runner.TaskID(runner.KindApply, key))

	one := 1
	m.runs[key] = &state.RunRecord{ModulePath: mod.Path, Workspace: "prod", Apply: &state.ApplyRecord{ExitCode: &one}}
	if k := m.viewKind(key); k != runner.KindApply {
		t.Errorf("recorded apply → %v, want apply", k)
	}
	planTask(m, key, true)
	if k := m.viewKind(key); k != runner.KindPlan {
		t.Errorf("plan in flight → %v, want plan", k)
	}
}

// enter on a workspace with a running apply follows the apply log.
func TestEnterFollowsRunningApplyLog(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	key := mod.Path + "//prod"
	runningApplyTask(m, key)
	m.cursor = 2 // workspace row

	if cmd := keyPress(m, "enter"); cmd == nil {
		t.Fatal("enter on a running apply should load its log")
	}
	if m.detailFollow != runner.TaskID(runner.KindApply, key) {
		t.Errorf("detailFollow = %q, want the apply task", m.detailFollow)
	}
}

// The pane's selection has to follow its task through a reordering, which
// happens on its own whenever a queued task starts running: running sorts to
// the front, pushing everything queued down a row under a fixed index.
func TestTaskPaneSelectionSurvivesReorder(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "a", "b", "c")
	started := time.Now()
	for i, ws := range []string{"a", "b", "c"} {
		key := mod.Path + "//" + ws
		m.tasks[runner.TaskID(runner.KindPlan, key)] = &taskState{
			kind: runner.KindPlan, key: key, started: started.Add(time.Duration(i) * time.Second),
		}
	}
	wantKey := mod.Path + "//b"

	keyPress(m, "T")
	keyPress(m, "j") // onto b, the second row
	if got := m.taskCursorID; got != runner.TaskID(runner.KindPlan, wantKey) {
		t.Fatalf("setup: selection = %q, want b's task", got)
	}

	// c starting jumps it above the queued a and b, so b is no longer the
	// second row.
	m.tasks[runner.TaskID(runner.KindPlan, mod.Path+"//c")].running = true

	tasks := m.sortedTasks()
	i, ok := m.taskCursorIndex(tasks)
	if !ok || tasks[i].key != wantKey {
		t.Errorf("selection left b's task for %v (ok=%v)", keysOf(tasks), ok)
	}
	if tasks[1].key == wantKey {
		t.Error("the reorder didn't move b, so this proves nothing")
	}
}

// Two tied tasks (same running/priority/started) must sort into the same
// order every time: m.tasks is a map, so the pre-sort order sortedTasks
// builds from it is randomized per call, and without a deterministic final
// tiebreaker sort.Slice (not stable) could legally disagree between two
// calls a keypress apart — the render call and the "act on selected row"
// call — making the highlighted row and the acted-on task different tasks.
func TestSortedTasksIsDeterministicForTiedTasks(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "a", "b", "c")
	started := time.Now()
	for _, ws := range []string{"a", "b", "c"} {
		key := mod.Path + "//" + ws
		m.tasks[runner.TaskID(runner.KindPlan, key)] = &taskState{
			kind: runner.KindPlan, key: key, running: false, started: started,
		}
	}

	first := m.sortedTasks()
	for i := 0; i < 50; i++ {
		got := m.sortedTasks()
		if len(got) != len(first) {
			t.Fatalf("length changed between calls")
		}
		for j := range got {
			if got[j].key != first[j].key {
				t.Fatalf("order changed between calls at %d: %v vs %v", j, keysOf(first), keysOf(got))
			}
		}
	}
}

func keysOf(tasks []*taskState) []string {
	out := make([]string, len(tasks))
	for i, ts := range tasks {
		out[i] = ts.key
	}
	return out
}

func TestTaskPaneSortsRunningFirst(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "a", "b")
	m.addTask(runner.KindEnumerate, mod.Path) // queued, low priority
	runningApplyTask(m, mod.Path+"//a")       // running, high priority

	tasks := m.sortedTasks()
	if len(tasks) != 2 || tasks[0].kind != runner.KindApply || !tasks[0].running {
		t.Errorf("running apply should sort first: %+v", tasks)
	}
}

// finishedPlan runs a plan task for prod through running to the given
// terminal phase and returns its key.
func finishedPlan(t *testing.T, m *Model, mod *domain.Module, phase runner.Phase) string {
	t.Helper()
	enumerated(t, m, mod, "prod")
	key := mod.Path + "//prod"
	m.addTask(runner.KindPlan, key)
	m.updateRunnerEvent(runner.Event{Kind: runner.KindPlan, Key: key, ModulePath: mod.Path, Phase: runner.PhaseRunning})
	m.updateRunnerEvent(runner.Event{Kind: runner.KindPlan, Key: key, ModulePath: mod.Path, Phase: phase, Err: "boom"})
	return key
}

func TestFinishedTaskStaysInPane(t *testing.T) {
	m, mod := fixtureModel(t)
	key := finishedPlan(t, m, mod, runner.PhaseFailed)

	if m.hasTask(runner.KindPlan, key) {
		t.Fatal("a finished task must not count as in flight")
	}
	keyPress(m, "T")
	v := m.View()
	if !strings.Contains(v, "Tasks (0 · 1 done)") {
		t.Errorf("pane header should count the finished task: %q", v)
	}
	if !strings.Contains(v, "✗ failed") || !strings.Contains(v, "prod") {
		t.Errorf("finished task not listed: %q", v)
	}
}

// A queued task canceled before it ever ran has no log, so it isn't kept.
func TestQueuedCancelNotKeptAsFinished(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	key := mod.Path + "//prod"
	m.addTask(runner.KindPlan, key)
	m.updateRunnerEvent(runner.Event{Kind: runner.KindPlan, Key: key, ModulePath: mod.Path, Phase: runner.PhaseCanceled})

	if len(m.recentlyDone()) != 0 {
		t.Errorf("queued-then-canceled task should not be listed: %v", m.done)
	}
}

// While a task is re-run, only its in-flight row is listed, not the previous
// finished one.
func TestRerunHidesPreviousFinishedRow(t *testing.T) {
	m, mod := fixtureModel(t)
	key := finishedPlan(t, m, mod, runner.PhaseDone)
	m.addTask(runner.KindPlan, key)

	if n := len(m.sortedTasks()); n != 1 {
		t.Errorf("rows = %d, want 1 (the re-run only)", n)
	}
}

func TestViewFinishedTaskLogReturnsToPane(t *testing.T) {
	m, mod := fixtureModel(t)
	key := finishedPlan(t, m, mod, runner.PhaseDone)

	keyPress(m, "T")
	cmd := keyPress(m, "enter")
	if cmd == nil {
		t.Fatal("enter on a finished task should load its log")
	}
	m.Update(logMsg{id: runner.TaskID(runner.KindPlan, key), content: "Plan: 1 to add"})
	if m.focus != focusDetail {
		t.Fatalf("focus = %v, want detail", m.focus)
	}
	if m.detailFollow != "" {
		t.Error("a finished task's log must not be followed")
	}
	keyPress(m, "esc")
	if m.focus != focusTasks {
		t.Errorf("esc should return to the task pane, focus = %v", m.focus)
	}
}

func TestCancelOnFinishedTaskIsNoop(t *testing.T) {
	m, mod := fixtureModel(t)
	finishedPlan(t, m, mod, runner.PhaseDone)

	keyPress(m, "T")
	keyPress(m, "x")
	if len(m.recentlyDone()) != 1 {
		t.Error("x on a finished task should leave it listed")
	}
}
