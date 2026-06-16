// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/japsu/tfmux/internal/config"
	"github.com/japsu/tfmux/internal/domain"
	"github.com/japsu/tfmux/internal/runner"
	"github.com/japsu/tfmux/internal/state"
)

func drainInit(t *testing.T, m *Model, n int) {
	t.Helper()
	timeout := time.After(15 * time.Second)
	for n > 0 {
		select {
		case ev := <-m.runner.Events:
			if ev.Kind == runner.KindInit && ev.Phase.Terminal() {
				n--
			}
		case <-timeout:
			t.Fatal("timed out draining init events")
		}
	}
}

// I on a repo row queues init -upgrade for every module in the repo.
func TestInitUpgradeRepoQueuesAllModules(t *testing.T) {
	m := NewModel(config.Default(), state.New(t.TempDir()))
	m.width, m.height = 100, 30
	repo := &domain.Repo{Path: "/iac/repo1", Name: "repo1"}
	m1 := &domain.Module{Repo: repo, Path: "/iac/repo1/a", RelPath: "a", TFBin: "terraform"}
	m2 := &domain.Module{Repo: repo, Path: "/iac/repo1/b", RelPath: "b", TFBin: "terraform"}
	repo.Modules = []*domain.Module{m1, m2}
	m.repos = []*domain.Repo{repo}
	m.reflow()
	m.cursor = 0 // repo row

	m.initUpgradeCurrent()

	if !m.hasTask(runner.KindInit, m1.Path) || !m.hasTask(runner.KindInit, m2.Path) {
		t.Errorf("init -upgrade not queued for every module: %v", m.tasks)
	}
	if !strings.Contains(m.status, "2 module") {
		t.Errorf("status = %q", m.status)
	}
	drainInit(t, m, 2) // let the async jobs finish before TempDir cleanup
}

// manyWorkspaces enumerates n workspaces so the list exceeds the screen.
func manyWorkspaces(t *testing.T, m *Model, mod *domain.Module, n int) {
	t.Helper()
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("ws%02d", i)
	}
	enumerated(t, m, mod, names...)
}

func drainEnum(t *testing.T, m *Model, n int) {
	t.Helper()
	timeout := time.After(15 * time.Second)
	for n > 0 {
		select {
		case ev := <-m.runner.Events:
			if ev.Kind == runner.KindEnumerate && ev.Phase.Terminal() {
				n--
			}
		case <-timeout:
			t.Fatal("timed out draining enum events")
		}
	}
}

// At the bottom of a scrolled list, moving up should walk the cursor up the
// screen rather than scrolling the whole list (stable scroll offset).
func TestCursorMovesWithinScreenAtBottom(t *testing.T) {
	m, mod := fixtureModel(t)
	manyWorkspaces(t, m, mod, 40)
	m.cursor = len(m.rows) - 1
	m.ensureVisible()
	top := m.top
	if top == 0 {
		t.Fatalf("expected a scrolled window; top=%d rows=%d h=%d", top, len(m.rows), m.visibleHeight())
	}
	keyPress(m, "k") // up
	if m.cursor != len(m.rows)-2 {
		t.Errorf("cursor = %d, want %d", m.cursor, len(m.rows)-2)
	}
	if m.top != top {
		t.Errorf("list scrolled on up-at-bottom: top %d -> %d", top, m.top)
	}
}

// PageDown first jumps to the bottom of the visible screen, then pages.
func TestPageDownTwoStage(t *testing.T) {
	m, mod := fixtureModel(t)
	manyWorkspaces(t, m, mod, 40)
	h := m.visibleHeight()
	m.cursor, m.top = 0, 0

	m.pageDown()
	if m.cursor != h-1 {
		t.Fatalf("first pageDown: cursor = %d, want bottom-of-screen %d", m.cursor, h-1)
	}
	if m.top != 0 {
		t.Fatalf("first pageDown should not scroll: top = %d", m.top)
	}

	prev := m.cursor
	m.pageDown()
	if m.cursor <= prev {
		t.Errorf("second pageDown did not advance: %d -> %d", prev, m.cursor)
	}
	if m.top == 0 {
		t.Errorf("second pageDown should have scrolled the window")
	}
}

// PageUp mirrors PageDown: first to the top of the screen, then a screenful up.
func TestPageUpTwoStage(t *testing.T) {
	m, mod := fixtureModel(t)
	manyWorkspaces(t, m, mod, 40)
	m.cursor = len(m.rows) - 1
	m.ensureVisible()
	top := m.top

	m.pageUp()
	if m.cursor != top {
		t.Fatalf("first pageUp: cursor = %d, want top-of-screen %d", m.cursor, top)
	}

	m.pageUp()
	if m.cursor >= top {
		t.Errorf("second pageUp did not page up: cursor = %d", m.cursor)
	}
}

// Discovery should populate workspaces from the on-disk cache without kicking
// off a (slow, rate-limited) enumeration.
func TestDiscoveryUsesWorkspaceCache(t *testing.T) {
	m := NewModel(config.Default(), state.New(t.TempDir()))
	m.width, m.height = 100, 30
	repo := &domain.Repo{Path: "/iac/repo1", Name: "repo1"}
	mod := &domain.Module{Repo: repo, Path: "/iac/repo1/envs/prod", RelPath: "envs/prod"}
	repo.Modules = []*domain.Module{mod}

	if err := m.store.SaveWorkspaces(mod.Path, []string{"default", "prod"}, time.Now()); err != nil {
		t.Fatal(err)
	}

	m.updateDiscovery(discoveryMsg{repos: []*domain.Repo{repo}})

	if mod.WorkspaceState != domain.WorkspacesReady {
		t.Fatalf("state = %v, want Ready (served from cache)", mod.WorkspaceState)
	}
	if len(mod.Workspaces) != 2 {
		t.Fatalf("workspaces = %d, want 2", len(mod.Workspaces))
	}
	if m.runner.Running(mod.Path) {
		t.Error("cache hit should not enqueue enumeration")
	}
}

// The refresh hotkey re-enumerates the module under the cursor.
func TestRefreshWorkspacesReEnumerates(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	m.cursor = 1 // module row

	m.refreshWorkspaces()

	if !m.hasTask(runner.KindEnumerate, mod.Path) {
		t.Error("expected an enumerate task to be queued")
	}
	if !strings.Contains(m.status, "re-enumerating") {
		t.Errorf("status = %q", m.status)
	}
	drainEnum(t, m, 1) // let the async job finish before TempDir cleanup
}

// Enter on a module whose enumeration failed opens the full error in the
// detail viewport (the row itself only shows the first line).
func TestViewEnumerationErrorLog(t *testing.T) {
	m, mod := fixtureModel(t)
	fullErr := "workspace list in /iac/repo1/envs/prod failed:\nError: No valid credential sources found\n  more detail here"
	m.updateRunnerEvent(runner.Event{
		Kind: runner.KindEnumerate, Key: mod.Path, ModulePath: mod.Path,
		Phase: runner.PhaseFailed, Err: fullErr,
	})
	m.cursor = 1 // module row

	keyPress(m, "enter")

	if m.focus != focusDetail {
		t.Fatalf("focus = %v, want focusDetail", m.focus)
	}
	if !strings.Contains(m.View(), "No valid credential sources") {
		t.Error("full enumeration error not shown in detail viewport")
	}
}

// Queued tasks render distinctly from running ones, and the header counts
// both. The runner's PhaseRunning event marks the queued→running transition.
func TestQueuedVsRunningDistinction(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "a", "b")
	ka, kb := mod.Path+"//a", mod.Path+"//b"

	// both queued (task created, neither started yet)
	m.addTask(runner.KindPlan, ka)
	m.addTask(runner.KindPlan, kb)
	v := m.View()
	if !strings.Contains(v, "queued") {
		t.Error("queued plans not labeled")
	}
	if strings.Contains(v, "planning") {
		t.Error("nothing should show as running yet")
	}
	if !strings.Contains(v, "2 queued") {
		t.Errorf("header should report 2 queued: %q", firstLine(v))
	}

	// a starts executing
	m.updateRunnerEvent(runner.Event{Kind: runner.KindPlan, Key: ka, ModulePath: mod.Path, Phase: runner.PhaseRunning})
	v = m.View()
	if ts := m.task(runner.KindPlan, ka); ts == nil || !ts.running {
		t.Fatal("PhaseRunning should mark the task running")
	}
	if !strings.Contains(v, "1 running") || !strings.Contains(v, "1 queued") {
		t.Errorf("header should report 1 running, 1 queued: %q", firstLine(v))
	}
}

// Canceling a queued plan clears its task immediately (the runner's later
// Canceled event is then a no-op).
func TestCancelQueuedClearsTask(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "a")
	k := mod.Path + "//a"
	m.addTask(runner.KindPlan, k) // queued, not yet running
	m.cursor = 2                  // workspace row
	keyPress(m, "x")
	if m.hasTask(runner.KindPlan, k) {
		t.Error("cancel should clear the queued plan task")
	}
}

// Ignored items, when revealed with Z, render with the muted marker.
func TestIgnoredRowRendersMuted(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	m.ignore[mod.Path] = true
	m.showIgnored = true
	m.reflow()
	if !strings.Contains(m.View(), "(ignored)") {
		t.Error("ignored marker missing from revealed row")
	}
}
