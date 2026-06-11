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
		runner:  New(parallelism, store),
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

// drain collects events until want of type T arrived or timeout.
func drainUntil[T any](t *testing.T, ch chan Event, want int) []T {
	t.Helper()
	var got []T
	timeout := time.After(30 * time.Second)
	for len(got) < want {
		select {
		case ev := <-ch:
			if v, ok := ev.(T); ok {
				got = append(got, v)
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %d events, got %d", want, len(got))
		}
	}
	return got
}

func TestEnumerateEmitsWorkspaces(t *testing.T) {
	f := newFixture(t, 2)
	m := f.newModule(t, "mod1")
	if !f.runner.EnqueueEnumerate(m) {
		t.Fatal("enqueue refused")
	}
	evs := drainUntil[EnumFinished](t, f.runner.Events, 1)
	if evs[0].Err != "" {
		t.Fatal(evs[0].Err)
	}
	if strings.Join(evs[0].Workspaces, ",") != "default,prod,staging" {
		t.Errorf("workspaces = %v", evs[0].Workspaces)
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
	drainUntil[EnumFinished](t, f.runner.Events, 1)
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
	evs := drainUntil[PlanFinished](t, f.runner.Events, 1)
	if evs[0].Err != "" {
		t.Fatal(evs[0].Err)
	}
	rec := evs[0].Record
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
	evs := drainUntil[PlanFinished](t, f.runner.Events, 1)
	if evs[0].Record.PlanExitCode != 0 {
		t.Fatalf("exit = %d", evs[0].Record.PlanExitCode)
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
	drainUntil[PlanFinished](t, f.runner.Events, 3)

	data, err := os.ReadFile(f.logFile)
	if err != nil {
		t.Fatal(err)
	}
	open := map[string]int{}        // module dir -> currently running plans
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
