// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/espoon-voltti/tfmux/internal/domain"
	"github.com/espoon-voltti/tfmux/internal/manifest"
	"github.com/espoon-voltti/tfmux/internal/runner"
)

func TestForceDiscoveryAppliesManifestNoEnumerate(t *testing.T) {
	m, mod := fixtureModel(t)
	mod.Repo.ManifestPath = "/fake/repo1/" + manifest.FileName
	mod.ManifestListed = true
	mod.ManifestWorkspaces = []string{"staging", "prod"}

	m.updateDiscovery(discoveryMsg{repos: m.repos, force: true})

	if mod.WorkspaceState != domain.WorkspacesReady || len(mod.Workspaces) != 2 {
		t.Fatalf("module: %+v", mod)
	}
	if n := countTasks(m, runner.KindEnumerate); n != 0 {
		t.Errorf("enumerate tasks = %d, want 0", n)
	}
}

func TestManifestHiddenModuleAbsentUntilShowIgnored(t *testing.T) {
	m, mod := fixtureModel(t)
	mod.Repo.ManifestPath = "/fake/repo1/" + manifest.FileName
	// mod is not ManifestListed: it's absent from the manifest.

	m.reflow()
	for _, r := range m.rows {
		if r.kind == rowModule && r.mod == mod {
			t.Fatal("manifest-hidden module should not be visible by default")
		}
	}

	m.showIgnored = true
	m.reflow()
	view := m.View()
	if !strings.Contains(view, "not in manifest") {
		t.Error("expected 'not in manifest' badge once revealed")
	}
}

func TestRefreshWorkspacesRereadsManifest(t *testing.T) {
	m, mod := fixtureModel(t)
	root := t.TempDir()
	repo := mod.Repo
	repo.Path = root
	mod.RelPath = "base"
	mod.Path = filepath.Join(root, "base")
	repo.ManifestPath = filepath.Join(root, manifest.FileName)
	mod.ManifestListed = true
	mod.ManifestWorkspaces = []string{"old"}
	if err := os.WriteFile(filepath.Join(root, manifest.FileName),
		[]byte(`[{"root_module": "base", "workspaces": ["new1", "new2"]}]`), 0o644); err != nil {
		t.Fatal(err)
	}

	m.cursor = 1 // module row: fixtureModel's rows are [repo, module]
	cmd := keyPress(m, "w")
	if cmd == nil {
		t.Fatal("expected a cmd from 'w' on a manifest-listed module")
	}
	m.Update(cmd())

	if len(mod.ManifestWorkspaces) != 2 || mod.ManifestWorkspaces[0] != "new1" {
		t.Errorf("mod.ManifestWorkspaces = %v", mod.ManifestWorkspaces)
	}
	if len(mod.Workspaces) != 2 {
		t.Errorf("mod.Workspaces = %v", mod.Workspaces)
	}
}

func TestToggleIgnoreManifestHiddenIsNoOp(t *testing.T) {
	m, mod := fixtureModel(t)
	mod.Repo.ManifestPath = "/fake/repo1/" + manifest.FileName
	m.showIgnored = true
	m.reflow()

	found := false
	for i, r := range m.rows {
		if r.kind == rowModule && r.mod == mod {
			m.cursor = i
			found = true
		}
	}
	if !found {
		t.Fatal("manifest-hidden module row not found under showIgnored")
	}

	keyPress(m, "i")

	if m.ignore[mod.Path] {
		t.Error("toggleIgnore should not have set ignore on a manifest-hidden module")
	}
	if !strings.Contains(m.status, manifest.FileName) {
		t.Errorf("status = %q, want a hint mentioning %s", m.status, manifest.FileName)
	}
}

func TestInitDoneSkipsEnumerateForManifestListed(t *testing.T) {
	m, mod := fixtureModel(t)
	mod.Repo.ManifestPath = "/fake/repo1/" + manifest.FileName
	mod.ManifestListed = true
	mod.ManifestWorkspaces = []string{"default"}

	m.updateRunnerEvent(runner.Event{
		Kind: runner.KindInit, Key: mod.Path, ModulePath: mod.Path,
		Phase: runner.PhaseDone,
	})

	if n := countTasks(m, runner.KindEnumerate); n != 0 {
		t.Errorf("enumerate tasks = %d, want 0", n)
	}
}
