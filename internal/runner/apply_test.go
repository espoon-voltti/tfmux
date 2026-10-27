// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package runner

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/espoon-voltti/tfmux/internal/domain"
	"github.com/espoon-voltti/tfmux/internal/state"
	"github.com/espoon-voltti/tfmux/internal/tfexec"
)

// readyPlan leaves ws with a plan record that has changes and a plan file on
// disk, as a finished plan would.
func (f *fixture) readyPlan(t *testing.T, m *domain.Module, ws string) {
	t.Helper()
	planFile, err := f.store.PlanFilePath(m.Path, ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planFile, []byte("fake plan"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &state.RunRecord{ModulePath: m.Path, Workspace: ws, PlanExitCode: tfexec.PlanChanges, PlanFinished: time.Now()}
	if err := f.store.SaveRun(rec); err != nil {
		t.Fatal(err)
	}
}

type fakeCall struct{ workspace, args string }

// fakeCallsWithWorkspace returns each fake terraform invocation with the
// TF_WORKSPACE it saw ("<none>" when unset), in order.
func (f *fixture) fakeCallsWithWorkspace(t *testing.T) []fakeCall {
	t.Helper()
	data, err := os.ReadFile(f.logFile)
	if err != nil {
		t.Fatal(err)
	}
	var out []fakeCall
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		if fields := strings.Fields(line); len(fields) > 4 && fields[0] == "start" {
			out = append(out, fakeCall{workspace: fields[3], args: strings.Join(fields[4:], " ")})
		}
	}
	return out
}

func (f *fixture) applyTerminal(t *testing.T, m *domain.Module, ws string) Event {
	t.Helper()
	if !f.runner.EnqueueApply(&domain.Workspace{Module: m, Name: ws}, "") {
		t.Fatal("enqueue refused")
	}
	return waitTerminal(t, f.runner.Events, KindApply, 1)[0]
}

func TestApplyRecordsOutcomeAndDiscardsPlan(t *testing.T) {
	f := newFixture(t, 1)
	m := f.newModule(t, "mod1")
	f.readyPlan(t, m, "prod")

	ev := f.applyTerminal(t, m, "prod")
	if ev.Phase != PhaseDone || ev.Record == nil {
		t.Fatalf("phase = %v, err = %q, record = %v", ev.Phase, ev.Err, ev.Record)
	}
	a := ev.Record.Apply
	if a == nil || a.ExitCode == nil || *a.ExitCode != 0 || a.Finished == nil || a.Aborted {
		t.Errorf("apply record = %+v", a)
	}
	saved, _ := f.store.LoadRun(m.Path, "prod")
	if saved == nil || saved.Apply == nil || saved.Apply.ExitCode == nil || *saved.Apply.ExitCode != 0 {
		t.Errorf("saved record = %+v", saved)
	}
	if f.store.HasPlanFile(m.Path, "prod") {
		t.Error("plan file kept after a successful apply")
	}
	logPath, _ := f.store.ApplyLogPath(m.Path, "prod")
	if log, err := os.ReadFile(logPath); err != nil || !strings.Contains(string(log), "Apply complete!") {
		t.Errorf("apply log = %q, %v", log, err)
	}
	calls := f.fakeCallsWithWorkspace(t)
	if len(calls) != 1 || calls[0].workspace != "prod" || !strings.HasPrefix(calls[0].args, "apply -input=false -no-color ") {
		t.Errorf("calls = %+v", calls)
	}
}

func TestApplyFailureKeepsPlan(t *testing.T) {
	f := newFixture(t, 1)
	t.Setenv("TFMUX_FAKE_APPLY_EXIT", "1")
	m := f.newModule(t, "mod1")
	f.readyPlan(t, m, "prod")

	ev := f.applyTerminal(t, m, "prod")
	if ev.Phase != PhaseDone || ev.Record == nil || ev.Record.Apply == nil {
		t.Fatalf("phase = %v, err = %q, record = %v", ev.Phase, ev.Err, ev.Record)
	}
	if a := ev.Record.Apply; a.ExitCode == nil || *a.ExitCode != 1 {
		t.Errorf("apply record = %+v", a)
	}
	if !f.store.HasPlanFile(m.Path, "prod") {
		t.Error("plan file discarded after a failed apply")
	}
}

// Canceling a running apply interrupts terraform and records the apply as
// aborted: the plan may be partially applied, so it is no longer usable.
func TestApplyCancelMarksAborted(t *testing.T) {
	f := newFixture(t, 1)
	t.Setenv("TFMUX_FAKE_SLEEP", "10")
	m := f.newModule(t, "mod1")
	f.readyPlan(t, m, "prod")
	key := m.Path + "//prod"

	if !f.runner.EnqueueApply(&domain.Workspace{Module: m, Name: "prod"}, "") {
		t.Fatal("enqueue refused")
	}
	waitFor(t, f.runner.Events, func(e Event) bool { return e.Kind == KindApply && e.Phase == PhaseRunning })
	time.Sleep(200 * time.Millisecond) // let the fake start sleeping
	f.runner.Cancel(KindApply, key)

	ev := waitTerminal(t, f.runner.Events, KindApply, 1)[0]
	if ev.Phase != PhaseCanceled || ev.Record == nil || ev.Record.Apply == nil || !ev.Record.Apply.Aborted {
		t.Fatalf("phase = %v, record = %+v", ev.Phase, ev.Record)
	}
	saved, _ := f.store.LoadRun(m.Path, "prod")
	if saved == nil || saved.Apply == nil || !saved.Apply.Aborted || saved.Apply.ExitCode != nil {
		t.Errorf("saved record = %+v", saved)
	}
}

func TestApplyWithoutRecordFails(t *testing.T) {
	f := newFixture(t, 1)
	m := f.newModule(t, "mod1")
	ev := f.applyTerminal(t, m, "prod")
	if ev.Phase != PhaseFailed || !strings.Contains(ev.Err, "no plan record") {
		t.Errorf("phase = %v, err = %q", ev.Phase, ev.Err)
	}
}

// Shutdown interrupts running tasks and returns once they have stopped,
// well before the fake's sleep would have ended.
func TestShutdownInterruptsRunningTasks(t *testing.T) {
	f := newFixture(t, 2)
	t.Setenv("TFMUX_FAKE_SLEEP", "10")
	m := f.newModule(t, "mod1")
	queued := f.newModule(t, "mod2")

	f.runner.EnqueuePlan(&domain.Workspace{Module: m, Name: "prod"})
	waitFor(t, f.runner.Events, func(e Event) bool { return e.Kind == KindPlan && e.Phase == PhaseRunning })
	f.runner.EnqueueEnumerate(queued)

	started := time.Now()
	if n := f.runner.Shutdown(5 * time.Second); n != 0 {
		t.Errorf("%d task(s) still running after shutdown", n)
	}
	if time.Since(started) > 4*time.Second {
		t.Error("shutdown waited for the sleep instead of interrupting it")
	}
	if f.runner.Running(queued.Path) {
		t.Error("queued task survived shutdown")
	}
}

// After Shutdown the runner accepts nothing: a task enqueued by a straggling
// caller, or the init a task deferred to while being interrupted, would
// outlive the exit.
func TestShutdownRefusesLaterWork(t *testing.T) {
	f := newFixture(t, 2)
	m := f.newModule(t, "mod1")
	if n := f.runner.Shutdown(time.Second); n != 0 {
		t.Fatalf("%d task(s) running in an idle runner", n)
	}
	if f.runner.EnqueuePlan(&domain.Workspace{Module: m, Name: "prod"}) {
		t.Error("enqueue accepted after shutdown")
	}
	if f.runner.Running(m.Path + "//prod") {
		t.Error("a task is in flight after shutdown")
	}
}
