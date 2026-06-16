// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/japsu/tfmux/internal/domain"
	"github.com/japsu/tfmux/internal/state"
	"github.com/japsu/tfmux/internal/tftest"
)

type fixture struct {
	runner  *Runner
	store   *state.Store
	logFile string
	bin     string
}

func newFixture(t *testing.T, parallelism int) *fixture {
	t.Helper()
	bin := tftest.Write(t, t.TempDir())
	logFile := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("TFMUX_FAKE_LOG", logFile)
	store := state.New(t.TempDir())
	return &fixture{
		runner:  New(parallelism, store, nil),
		store:   store,
		logFile: logFile,
		bin:     bin,
	}
}

// newModule creates a git-less module dir with .terraform pre-created.
func (f *fixture) newModule(t *testing.T, name string) *domain.Module {
	t.Helper()
	repoDir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(repoDir, ".terraform"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := &domain.Repo{Path: repoDir, Name: name}
	m := &domain.Module{Repo: repo, Path: repoDir, RelPath: ".", TFBin: f.bin}
	repo.Modules = []*domain.Module{m}
	return m
}

// waitFor returns the first event satisfying pred, or fails on timeout.
func waitFor(t *testing.T, ch chan Event, pred func(Event) bool) Event {
	t.Helper()
	timeout := time.After(30 * time.Second)
	for {
		select {
		case ev := <-ch:
			if pred(ev) {
				return ev
			}
		case <-timeout:
			t.Fatal("timed out waiting for event")
		}
	}
}

// waitTerminal collects want terminal events of the given kind.
func waitTerminal(t *testing.T, ch chan Event, kind Kind, want int) []Event {
	t.Helper()
	var got []Event
	for len(got) < want {
		ev := waitFor(t, ch, func(e Event) bool { return e.Kind == kind && e.Phase.Terminal() })
		got = append(got, ev)
	}
	return got
}

func TestEnumerateEmitsWorkspaces(t *testing.T) {
	f := newFixture(t, 2)
	m := f.newModule(t, "mod1")
	if !f.runner.EnqueueEnumerate(m) {
		t.Fatal("enqueue refused")
	}
	ev := waitTerminal(t, f.runner.Events, KindEnumerate, 1)[0]
	if ev.Phase != PhaseDone {
		t.Fatalf("phase = %v, err = %q", ev.Phase, ev.Err)
	}
	if strings.Join(ev.Workspaces, ",") != "default,prod,staging" {
		t.Errorf("workspaces = %v", ev.Workspaces)
	}
	if cache, ok := f.store.LoadWorkspaces(m.Path); !ok || len(cache.Workspaces) != 3 {
		t.Errorf("enumeration not cached: %+v ok=%v", cache, ok)
	}
}

func TestEnqueueDedup(t *testing.T) {
	f := newFixture(t, 1)
	t.Setenv("TFMUX_FAKE_SLEEP", "1")
	m := f.newModule(t, "mod1")
	if !f.runner.EnqueueEnumerate(m) {
		t.Fatal("first enqueue refused")
	}
	if f.runner.EnqueueEnumerate(m) {
		t.Error("duplicate enqueue accepted")
	}
	waitTerminal(t, f.runner.Events, KindEnumerate, 1)
}

func TestPlanPersistsRecordAndPlanFile(t *testing.T) {
	f := newFixture(t, 2)
	t.Setenv("TFMUX_FAKE_PLAN_EXIT", "2")
	showJSON := filepath.Join(t.TempDir(), "show.json")
	plan := `{"format_version":"1.2","resource_changes":[
		{"address":"a","change":{"actions":["create"]}},
		{"address":"b","change":{"actions":["delete","create"]}},
		{"address":"c","change":{"actions":["update"]}},
		{"address":"d","change":{"actions":["no-op"]}}]}`
	if err := os.WriteFile(showJSON, []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TFMUX_FAKE_SHOW_JSON", showJSON)

	m := f.newModule(t, "mod1")
	ws := &domain.Workspace{Module: m, Name: "prod"}
	if !f.runner.EnqueuePlan(ws) {
		t.Fatal("enqueue refused")
	}
	ev := waitTerminal(t, f.runner.Events, KindPlan, 1)[0]
	if ev.Phase != PhaseDone {
		t.Fatalf("phase = %v, err = %q", ev.Phase, ev.Err)
	}
	rec := ev.Record
	if rec.PlanExitCode != 2 {
		t.Errorf("exit = %d", rec.PlanExitCode)
	}
	if rec.Summary.Add != 2 || rec.Summary.Change != 1 || rec.Summary.Destroy != 1 {
		t.Errorf("summary = %+v", rec.Summary)
	}
	if rec.TFBinVersion != "1.9.9" {
		t.Errorf("version = %q", rec.TFBinVersion)
	}
	if !f.store.HasPlanFile(m.Path, "prod") {
		t.Error("plan file missing")
	}
	planPath, _ := f.store.PlanFilePath(m.Path, "prod")
	if info, err := os.Stat(planPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("plan file mode: %v err: %v", info.Mode(), err)
	}
	loaded, err := f.store.LoadRun(m.Path, "prod")
	if err != nil || loaded == nil || loaded.PlanExitCode != 2 {
		t.Errorf("LoadRun: %+v err %v", loaded, err)
	}
	logPath, _ := f.store.PlanLogPath(m.Path, "prod")
	if data, err := os.ReadFile(logPath); err != nil || !strings.Contains(string(data), "Plan: 1 to add") {
		t.Errorf("plan log: %q err %v", data, err)
	}
}

func TestCleanPlanDiscardsPlanFile(t *testing.T) {
	f := newFixture(t, 2)
	t.Setenv("TFMUX_FAKE_PLAN_EXIT", "0")
	m := f.newModule(t, "mod1")
	ws := &domain.Workspace{Module: m, Name: "default"}
	f.runner.EnqueuePlan(ws)
	ev := waitTerminal(t, f.runner.Events, KindPlan, 1)[0]
	if ev.Record.PlanExitCode != 0 {
		t.Fatalf("exit = %d", ev.Record.PlanExitCode)
	}
	if f.store.HasPlanFile(m.Path, "default") {
		t.Error("clean plan should not leave a plan file")
	}
}

// TestSameModuleSerializedCrossModuleParallel reads the stub's append-only
// call log: the start/end line order proves same-module jobs never overlap,
// and that cross-module jobs do (a mod2 start appears before mod1 finishes).
func TestSameModuleSerializedCrossModuleParallel(t *testing.T) {
	f := newFixture(t, 4)
	t.Setenv("TFMUX_FAKE_SLEEP", "1")
	m1 := f.newModule(t, "mod1")
	m2 := f.newModule(t, "mod2")
	// two workspaces in m1 (must serialize), one in m2 (may overlap with m1)
	f.runner.EnqueuePlan(&domain.Workspace{Module: m1, Name: "prod"})
	f.runner.EnqueuePlan(&domain.Workspace{Module: m1, Name: "staging"})
	f.runner.EnqueuePlan(&domain.Workspace{Module: m2, Name: "prod"})
	waitTerminal(t, f.runner.Events, KindPlan, 3)

	data, err := os.ReadFile(f.logFile)
	if err != nil {
		t.Fatal(err)
	}
	open := map[string]int{} // module dir -> currently running plans
	sawCrossModuleOverlap := false
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[4] != "plan" {
			continue
		}
		kind, pwd := fields[0], fields[2]
		switch kind {
		case "start":
			open[pwd]++
			if open[pwd] > 1 {
				t.Fatalf("module %s had %d concurrent plans", pwd, open[pwd])
			}
			for other, n := range open {
				if other != pwd && n > 0 {
					sawCrossModuleOverlap = true
				}
			}
		case "end":
			open[pwd]--
		}
	}
	if !sawCrossModuleOverlap {
		t.Error("expected cross-module plans to overlap with parallelism 4")
	}
}

// With one slot, a high-priority plan jumps ahead of a queued low-priority
// enumeration once the slot frees.
func TestPriorityPlanBeatsEnumerate(t *testing.T) {
	f := newFixture(t, 1)
	t.Setenv("TFMUX_FAKE_SLEEP", "1")
	blocker := f.newModule(t, "blocker")
	low := f.newModule(t, "low")
	high := f.newModule(t, "high")

	f.runner.EnqueueEnumerate(blocker) // fills the only slot
	waitFor(t, f.runner.Events, func(e Event) bool {
		return e.Phase == PhaseRunning && e.Kind == KindEnumerate && e.Key == blocker.Path
	})
	// both queue behind the blocker; plan must start first
	f.runner.EnqueueEnumerate(low)
	planWS := &domain.Workspace{Module: high, Name: "prod"}
	f.runner.EnqueuePlan(planWS)

	planID := TaskID(KindPlan, planWS.Key())
	lowID := TaskID(KindEnumerate, low.Path)
	var order []string
	for len(order) < 2 {
		ev := waitFor(t, f.runner.Events, func(e Event) bool {
			return e.Phase == PhaseRunning && (e.TaskID() == planID || e.TaskID() == lowID)
		})
		order = append(order, ev.TaskID())
	}
	if order[0] != planID {
		t.Errorf("expected plan to run before low-priority enumeration, got %v", order)
	}
	// low runs last (one slot), so its terminal means everything has finished
	waitFor(t, f.runner.Events, func(e Event) bool { return e.TaskID() == lowID && e.Phase.Terminal() })
}

// A task canceled while still queued emits Canceled and never runs.
func TestCancelQueuedEmitsCanceled(t *testing.T) {
	f := newFixture(t, 1)
	t.Setenv("TFMUX_FAKE_SLEEP", "1")
	blocker := f.newModule(t, "blocker")
	victim := f.newModule(t, "victim")

	f.runner.EnqueueEnumerate(blocker)
	waitFor(t, f.runner.Events, func(e Event) bool {
		return e.Phase == PhaseRunning && e.Key == blocker.Path
	})
	ws := &domain.Workspace{Module: victim, Name: "prod"}
	f.runner.EnqueuePlan(ws)
	f.runner.Cancel(ws.Key())

	ev := waitFor(t, f.runner.Events, func(e Event) bool {
		return e.Kind == KindPlan && e.Key == ws.Key()
	})
	if ev.Phase != PhaseCanceled {
		t.Errorf("first plan event = %v, want Canceled (it should never have run)", ev.Phase)
	}
	// let the blocker finish before TempDir cleanup
	waitFor(t, f.runner.Events, func(e Event) bool {
		return e.Key == blocker.Path && e.Phase.Terminal()
	})
}
