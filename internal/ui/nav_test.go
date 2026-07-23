// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"

	"github.com/espoon-voltti/tfmux/internal/config"
	"github.com/espoon-voltti/tfmux/internal/domain"
	"github.com/espoon-voltti/tfmux/internal/runner"
	"github.com/espoon-voltti/tfmux/internal/state"
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

// r fans out one gitStatusMsg (per repo) and one fingerprintMsg (per
// non-ignored module) plus an expiredPlansMsg, all tagged with the same
// refresh generation. The status bar should stay on "refreshing…" until every
// one of them has reported back, then announce completion.
func TestRefreshAnnouncesCompletion(t *testing.T) {
	m, _ := fixtureModel(t) // one repo, one module (see fixtureModel)

	m.refresh()
	gen := m.refreshGen

	if m.status != "refreshing…" {
		t.Fatalf("status = %q, want refreshing…", m.status)
	}

	m.Update(gitStatusMsg{repoPath: m.repos[0].Path, gen: gen})
	if m.status != "refreshing…" {
		t.Fatalf("status = %q after gitStatusMsg, want still refreshing…", m.status)
	}

	m.Update(fingerprintMsg{modulePath: m.repos[0].Modules[0].Path, fingerprint: "abc", gen: gen})
	if m.status != "refreshing…" {
		t.Fatalf("status = %q after fingerprintMsg, want still refreshing…", m.status)
	}

	m.Update(expiredPlansMsg{n: 0, gen: gen})
	if m.status != "refreshed" {
		t.Errorf("status = %q, want refreshed", m.status)
	}
}

// The completion message folds in the expired-plan count, when nonzero.
func TestRefreshAnnouncesExpiredPlans(t *testing.T) {
	m, _ := fixtureModel(t)

	m.refresh()
	gen := m.refreshGen

	m.Update(gitStatusMsg{repoPath: m.repos[0].Path, gen: gen})
	m.Update(fingerprintMsg{modulePath: m.repos[0].Modules[0].Path, fingerprint: "abc", gen: gen})
	m.Update(expiredPlansMsg{n: 2, gen: gen})

	if want := "refreshed — expired 2 stale plan file(s)"; m.status != want {
		t.Errorf("status = %q, want %q", m.status, want)
	}
}

// Pressing r again mid-refresh supersedes the previous generation: stragglers
// from gen 1 must not complete gen 2's count early or clobber its status.
func TestRefreshSupersedesPreviousGeneration(t *testing.T) {
	m, _ := fixtureModel(t)

	m.refresh()
	gen1 := m.refreshGen

	m.refresh() // superseded before gen1 finished
	gen2 := m.refreshGen

	// A straggler from gen1 must not count toward gen2.
	m.Update(gitStatusMsg{repoPath: m.repos[0].Path, gen: gen1})
	m.Update(fingerprintMsg{modulePath: m.repos[0].Modules[0].Path, fingerprint: "abc", gen: gen1})
	m.Update(expiredPlansMsg{n: 0, gen: gen1})
	if m.status != "refreshing…" {
		t.Fatalf("stale gen1 messages completed the refresh: status = %q", m.status)
	}

	m.Update(gitStatusMsg{repoPath: m.repos[0].Path, gen: gen2})
	m.Update(fingerprintMsg{modulePath: m.repos[0].Modules[0].Path, fingerprint: "abc", gen: gen2})
	m.Update(expiredPlansMsg{n: 0, gen: gen2})
	if m.status != "refreshed" {
		t.Errorf("status = %q, want refreshed", m.status)
	}
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

// twoRepoModel builds two repos, each with two modules, for collapse tests.
func twoRepoModel(t *testing.T) *Model {
	t.Helper()
	m := NewModel(config.Default(), state.New(t.TempDir()))
	m.width, m.height = 100, 30
	var repos []*domain.Repo
	for _, rn := range []string{"repo1", "repo2"} {
		repo := &domain.Repo{Path: "/iac/" + rn, Name: rn}
		for _, mn := range []string{"a", "b"} {
			mod := &domain.Module{Repo: repo, Path: "/iac/" + rn + "/" + mn, RelPath: mn, TFBin: "terraform"}
			repo.Modules = append(repo.Modules, mod)
		}
		repos = append(repos, repo)
	}
	m.repos = repos
	m.reflow()
	return m
}

// H collapses every other repo (and its modules), leaving the cursor's repo and
// all its root modules expanded, and keeps the cursor on its item.
func TestCollapseOthersZoomsToCursor(t *testing.T) {
	m := twoRepoModel(t)
	mod := m.repos[0].Modules[0] // repo1/a
	enumerated(t, m, mod, "prod")
	// rows: repo1, a, prod, b, repo2, c, d ; put cursor on workspace prod
	m.cursor = 2
	if r, _ := m.currentRow(); r.kind != rowWorkspace {
		t.Fatalf("setup: cursor not on workspace, got %v", r.kind)
	}

	keyPress(m, "H")

	if !m.collapsed[m.repos[1].Path] {
		t.Error("other repo should be collapsed")
	}
	if m.collapsed[m.repos[0].Path] {
		t.Error("cursor's repo should stay expanded")
	}
	for _, sib := range m.repos[0].Modules {
		if m.collapsed[sib.Path] {
			t.Errorf("module %s in the cursor's repo should stay expanded", sib.RelPath)
		}
	}
	if r, ok := m.currentRow(); !ok || r.kind != rowWorkspace || r.ws.Name != "prod" {
		t.Errorf("cursor should stay on the workspace, got %+v", r)
	}
}

// L expands the whole tree, keeping the cursor on its item.
func TestExpandAllUncollapses(t *testing.T) {
	m := twoRepoModel(t)
	m.collapsed[m.repos[0].Path] = true
	m.collapsed[m.repos[1].Modules[0].Path] = true
	m.reflow()
	m.cursor = 0 // repo1 row

	keyPress(m, "L")

	if len(m.collapsed) != 0 {
		t.Errorf("expand all should clear collapsed state, got %v", m.collapsed)
	}
	if r, ok := m.currentRow(); !ok || r.kind != rowRepo || r.repo.Name != "repo1" {
		t.Errorf("cursor should stay on repo1, got %+v", r)
	}
}

// The title bar shows data-scale counts on the main view and the plan's
// repo/module/workspace when a plan log is open.
func TestTitleBarContext(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	if !strings.Contains(firstLine(m.View()), "1 repos · 1 root modules · 1 workspaces") {
		t.Errorf("main view header missing counts: %q", firstLine(m.View()))
	}

	m.cursor = 2 // workspace row
	m.detailTitle = m.detailTitleFor(runner.KindPlan, mod.Path+"//prod")
	m.focus = focusDetail
	if !strings.Contains(firstLine(m.View()), "repo1 · envs/prod · prod") {
		t.Errorf("detail header missing plan context: %q", firstLine(m.View()))
	}
}

// The main-view totals exclude ignored items, but counting everything once Z
// reveals them.
func TestTitleBarCountsRespectIgnores(t *testing.T) {
	m := twoRepoModel(t)
	for _, repo := range m.repos {
		for _, mod := range repo.Modules {
			enumerated(t, m, mod, "default", "prod")
		}
	}
	// 2 repos · 4 modules · 8 workspaces with nothing ignored
	if !strings.Contains(firstLine(m.View()), "2 repos · 4 root modules · 8 workspaces") {
		t.Fatalf("unignored counts wrong: %q", firstLine(m.View()))
	}

	// ignore one whole repo and one module of the other
	m.ignore[m.repos[0].Path] = true
	m.ignore[m.repos[1].Modules[0].Path] = true
	m.reflow()
	if !strings.Contains(firstLine(m.View()), "1 repos · 1 root modules · 2 workspaces") {
		t.Errorf("ignored counts not excluded: %q", firstLine(m.View()))
	}

	// Z reveals everything → count all again (the Z handler reflows)
	m.showIgnored = true
	m.reflow()
	if !strings.Contains(firstLine(m.View()), "2 repos · 4 root modules · 8 workspaces") {
		t.Errorf("Z should count everything: %q", firstLine(m.View()))
	}
}

// ←/h on a workspace jumps to its module and collapses it; pressing it again on
// the now-collapsed module jumps to the repo and collapses that.
func TestCollapseLeftWalksUp(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	m.cursor = 2 // workspace row
	if r, _ := m.currentRow(); r.kind != rowWorkspace {
		t.Fatalf("setup: expected workspace, got %v", r.kind)
	}

	keyPress(m, "h")
	if !m.collapsed[mod.Path] {
		t.Error("h on a workspace should collapse its module")
	}
	if r, ok := m.currentRow(); !ok || r.kind != rowModule {
		t.Fatalf("cursor should land on the module row, got %+v", r)
	}

	keyPress(m, "h")
	if !m.collapsed[mod.Repo.Path] {
		t.Error("h on a collapsed module should collapse its repo")
	}
	if r, ok := m.currentRow(); !ok || r.kind != rowRepo {
		t.Fatalf("cursor should land on the repo row, got %+v", r)
	}
}

// ←/h on an expanded module collapses it in place, leaving the cursor on it.
func TestCollapseLeftOnExpandedModuleStays(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	m.cursor = 1 // module row (expanded)

	keyPress(m, "h")
	if !m.collapsed[mod.Path] {
		t.Error("h on an expanded module should collapse it")
	}
	if r, ok := m.currentRow(); !ok || r.kind != rowModule {
		t.Errorf("cursor should stay on the module, got %+v", r)
	}
}

// →/l on an already-expanded repo expands all its root modules.
func TestExpandRightOnRepoExpandsModules(t *testing.T) {
	m := twoRepoModel(t)
	repo := m.repos[0]
	for _, mod := range repo.Modules {
		m.collapsed[mod.Path] = true
	}
	m.reflow()
	m.cursor = 0 // repo1 row (expanded)
	if r, _ := m.currentRow(); r.kind != rowRepo {
		t.Fatalf("setup: expected repo, got %v", r.kind)
	}

	keyPress(m, "l")
	for _, mod := range repo.Modules {
		if m.collapsed[mod.Path] {
			t.Errorf("l on an expanded repo should expand module %s", mod.RelPath)
		}
	}
}

// →/l on a collapsed repo reveals it without expanding its modules.
func TestExpandRightOnCollapsedRepoRevealsModules(t *testing.T) {
	m := twoRepoModel(t)
	repo := m.repos[0]
	m.collapsed[repo.Path] = true
	m.collapsed[repo.Modules[0].Path] = true
	m.reflow()
	m.cursor = 0 // repo1 (collapsed)

	keyPress(m, "l")
	if m.collapsed[repo.Path] {
		t.Error("l on a collapsed repo should expand the repo")
	}
	if !m.collapsed[repo.Modules[0].Path] {
		t.Error("expanding a collapsed repo should not also expand its modules")
	}
}

// The spinner's tick loop goes quiet when nothing is animating, and re-arms the
// moment new work appears — so an idle screen stops redrawing.
func TestSpinnerStopsWhenIdleAndRearms(t *testing.T) {
	m, mod := fixtureModel(t)
	m.spinning = true // pretend the loop from Init is running

	// Idle (no tasks, not discovering): the tick stops itself.
	_, cmd := m.Update(spinner.TickMsg{})
	if m.spinning {
		t.Error("spinner should stop ticking when idle")
	}
	if cmd != nil {
		t.Error("idle tick should not schedule another tick")
	}

	// New work re-arms the loop.
	enumerated(t, m, mod, "prod")
	m.addTask(runner.KindPlan, mod.Path+"//prod")
	if c := m.tickSpinner(); c == nil || !m.spinning {
		t.Error("tickSpinner should restart the loop when work appears")
	}
	// ...and is idempotent while already spinning.
	if c := m.tickSpinner(); c != nil {
		t.Error("tickSpinner should be a no-op while already spinning")
	}
}

// Z retains the cursor on the previously focused item when showing ignored
// items; when hiding them again, the cursor stays if the item is still visible.
func TestShowIgnoredRetainsCursor(t *testing.T) {
	m := twoRepoModel(t)
	for _, repo := range m.repos {
		for _, mod := range repo.Modules {
			enumerated(t, m, mod, "default", "prod")
		}
	}
	// ignore entire repo1 (rows 0–4) so they vanish; cursor lands on repo2
	m.ignore[m.repos[0].Path] = true
	m.reflow()
	// move cursor to repo2/b/prod (last visible workspace)
	m.cursor = len(m.rows) - 1
	focused, _ := m.currentRow()

	// Z: show ignored — repo1's rows insert before repo2; cursor must stay on same item
	keyPress(m, "Z")
	if r, _ := m.currentRow(); r.nodeKey() != focused.nodeKey() {
		t.Errorf("after showing ignored: cursor on %q, want %q", r.nodeKey(), focused.nodeKey())
	}

	// Z again: hide ignored — item is still visible (it was never ignored)
	keyPress(m, "Z")
	if r, _ := m.currentRow(); r.nodeKey() != focused.nodeKey() {
		t.Errorf("after hiding ignored: cursor on %q, want %q", r.nodeKey(), focused.nodeKey())
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
