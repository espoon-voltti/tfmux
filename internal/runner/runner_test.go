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

	"github.com/espoon-voltti/tfmux/internal/domain"
	"github.com/espoon-voltti/tfmux/internal/state"
	"github.com/espoon-voltti/tfmux/internal/tftest"
)

type fixture struct {
	runner  *Runner
	store   *state.Store
	logFile string
	bin     string
}

func newFixture(t *testing.T, parallelism int) *fixture {
	return newFixtureInitLimit(t, parallelism, 0)
}

func newFixtureInitLimit(t *testing.T, parallelism, initLimit int) *fixture {
	t.Helper()
	bin := tftest.Write(t, t.TempDir())
	logFile := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("TFMUX_FAKE_LOG", logFile)
	store := state.New(t.TempDir())
	return &fixture{
		runner:  New(parallelism, initLimit, store, nil),
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

// newUninitModule creates a git-less module dir without .terraform, so the
// first task against it must go through init.
func (f *fixture) newUninitModule(t *testing.T, name string) *domain.Module {
	t.Helper()
	repoDir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
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

func TestOutputCapturesLog(t *testing.T) {
	f := newFixture(t, 2)
	m := f.newModule(t, "mod1")
	ws := &domain.Workspace{Module: m, Name: "prod"}
	if !f.runner.EnqueueOutput(ws) {
		t.Fatal("enqueue refused")
	}
	ev := waitTerminal(t, f.runner.Events, KindOutput, 1)[0]
	if ev.Phase != PhaseDone {
		t.Fatalf("phase = %v, err = %q", ev.Phase, ev.Err)
	}
	logPath, _ := f.store.OutputLogPath(m.Path, "prod")
	if data, err := os.ReadFile(logPath); err != nil || !strings.Contains(string(data), "hello") {
		t.Errorf("output log: %q err %v", data, err)
	}
}

func TestOutputFailurePropagatesError(t *testing.T) {
	f := newFixture(t, 2)
	t.Setenv("TFMUX_FAKE_OUTPUT_EXIT", "1")
	m := f.newModule(t, "mod1")
	ws := &domain.Workspace{Module: m, Name: "prod"}
	f.runner.EnqueueOutput(ws)
	ev := waitTerminal(t, f.runner.Events, KindOutput, 1)[0]
	if ev.Phase != PhaseFailed {
		t.Fatalf("phase = %v, want Failed", ev.Phase)
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

func TestPlanErrorClassifiesKnownSignatures(t *testing.T) {
	for stderr, want := range map[string]string{
		"Error: Inconsistent dependency lock file":  "init -upgrade required",
		"Error: Error acquiring the state lock":     "state locked",
		"Error: something else went wrong entirely": "",
	} {
		f := newFixture(t, 2)
		t.Setenv("TFMUX_FAKE_PLAN_STDERR", stderr)
		m := f.newModule(t, "mod1")
		ws := &domain.Workspace{Module: m, Name: "prod"}
		f.runner.EnqueuePlan(ws)
		ev := waitTerminal(t, f.runner.Events, KindPlan, 1)[0]
		if ev.Record.PlanExitCode != 1 {
			t.Fatalf("stderr %q: exit = %d", stderr, ev.Record.PlanExitCode)
		}
		if ev.Record.PlanErrorKind != want {
			t.Errorf("stderr %q: PlanErrorKind = %q, want %q", stderr, ev.Record.PlanErrorKind, want)
		}
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
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
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

// A task on an uninitialized module must defer to a KindInit task and retry
// afterward, transparently to the caller: two Running events (before and
// after the init) and exactly one terminal event, reporting success.
func TestDefersToInitWhenUninitialized(t *testing.T) {
	cases := []struct {
		name    string
		kind    Kind
		enqueue func(f *fixture, m *domain.Module) (ok bool, key string)
	}{
		{"enumerate", KindEnumerate, func(f *fixture, m *domain.Module) (bool, string) {
			return f.runner.EnqueueEnumerate(m), m.Path
		}},
		{"plan", KindPlan, func(f *fixture, m *domain.Module) (bool, string) {
			ws := &domain.Workspace{Module: m, Name: "prod"}
			return f.runner.EnqueuePlan(ws), ws.Key()
		}},
		{"output", KindOutput, func(f *fixture, m *domain.Module) (bool, string) {
			ws := &domain.Workspace{Module: m, Name: "prod"}
			return f.runner.EnqueueOutput(ws), ws.Key()
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, 2)
			m := f.newUninitModule(t, "mod1")
			ok, key := c.enqueue(f, m)
			if !ok {
				t.Fatal("enqueue refused")
			}
			taskID := TaskID(c.kind, key)
			initID := TaskID(KindInit, m.Path)

			var running int
			var initDone bool
			var final Event
		loop:
			for {
				ev := waitFor(t, f.runner.Events, func(e Event) bool {
					return e.TaskID() == taskID || e.TaskID() == initID
				})
				switch {
				case ev.TaskID() == taskID && ev.Phase == PhaseRunning:
					running++
				case ev.TaskID() == taskID && ev.Phase.Terminal():
					final = ev
					break loop
				case ev.TaskID() == initID && ev.Phase.Terminal():
					initDone = true
				}
			}
			if running != 2 {
				t.Errorf("expected 2 Running events (before and after init), got %d", running)
			}
			if !initDone {
				t.Error("expected the deferred KindInit task to complete")
			}
			if final.Phase != PhaseDone {
				t.Errorf("final phase = %v, err = %q", final.Phase, final.Err)
			}
		})
	}
}

// A plan that keeps failing with an init-shaped error retries exactly once
// (via one KindInit task) and then reports the real failure — no infinite
// requeue loop.
func TestPlanNeedsInitRetriesOnceThenReportsFailure(t *testing.T) {
	f := newFixture(t, 2)
	t.Setenv("TFMUX_FAKE_PLAN_STDERR", `Error: Backend initialization required, please run "terraform init"`)
	m := f.newModule(t, "mod1") // already initialized: exercises the reactive path, not the upfront check
	ws := &domain.Workspace{Module: m, Name: "prod"}
	f.runner.EnqueuePlan(ws)

	waitTerminal(t, f.runner.Events, KindInit, 1)
	ev := waitTerminal(t, f.runner.Events, KindPlan, 1)[0]
	if ev.Record == nil || ev.Record.PlanExitCode != 1 {
		t.Fatalf("record = %+v", ev.Record)
	}

	data, err := os.ReadFile(f.logFile)
	if err != nil {
		t.Fatal(err)
	}
	var planCalls int
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 5 && fields[0] == "start" && fields[4] == "plan" {
			planCalls++
		}
	}
	if planCalls != 2 {
		t.Errorf("expected exactly one retry (2 plan calls), got %d", planCalls)
	}
}

// With init_parallelism=1, two modules that both need init never run their
// init step concurrently, while a third, already-initialized module's plan
// is not held up behind that serialization.
func TestInitParallelismLimitsGlobally(t *testing.T) {
	f := newFixtureInitLimit(t, 3, 1)
	t.Setenv("TFMUX_FAKE_SLEEP", "1")
	mod1 := f.newUninitModule(t, "mod1")
	mod2 := f.newUninitModule(t, "mod2")
	mod3 := f.newModule(t, "mod3")

	f.runner.EnqueuePlan(&domain.Workspace{Module: mod1, Name: "prod"})
	f.runner.EnqueuePlan(&domain.Workspace{Module: mod2, Name: "prod"})
	mod3WS := &domain.Workspace{Module: mod3, Name: "prod"}
	f.runner.EnqueuePlan(mod3WS)
	mod3PlanID := TaskID(KindPlan, mod3WS.Key())

	// Collect every relevant event in one pass — waitFor/waitTerminal
	// discard non-matching events, so calling them one after another here
	// would drop whichever of these arrives out of the order queried.
	var initTerminal, planTerminal int
	initTerminalAtMod3Start := -1
	timeout := time.After(30 * time.Second)
	for initTerminal < 2 || planTerminal < 3 {
		select {
		case ev := <-f.runner.Events:
			if ev.TaskID() == mod3PlanID && ev.Phase == PhaseRunning && initTerminalAtMod3Start < 0 {
				initTerminalAtMod3Start = initTerminal
			}
			if ev.Kind == KindInit && ev.Phase.Terminal() {
				initTerminal++
			}
			if ev.Kind == KindPlan && ev.Phase.Terminal() {
				planTerminal++
			}
		case <-timeout:
			t.Fatal("timed out waiting for events")
		}
	}
	// mod3 never needs init, so it must not be stuck behind the two
	// serialized inits (both had already finished before it even started
	// would mean it waited on them).
	if initTerminalAtMod3Start < 0 {
		t.Fatal("mod3's plan never reported Running")
	}
	if initTerminalAtMod3Start >= 2 {
		t.Errorf("mod3's plan didn't start until %d inits had already finished", initTerminalAtMod3Start)
	}

	data, err := os.ReadFile(f.logFile)
	if err != nil {
		t.Fatal(err)
	}
	open := 0
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[4] != "init" {
			continue
		}
		switch fields[0] {
		case "start":
			open++
			if open > 1 {
				t.Fatalf("two init calls overlapped despite init_parallelism=1")
			}
		case "end":
			open--
		}
	}
}
