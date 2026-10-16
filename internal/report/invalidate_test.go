// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package report

import (
	"testing"
	"time"

	"github.com/espoon-voltti/tfmux/internal/deps"
	"github.com/espoon-voltti/tfmux/internal/domain"
	"github.com/espoon-voltti/tfmux/internal/state"
	"github.com/espoon-voltti/tfmux/internal/tfexec"
)

// stateEstate wires base (holding a backend) and service (reading it with
// the given workspace expression) into one repo with workspaces prod and
// staging each, and returns the index over them.
func stateEstate(kind domain.WorkspaceExprKind, expr string) (ix *deps.Index, base, service *domain.Module) {
	repo := &domain.Repo{Path: "/iac/repo1", Name: "repo1"}
	addr := domain.StateAddress{Backend: "s3", Bucket: "tf", Key: "base.tfstate"}
	base = &domain.Module{Repo: repo, Path: "/iac/repo1/envs/base", RelPath: "envs/base", Deps: &domain.Deps{Backend: &addr}}
	service = &domain.Module{Repo: repo, Path: "/iac/repo1/envs/service", RelPath: "envs/service",
		Deps: &domain.Deps{RemoteStates: []domain.RemoteStateRef{{Name: "base", StateAddress: addr, WorkspaceKind: kind, WorkspaceExpr: expr}}}}
	for _, mod := range []*domain.Module{base, service} {
		for _, ws := range []string{"prod", "staging"} {
			mod.Workspaces = append(mod.Workspaces, &domain.Workspace{Module: mod, Name: ws})
		}
	}
	repo.Modules = []*domain.Module{base, service}
	return deps.Build([]*domain.Repo{repo}), base, service
}

func keysOf(wss []*domain.Workspace) []string {
	var out []string
	for _, ws := range wss {
		out = append(out, ws.Key())
	}
	return out
}

func TestDownstream(t *testing.T) {
	applied := &state.RunRecord{Workspace: "prod", PlanExitCode: tfexec.PlanChanges, OutputsChanged: true}
	cases := []struct {
		name string
		kind domain.WorkspaceExprKind
		expr string
		rec  *state.RunRecord
		want []string
	}{
		{"same workspace maps one to one", domain.WorkspaceSame, "", applied, []string{"/iac/repo1/envs/service//prod"}},
		{"literal workspace", domain.WorkspaceLiteral, "prod", applied, []string{"/iac/repo1/envs/service//prod", "/iac/repo1/envs/service//staging"}},
		{"literal workspace not applied", domain.WorkspaceLiteral, "staging", applied, nil},
		{"unresolvable expression reads every workspace", domain.WorkspaceUnknown, "local.x", applied, []string{"/iac/repo1/envs/service//prod", "/iac/repo1/envs/service//staging"}},
		{"outputs unchanged", domain.WorkspaceSame, "", &state.RunRecord{Workspace: "prod", PlanExitCode: tfexec.PlanChanges}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ix, base, _ := stateEstate(c.kind, c.expr)
			got := keysOf(Downstream(ix, base, c.rec))
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Errorf("got %v, want %v", got, c.want)
				}
			}
		})
	}
}

// Roots reading each other's state are marked one hop out and no further,
// and a root reading its own state is never its own downstream.
func TestDownstreamIsOneHopAndSkipsSelf(t *testing.T) {
	repo := &domain.Repo{Path: "/iac/repo1", Name: "repo1"}
	addrA := domain.StateAddress{Backend: "s3", Bucket: "tf", Key: "a.tfstate"}
	addrB := domain.StateAddress{Backend: "s3", Bucket: "tf", Key: "b.tfstate"}
	a := &domain.Module{Repo: repo, Path: "/iac/repo1/a", RelPath: "a",
		Deps: &domain.Deps{Backend: &addrA, RemoteStates: []domain.RemoteStateRef{
			{Name: "b", StateAddress: addrB, WorkspaceKind: domain.WorkspaceSame},
			{Name: "self", StateAddress: addrA, WorkspaceKind: domain.WorkspaceSame},
		}}}
	b := &domain.Module{Repo: repo, Path: "/iac/repo1/b", RelPath: "b",
		Deps: &domain.Deps{Backend: &addrB, RemoteStates: []domain.RemoteStateRef{{Name: "a", StateAddress: addrA, WorkspaceKind: domain.WorkspaceSame}}}}
	for _, mod := range []*domain.Module{a, b} {
		mod.Workspaces = []*domain.Workspace{{Module: mod, Name: "prod"}}
	}
	repo.Modules = []*domain.Module{a, b}
	ix := deps.Build([]*domain.Repo{repo})
	applied := &state.RunRecord{Workspace: "prod", OutputsChanged: true}
	if got := keysOf(Downstream(ix, a, applied)); len(got) != 1 || got[0] != "/iac/repo1/b//prod" {
		t.Errorf("downstream of a = %v, want b//prod only", got)
	}
}

func TestInvalidateDownstreamMarksPlannedRecordsOnce(t *testing.T) {
	ix, base, service := stateEstate(domain.WorkspaceSame, "")
	store := state.New(t.TempDir())
	if err := store.SaveRun(&state.RunRecord{ModulePath: service.Path, Workspace: "prod", PlanExitCode: tfexec.PlanClean}); err != nil {
		t.Fatal(err)
	}
	applied := &state.RunRecord{ModulePath: base.Path, Workspace: "prod", OutputsChanged: true}
	now := time.Now()
	marked := InvalidateDownstream(store, ix, base, applied, now)
	if len(marked) != 1 || marked[0] != service.Path+"//prod" {
		t.Fatalf("marked = %v", marked)
	}
	rec, _ := store.LoadRun(service.Path, "prod")
	if rec.Invalidated == nil || rec.Invalidated.By != "repo1/envs/base//prod" {
		t.Errorf("record not marked: %+v", rec)
	}
	if again := InvalidateDownstream(store, ix, base, applied, now.Add(time.Hour)); len(again) != 0 {
		t.Errorf("an already-marked record was marked again: %v", again)
	}
	if rec, _ := store.LoadRun(service.Path, "staging"); rec != nil {
		t.Error("an unplanned workspace must not get a record")
	}
}
