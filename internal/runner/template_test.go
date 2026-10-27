// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/espoon-voltti/tfmux/internal/domain"
)

const (
	wsInit  = `"$TFMUX_TF_BIN" init -backend-config=$TFMUX_WORKSPACE.hcl`
	wsPlan  = `"$TFMUX_TF_BIN" plan -var-file=$TFMUX_WORKSPACE.tfvars`
	wsApply = `"$TFMUX_TF_BIN" apply`
)

// newTemplateModule creates an uninitialised module with manifest templates
// and workspaces staging and prod.
func (f *fixture) newTemplateModule(t *testing.T, name string, tmpl *domain.CommandTemplates) *domain.Module {
	t.Helper()
	m := f.newUninitModule(t, name)
	m.ManifestListed = true
	m.ManifestWorkspaces = []string{"staging", "prod"}
	m.Templates = tmpl
	return m
}

// fakeCalls returns "<subcommand> <args…>" per fake terraform invocation, in
// order.
func (f *fixture) fakeCalls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(f.logFile)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		if fields := strings.Fields(line); len(fields) > 4 && fields[0] == "start" {
			if fields[3] != "<none>" {
				t.Errorf("TF_WORKSPACE set for a template module: %q", line)
			}
			out = append(out, strings.Join(fields[4:], " "))
		}
	}
	return out
}

func initsFor(calls []string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, "init ") {
			out = append(out, c)
		}
	}
	return out
}

func TestTemplatePlanInitsForWorkspaceThenPlans(t *testing.T) {
	f := newFixture(t, 2)
	m := f.newTemplateModule(t, "mod1", &domain.CommandTemplates{Init: wsInit, Plan: wsPlan})
	if !f.runner.EnqueuePlan(&domain.Workspace{Module: m, Name: "prod"}) {
		t.Fatal("enqueue refused")
	}
	ev := waitTerminal(t, f.runner.Events, KindPlan, 1)[0]
	if ev.Phase != PhaseDone {
		t.Fatalf("phase = %v, err = %q", ev.Phase, ev.Err)
	}
	calls := f.fakeCalls(t)
	if len(calls) < 2 || !strings.HasPrefix(calls[0], "init -backend-config=prod.hcl") || !strings.HasPrefix(calls[1], "plan -var-file=prod.tfvars") {
		t.Errorf("calls = %v", calls)
	}
	if ws, ok := f.store.LoadInitWorkspace(m.Path); !ok || ws != "prod" {
		t.Errorf("init marker = %q, %v", ws, ok)
	}
}

func TestTemplatePlanReinitsOnWorkspaceSwitch(t *testing.T) {
	f := newFixture(t, 2)
	m := f.newTemplateModule(t, "mod1", &domain.CommandTemplates{Init: wsInit, Plan: wsPlan})
	if err := os.MkdirAll(filepath.Join(m.Path, ".terraform"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SaveInitWorkspace(m.Path, "staging"); err != nil {
		t.Fatal(err)
	}
	prod := &domain.Workspace{Module: m, Name: "prod"}
	if !f.runner.EnqueuePlan(prod) {
		t.Fatal("enqueue refused")
	}
	if ev := waitTerminal(t, f.runner.Events, KindPlan, 1)[0]; ev.Phase != PhaseDone {
		t.Fatalf("phase = %v, err = %q", ev.Phase, ev.Err)
	}
	if inits := initsFor(f.fakeCalls(t)); len(inits) != 1 || !strings.Contains(inits[0], "prod.hcl") {
		t.Fatalf("inits after switching workspace = %v", inits)
	}

	if !f.runner.EnqueuePlan(prod) {
		t.Fatal("second enqueue refused")
	}
	if ev := waitTerminal(t, f.runner.Events, KindPlan, 1)[0]; ev.Phase != PhaseDone {
		t.Fatalf("phase = %v, err = %q", ev.Phase, ev.Err)
	}
	if inits := initsFor(f.fakeCalls(t)); len(inits) != 1 {
		t.Errorf("a plan for the initialised workspace re-ran init: %v", inits)
	}
}

// Two workspaces of one workspace-dependent module planned together share the
// per-module init task without either being told "already initialised" for
// the other's workspace.
func TestTemplateTwoWorkspacesShareOneInitQueue(t *testing.T) {
	f := newFixture(t, 2)
	m := f.newTemplateModule(t, "mod1", &domain.CommandTemplates{Init: wsInit, Plan: wsPlan})
	for _, ws := range []string{"staging", "prod"} {
		if !f.runner.EnqueuePlan(&domain.Workspace{Module: m, Name: ws}) {
			t.Fatalf("enqueue %s refused", ws)
		}
	}
	for _, ev := range waitTerminal(t, f.runner.Events, KindPlan, 2) {
		if ev.Phase != PhaseDone {
			t.Errorf("%s: phase = %v, err = %q", ev.Key, ev.Phase, ev.Err)
		}
	}
	calls := f.fakeCalls(t)
	if inits := initsFor(calls); len(inits) != 2 {
		t.Errorf("inits = %v, want one per workspace", inits)
	}
	lastInit := ""
	for _, c := range calls {
		switch {
		case strings.HasPrefix(c, "init "):
			lastInit = c
		case strings.HasPrefix(c, "plan "):
			ws := strings.TrimSuffix(strings.TrimPrefix(strings.Fields(c)[1], "-var-file="), ".tfvars")
			if !strings.Contains(lastInit, "-backend-config="+ws+".hcl") {
				t.Errorf("plan for %s ran after %q", ws, lastInit)
			}
		}
	}
}

func TestTemplateOutputInitsForWorkspace(t *testing.T) {
	f := newFixture(t, 2)
	m := f.newTemplateModule(t, "mod1", &domain.CommandTemplates{Init: wsInit})
	if err := os.MkdirAll(filepath.Join(m.Path, ".terraform"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SaveInitWorkspace(m.Path, "staging"); err != nil {
		t.Fatal(err)
	}
	if !f.runner.EnqueueOutput(&domain.Workspace{Module: m, Name: "prod"}) {
		t.Fatal("enqueue refused")
	}
	if ev := waitTerminal(t, f.runner.Events, KindOutput, 1)[0]; ev.Phase != PhaseDone {
		t.Fatalf("phase = %v, err = %q", ev.Phase, ev.Err)
	}
	calls := f.fakeCalls(t)
	if len(calls) != 2 || !strings.HasPrefix(calls[0], "init -backend-config=prod.hcl") || !strings.HasPrefix(calls[1], "output") {
		t.Errorf("calls = %v", calls)
	}
}

// An init template that doesn't mention the workspace initialises the module
// once for every workspace and keeps no marker.
func TestTemplateNonWorkspaceInitNoMarker(t *testing.T) {
	f := newFixture(t, 2)
	m := f.newTemplateModule(t, "mod1", &domain.CommandTemplates{Init: `"$TFMUX_TF_BIN" init`, Plan: wsPlan})
	for _, ws := range []string{"staging", "prod"} {
		if !f.runner.EnqueuePlan(&domain.Workspace{Module: m, Name: ws}) {
			t.Fatalf("enqueue %s refused", ws)
		}
	}
	for _, ev := range waitTerminal(t, f.runner.Events, KindPlan, 2) {
		if ev.Phase != PhaseDone {
			t.Errorf("%s: phase = %v, err = %q", ev.Key, ev.Phase, ev.Err)
		}
	}
	if inits := initsFor(f.fakeCalls(t)); len(inits) != 1 {
		t.Errorf("inits = %v, want exactly one", inits)
	}
	if ws, ok := f.store.LoadInitWorkspace(m.Path); ok {
		t.Errorf("marker written for a workspace-independent init: %q", ws)
	}
}

func TestInitUpgradeTemplateUsesMarkerThenFirstWorkspace(t *testing.T) {
	f := newFixture(t, 2)
	m := f.newTemplateModule(t, "mod1", &domain.CommandTemplates{Init: wsInit})
	if !f.runner.EnqueueInitUpgrade(m) {
		t.Fatal("enqueue refused")
	}
	if ev := waitTerminal(t, f.runner.Events, KindInit, 1)[0]; ev.Phase != PhaseDone {
		t.Fatalf("phase = %v, err = %q", ev.Phase, ev.Err)
	}
	calls := f.fakeCalls(t)
	if len(calls) != 1 || !strings.Contains(calls[0], "staging.hcl") || !strings.Contains(calls[0], "-upgrade") {
		t.Errorf("without a marker, init -upgrade ran as %v; want the first manifest workspace", calls)
	}

	if err := f.store.SaveInitWorkspace(m.Path, "prod"); err != nil {
		t.Fatal(err)
	}
	if !f.runner.EnqueueInitUpgrade(m) {
		t.Fatal("second enqueue refused")
	}
	if ev := waitTerminal(t, f.runner.Events, KindInit, 1)[0]; ev.Phase != PhaseDone {
		t.Fatalf("phase = %v, err = %q", ev.Phase, ev.Err)
	}
	calls = f.fakeCalls(t)
	if len(calls) != 2 || !strings.Contains(calls[1], "prod.hcl") {
		t.Errorf("with a marker, init -upgrade ran as %v; want the marker's workspace", calls)
	}
}

// applyCalls applies ws and returns the fake terraform calls it made.
func (f *fixture) applyCalls(t *testing.T, m *domain.Module, ws string) []string {
	t.Helper()
	f.readyPlan(t, m, ws)
	if !f.runner.EnqueueApply(&domain.Workspace{Module: m, Name: ws}, "") {
		t.Fatal("enqueue refused")
	}
	ev := waitTerminal(t, f.runner.Events, KindApply, 1)[0]
	if ev.Phase != PhaseDone || ev.Record == nil || ev.Record.Apply == nil || ev.Record.Apply.ExitCode == nil || *ev.Record.Apply.ExitCode != 0 {
		t.Fatalf("phase = %v, err = %q, record = %+v", ev.Phase, ev.Err, ev.Record)
	}
	return f.fakeCalls(t)
}

// An apply for a workspace the module dir is not initialised for defers to
// an init for that workspace first, like a plan does.
func TestApplyTemplateInitsForWorkspaceWhenMarkerMismatch(t *testing.T) {
	f := newFixture(t, 1)
	m := f.newTemplateModule(t, "mod1", &domain.CommandTemplates{Init: wsInit, Apply: wsApply})
	if err := os.MkdirAll(filepath.Join(m.Path, ".terraform"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SaveInitWorkspace(m.Path, "staging"); err != nil {
		t.Fatal(err)
	}
	calls := f.applyCalls(t, m, "prod")
	if len(calls) != 2 || !strings.HasPrefix(calls[0], "init -backend-config=prod.hcl") || !strings.HasPrefix(calls[1], "apply -input=false -no-color ") {
		t.Errorf("calls = %v", calls)
	}
	if ws, ok := f.store.LoadInitWorkspace(m.Path); !ok || ws != "prod" {
		t.Errorf("marker after apply = %q, %v; want prod", ws, ok)
	}
}

func TestApplyTemplateSkipsInitWhenMarkerMatches(t *testing.T) {
	f := newFixture(t, 1)
	m := f.newTemplateModule(t, "mod1", &domain.CommandTemplates{Init: wsInit, Apply: wsApply})
	if err := os.MkdirAll(filepath.Join(m.Path, ".terraform"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SaveInitWorkspace(m.Path, "prod"); err != nil {
		t.Fatal(err)
	}
	calls := f.applyCalls(t, m, "prod")
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "apply -input=false -no-color ") {
		t.Errorf("calls = %v", calls)
	}
	if ws, ok := f.store.LoadInitWorkspace(m.Path); !ok || ws != "prod" {
		t.Errorf("marker changed by an apply that needed no init: %q, %v", ws, ok)
	}
}

// Every workspace of a template module queued at once: each init serves the
// workspace queued first, which then plans, so plans complete in queue order
// and each one needs exactly one init. A deferred task that lost its queue
// position would instead have its init claimed by the next workspace in line
// and alternate finished and starved workspaces.
func TestTemplateManyWorkspacesPlanInQueueOrder(t *testing.T) {
	f := newFixture(t, 2)
	m := f.newTemplateModule(t, "mod1", &domain.CommandTemplates{Init: wsInit, Plan: wsPlan})
	m.ManifestWorkspaces = []string{"dev", "test", "prod", "hk-test", "hk-prod", "ka-test"}
	for _, ws := range m.ManifestWorkspaces {
		if !f.runner.EnqueuePlan(&domain.Workspace{Module: m, Name: ws}) {
			t.Fatalf("enqueue %s refused", ws)
		}
	}
	var order []string
	for _, ev := range waitTerminal(t, f.runner.Events, KindPlan, len(m.ManifestWorkspaces)) {
		if ev.Phase != PhaseDone {
			t.Errorf("%s: phase = %v, err = %q", ev.Key, ev.Phase, ev.Err)
		}
		order = append(order, ev.Record.Workspace)
	}
	if got, want := strings.Join(order, " "), strings.Join(m.ManifestWorkspaces, " "); got != want {
		t.Errorf("plans finished in order %q, want %q", got, want)
	}
	calls := f.fakeCalls(t)
	if inits := initsFor(calls); len(inits) != len(m.ManifestWorkspaces) {
		t.Errorf("inits = %v, want one per workspace", inits)
	}
	lastInit := ""
	for _, c := range calls {
		switch {
		case strings.HasPrefix(c, "init "):
			lastInit = c
		case strings.HasPrefix(c, "plan "):
			ws := strings.TrimSuffix(strings.TrimPrefix(strings.Fields(c)[1], "-var-file="), ".tfvars")
			if !strings.Contains(lastInit, "-backend-config="+ws+".hcl") {
				t.Errorf("plan for %s ran after %q", ws, lastInit)
			}
		}
	}
}
