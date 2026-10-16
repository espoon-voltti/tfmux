// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/espoon-voltti/tfmux/internal/deps"
	"github.com/espoon-voltti/tfmux/internal/domain"
	"github.com/espoon-voltti/tfmux/internal/runner"
	"github.com/espoon-voltti/tfmux/internal/state"
	"github.com/espoon-voltti/tfmux/internal/tfexec"
)

// pendingFixture: envs/prod has prod (never planned) and staging (planned
// clean, current); envs/base has one clean workspace but pins waf behind.
func pendingFixture(t *testing.T) (m *Model, prod, base *domain.Module) {
	t.Helper()
	m, prod = fixtureModel(t)
	enumerated(t, m, prod, "prod", "staging")
	base = &domain.Module{Repo: prod.Repo, Path: "/iac/repo1/envs/base", RelPath: "envs/base", TFBin: "terraform"}
	base.Deps = &domain.Deps{Git: []domain.GitRef{{Repo: "github.com/org/shared", Subdir: "modules/waf", Ref: "v16", Via: "waf"}}}
	prod.Repo.Modules = append(prod.Repo.Modules, base)
	enumerated(t, m, base, "default")
	m.behind[deps.RefKey(base.Deps.Git[0])] = deps.Behind{Ref: "v16", NewerTag: "v18"}
	for _, key := range []string{prod.Path + "//staging", base.Path + "//default"} {
		modPath, ws, _ := splitWSKey(key)
		m.runs[key] = &state.RunRecord{ModulePath: modPath, Workspace: ws, PlanFinished: time.Now(), PlanExitCode: tfexec.PlanClean, Fingerprint: "f"}
	}
	m.fingerprints[prod.Path] = "f"
	m.fingerprints[base.Path] = "f"
	m.reflow()
	return m, prod, base
}

func rowIDs(m *Model) []string {
	var ids []string
	for _, r := range m.rows {
		ids = append(ids, r.rowID())
	}
	return ids
}

func TestPendingOnlyShowsAttentionItemsExpanded(t *testing.T) {
	m, prod, base := pendingFixture(t)
	m.collapsed[prod.Repo.Path] = true
	m.collapsed[prod.Path] = true
	m.reflow()
	keyPress(m, "!")
	want := strings.Join([]string{
		repoRowID(prod.Repo.Path),
		moduleRowID(prod.Path),
		workspaceRowID(prod.Path + "//prod"),
		moduleRowID(base.Path),
	}, "\n")
	if got := strings.Join(rowIDs(m), "\n"); got != want {
		t.Errorf("rows:\n%s\nwant:\n%s", got, want)
	}
	if v := m.View(); !strings.Contains(v, "pending only") {
		t.Errorf("title bar should flag the filter:\n%s", v)
	}
	keyPress(m, "!")
	if n := len(m.rows); n != 6 {
		t.Errorf("toggling off should restore all 6 rows, got %d", n)
	}
}

func TestPendingOnlyIsASnapshot(t *testing.T) {
	m, prod, _ := pendingFixture(t)
	keyPress(m, "!")
	key := prod.Path + "//prod"
	rec := &state.RunRecord{ModulePath: prod.Path, Workspace: "prod", PlanFinished: time.Now(), PlanExitCode: tfexec.PlanClean, Fingerprint: "f"}
	m.updateRunnerEvent(runner.Event{Kind: runner.KindPlan, Key: key, ModulePath: prod.Path, Phase: runner.PhaseDone, Record: rec})
	m.reflow()
	if !strings.Contains(strings.Join(rowIDs(m), "\n"), workspaceRowID(key)) {
		t.Error("a workspace planned clean must stay listed until the filter is toggled")
	}
	keyPress(m, "!")
	keyPress(m, "!")
	if strings.Contains(strings.Join(rowIDs(m), "\n"), workspaceRowID(key)) {
		t.Error("re-toggling should drop the now-clean workspace")
	}
}

func TestPendingOnlyScopesRepoRowActions(t *testing.T) {
	m, prod, _ := pendingFixture(t)
	keyPress(m, "!")
	m.cursor = 0
	wss := m.workspacesUnder(m.rows[0])
	if len(wss) != 1 || wss[0].Key() != prod.Path+"//prod" {
		t.Errorf("repo-row actions should target only the pending workspace, got %d", len(wss))
	}
	mods := m.modulesUnder(m.rows[0])
	if len(mods) != 2 {
		t.Errorf("repo-row module actions should cover both pending modules, got %d", len(mods))
	}
}

func TestPendingOnlyWithNothingPending(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "prod")
	key := mod.Path + "//prod"
	m.runs[key] = &state.RunRecord{ModulePath: mod.Path, Workspace: "prod", PlanFinished: time.Now(), PlanExitCode: tfexec.PlanClean, Fingerprint: "f"}
	m.fingerprints[mod.Path] = "f"
	m.reflow()
	keyPress(m, "!")
	if len(m.rows) != 0 {
		t.Errorf("expected an empty tree, got %d rows", len(m.rows))
	}
	if v := m.View(); !strings.Contains(v, "nothing pending") {
		t.Errorf("empty-tree hint missing:\n%s", v)
	}
}
