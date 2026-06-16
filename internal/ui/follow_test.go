// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package ui

import (
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/espoon-voltti/tfmux/internal/runner"
)

// writePlanLog seeds a workspace's plan.log on disk.
func writePlanLog(t *testing.T, m *Model, mod, ws, content string) {
	t.Helper()
	path, err := m.store.PlanLogPath(mod, ws) // also creates the workspace dir
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Opening the log of a workspace whose plan is running follows it live:
// detail focuses, shows current output, and keeps a tail tick going.
func TestViewFollowsRunningPlan(t *testing.T) {
	m, mod := fixtureModel(t)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30}) // give the detail viewport real dims
	enumerated(t, m, mod, "prod")
	key := mod.Path + "//prod"
	m.tasks[runner.TaskID(runner.KindPlan, key)] = &taskState{kind: runner.KindPlan, key: key, running: true}
	writePlanLog(t, m, mod.Path, "prod", "Refreshing state...\n")
	m.cursor = 2 // workspace row

	cmd := keyPress(m, "enter")
	if want := runner.TaskID(runner.KindPlan, key); m.detailFollow != want {
		t.Fatalf("expected to follow %q, got %q", want, m.detailFollow)
	}
	if cmd == nil {
		t.Fatal("expected a log-load command")
	}
	// run the load, feed the result back
	_, tick := m.Update(cmd())
	if m.focus != focusDetail {
		t.Error("detail viewport should be focused")
	}
	if !strings.Contains(m.View(), "Refreshing state") {
		t.Error("live log content not shown")
	}
	if tick == nil {
		t.Error("expected a follow tick while the plan is still running")
	}
}

// Once the task is no longer in flight, the next read stops following.
func TestFollowStopsWhenPlanDone(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	key := mod.Path + "//prod"
	id := runner.TaskID(runner.KindPlan, key)
	m.detailFollow = id // following, but no plan task in flight anymore

	_, tick := m.updateLog(logMsg{id: id, content: "Plan: 1 to add, 0 to change, 0 to destroy."})

	if m.detailFollow != "" {
		t.Error("follow should stop once the plan task is gone")
	}
	if tick != nil {
		t.Error("no further tick should be scheduled after the plan finishes")
	}
}

// enter on a module whose enumeration is running follows the enumerate log.
func TestViewFollowsRunningEnumerate(t *testing.T) {
	m, mod := fixtureModel(t)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.tasks[runner.TaskID(runner.KindEnumerate, mod.Path)] = &taskState{kind: runner.KindEnumerate, key: mod.Path, running: true}
	path, _ := m.store.ModuleLogPath(mod.Path, "enumerate")
	if err := os.WriteFile(path, []byte("Listing workspaces...\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.cursor = 1 // module row (repo, module)

	cmd := keyPress(m, "enter")
	if want := runner.TaskID(runner.KindEnumerate, mod.Path); m.detailFollow != want {
		t.Fatalf("expected to follow %q, got %q", want, m.detailFollow)
	}
	_, tick := m.Update(cmd())
	if !strings.Contains(m.View(), "Listing workspaces") {
		t.Error("live enumerate log not shown")
	}
	if tick == nil {
		t.Error("expected a follow tick while enumeration runs")
	}
}

// A completed plan (no task) opens statically, not in follow mode.
func TestViewCompletedPlanDoesNotFollow(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	writePlanLog(t, m, mod.Path, "prod", "Plan: 1 to add, 0 to change, 0 to destroy.\n")
	m.cursor = 2

	cmd := keyPress(m, "enter")
	if m.detailFollow != "" {
		t.Error("a completed plan should open statically, not follow")
	}
	_, tick := m.Update(cmd())
	if tick != nil {
		t.Error("static log view should not schedule a follow tick")
	}
}
