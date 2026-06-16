// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package ui

import (
	"strings"
	"testing"

	"github.com/japsu/tfmux/internal/runner"
	"github.com/japsu/tfmux/internal/state"
	"github.com/japsu/tfmux/internal/tmuxctl"
)

func runningApplyTask(m *Model, key, windowID string) {
	m.tasks[runner.TaskID(runner.KindApply, key)] = &taskState{
		kind: runner.KindApply, key: key, running: true, windowID: windowID,
	}
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
	runningApplyTask(m, mod.Path+"//a", "@1")

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

	var killed []string
	m.tmux = tmuxctl.NewWithRunner("sess", func(args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "kill-window" {
			killed = append(killed, args[len(args)-1])
		}
		return nil, nil
	})
	runningApplyTask(m, key, "@1")

	keyPress(m, "T")
	keyPress(m, "x") // selects the running apply
	if m.confirmKill == "" {
		t.Fatal("killing a running apply should ask for confirmation")
	}
	if len(killed) != 0 {
		t.Error("must not kill before confirmation")
	}

	keyPress(m, "y") // confirm
	if m.confirmKill != "" {
		t.Error("confirmation should clear after y")
	}
	if len(killed) != 1 || killed[0] != "@1" {
		t.Errorf("kill-window not invoked on the window: %v", killed)
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

	runningApplyTask(m, key, "@1")
	if w, ok := m.liveApplyWindow(key); !ok || w != "@1" {
		t.Errorf("running apply: got %q %v", w, ok)
	}
	delete(m.tasks, runner.TaskID(runner.KindApply, key))

	zero := 0
	m.runs[key] = &state.RunRecord{
		ModulePath: mod.Path, Workspace: "prod",
		Apply: &state.ApplyRecord{WindowID: "@2", ExitCode: &zero},
	}
	if _, ok := m.liveApplyWindow(key); ok {
		t.Error("clean apply → no live window")
	}

	one := 1
	m.runs[key].Apply.ExitCode = &one
	if w, ok := m.liveApplyWindow(key); !ok || w != "@2" {
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
	m.tmux = tmuxctl.NewWithRunner("sess", func(args ...string) ([]byte, error) { return nil, nil })
	runningApplyTask(m, key, "@1")
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

func TestTaskPaneSortsRunningFirst(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "a", "b")
	m.addTask(runner.KindEnumerate, mod.Path) // queued, low priority
	runningApplyTask(m, mod.Path+"//a", "@1") // running, high priority

	tasks := m.sortedTasks()
	if len(tasks) != 2 || tasks[0].kind != runner.KindApply || !tasks[0].running {
		t.Errorf("running apply should sort first: %+v", tasks)
	}
}
