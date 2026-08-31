// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/espoon-voltti/tfmux/internal/runner"
	"github.com/espoon-voltti/tfmux/internal/state"
	"github.com/espoon-voltti/tfmux/internal/tmuxctl"
)

func runningApplyTask(m *Model, key, token string) {
	m.tasks[runner.TaskID(runner.KindApply, key)] = &taskState{
		kind: runner.KindApply, key: key, running: true, token: token,
	}
}

// fakeTmux stands in for a real tmux server: windows exist only if
// registered via addWindow, keyed by the same id/token scheme
// tmuxctl.LaunchApply uses (WindowFor resolves by token, never by a raw id
// alone) — so tests exercise attach/kill exactly as the real resolution path
// does, rather than a mock that answers every call unconditionally.
type fakeTmux struct {
	windows map[string]string // window id -> token
	killed  []string
}

func newFakeTmux() *fakeTmux { return &fakeTmux{windows: map[string]string{}} }

func (f *fakeTmux) addWindow(id, token string) { f.windows[id] = token }

func (f *fakeTmux) run(args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, nil
	}
	switch args[0] {
	case "has-session":
		return nil, nil
	case "list-windows":
		var b strings.Builder
		for id, token := range f.windows {
			fmt.Fprintf(&b, "%s %s\n", id, token)
		}
		return []byte(b.String()), nil
	case "select-window":
		id := args[len(args)-1]
		if _, ok := f.windows[id]; !ok {
			return nil, fmt.Errorf("no such window: %s", id)
		}
		return nil, nil
	case "kill-window":
		id := args[len(args)-1]
		f.killed = append(f.killed, id)
		delete(f.windows, id)
		return nil, nil
	}
	return nil, nil
}

func TestTaskPaneListsTasks(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	m.addTask(runner.KindPlan, mod.Path+"//prod")

	keyPress(m, "T")
	if m.focus != focusTasks {
		t.Fatal("T should open the task pane")
	}
	v := m.View()
	if !strings.Contains(v, "Tasks (1)") {
		t.Errorf("pane header missing: %q", v)
	}
	if !strings.Contains(v, "queued") || !strings.Contains(v, "prod") {
		t.Errorf("queued task not listed: %q", v)
	}

	keyPress(m, "T") // toggle closed
	if m.focus != focusTree {
		t.Error("T should close the pane")
	}
}

func TestCancelSelectedQueuedPlan(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	key := mod.Path + "//prod"
	m.addTask(runner.KindPlan, key)

	keyPress(m, "T")
	keyPress(m, "x")
	if m.hasTask(runner.KindPlan, key) {
		t.Error("x should cancel the selected queued plan")
	}
}

func TestCancelAllQueuedKeepsRunningApply(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "a", "b")
	m.addTask(runner.KindPlan, mod.Path+"//a")
	m.addTask(runner.KindPlan, mod.Path+"//b")
	runningApplyTask(m, mod.Path+"//a", "tok-a")

	m.cancelQueuedTasks()

	if n := countTasks(m, runner.KindPlan); n != 0 {
		t.Errorf("queued plans not cleared: %d remain", n)
	}
	if !m.hasTask(runner.KindApply, mod.Path+"//a") {
		t.Error("a running apply must survive bulk cancel of queued tasks")
	}
}

func TestKillRunningApplyNeedsConfirm(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	key := mod.Path + "//prod"

	ft := newFakeTmux()
	ft.addWindow("@1", "tok-1")
	m.tmux = tmuxctl.NewWithRunner("sess", ft.run)
	runningApplyTask(m, key, "tok-1")

	keyPress(m, "T")
	keyPress(m, "x") // selects the running apply
	if m.confirmKill == "" {
		t.Fatal("killing a running apply should ask for confirmation")
	}
	if len(ft.killed) != 0 {
		t.Error("must not kill before confirmation")
	}

	keyPress(m, "y") // confirm
	if m.confirmKill != "" {
		t.Error("confirmation should clear after y")
	}
	if len(ft.killed) != 1 || ft.killed[0] != "@1" {
		t.Errorf("kill-window not invoked on the window: %v", ft.killed)
	}
}

// liveApplyWindow attaches for a running or failed apply, but not a clean or
// aborted one.
func TestLiveApplyWindow(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	key := mod.Path + "//prod"

	if _, ok := m.liveApplyWindow(key); ok {
		t.Error("no apply → no window")
	}

	runningApplyTask(m, key, "tok-1")
	if w, ok := m.liveApplyWindow(key); !ok || w != "tok-1" {
		t.Errorf("running apply: got %q %v", w, ok)
	}
	delete(m.tasks, runner.TaskID(runner.KindApply, key))

	zero := 0
	m.runs[key] = &state.RunRecord{
		ModulePath: mod.Path, Workspace: "prod",
		Apply: &state.ApplyRecord{WindowID: "@2", Token: "tok-2", ExitCode: &zero},
	}
	if _, ok := m.liveApplyWindow(key); ok {
		t.Error("clean apply → no live window")
	}

	one := 1
	m.runs[key].Apply.ExitCode = &one
	if w, ok := m.liveApplyWindow(key); !ok || w != "tok-2" {
		t.Errorf("failed apply: got %q %v", w, ok)
	}

	m.runs[key].Apply.Aborted = true
	if _, ok := m.liveApplyWindow(key); ok {
		t.Error("aborted apply → no live window")
	}
}

// enter on a workspace with a running apply attaches (tmux) rather than
// opening the plan log.
func TestEnterAttachesToRunningApply(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	key := mod.Path + "//prod"
	m.tmuxOK = true
	ft := newFakeTmux()
	ft.addWindow("@1", "tok-1")
	m.tmux = tmuxctl.NewWithRunner("sess", ft.run)
	runningApplyTask(m, key, "tok-1")
	m.cursor = 2 // workspace row

	cmd := keyPress(m, "enter")
	if cmd == nil {
		t.Fatal("enter on a running apply should produce an attach command")
	}
	if m.detailFollow != "" {
		t.Error("attaching must not start log follow")
	}
	if m.focus == focusDetail {
		t.Error("attaching must not open the log viewer")
	}
}

// Pressing enter on an apply whose token no longer resolves to any window
// (the tmux server restarted, or the window was closed) must say so. Attaching
// anyway is what used to drop the user into an unrelated leftover window and
// present its days-old output as this apply's.
func TestEnterOnGoneApplyWindowSaysSo(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	key := mod.Path + "//prod"
	m.tmuxOK = true
	m.tmux = tmuxctl.NewWithRunner("sess", newFakeTmux().run) // no windows at all
	runningApplyTask(m, key, "tok-gone")
	m.cursor = 2 // workspace row

	if cmd := keyPress(m, "enter"); cmd != nil {
		t.Error("must not attach when the token resolves to no window")
	}
	if !strings.Contains(m.status, "gone") {
		t.Errorf("status doesn't say the window is gone: %q", m.status)
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
	m.addTask(runner.KindEnumerate, mod.Path)    // queued, low priority
	runningApplyTask(m, mod.Path+"//a", "tok-a") // running, high priority

	tasks := m.sortedTasks()
	if len(tasks) != 2 || tasks[0].kind != runner.KindApply || !tasks[0].running {
		t.Errorf("running apply should sort first: %+v", tasks)
	}
}
