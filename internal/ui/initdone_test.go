// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package ui

import (
	"strings"
	"testing"

	"github.com/espoon-voltti/tfmux/internal/runner"
	"github.com/espoon-voltti/tfmux/internal/tftest"
)

func initDone(m *Model, mod string) {
	m.updateRunnerEvent(runner.Event{
		Kind: runner.KindInit, Key: mod, ModulePath: mod, Phase: runner.PhaseDone,
	})
}

// init -upgrade doesn't change the workspace list, and enumerating hits the
// backend — so a module that already has workspaces is left alone.
func TestInitDoneNoEnumerateWhenWorkspacesExist(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "default", "prod")
	initDone(m, mod.Path)
	if m.hasTask(runner.KindEnumerate, mod.Path) {
		t.Fatal("init -upgrade should not auto-enumerate when workspaces already exist")
	}
}

// A module with no workspaces yet (e.g. its first init) still gets enumerated,
// so the user sees something to act on.
func TestInitDoneEnumeratesWhenNoWorkspaces(t *testing.T) {
	m, mod := fixtureModel(t)
	mod.TFBin = tftest.Write(t, t.TempDir()) // queued enumerate runs against the fake
	initDone(m, mod.Path)
	if !m.hasTask(runner.KindEnumerate, mod.Path) {
		t.Fatal("init -upgrade should auto-enumerate when the module has no workspaces")
	}
	drain(t, m, runner.KindEnumerate, 1)
}

// A failed init leaves its error on the module row (headlined by terraform's
// "Error:" line, not the progress chatter before it) until an init succeeds.
func TestInitFailedShowsOnModuleRow(t *testing.T) {
	m, mod := fixtureModel(t)
	enumerated(t, m, mod, "default")
	m.updateRunnerEvent(runner.Event{
		Kind: runner.KindInit, Key: mod.Path, ModulePath: mod.Path, Phase: runner.PhaseFailed,
		Err:                "Initializing the backend...\n\nError: Failed to query available provider packages\n",
		CanceledDependents: 2,
	})
	row := m.renderModuleRow(mod)
	if !strings.Contains(row, "init error: Error: Failed to query available provider packages") {
		t.Errorf("module row = %q", row)
	}
	if !strings.Contains(m.status, "2 queued canceled") {
		t.Errorf("status = %q", m.status)
	}

	initDone(m, mod.Path)
	if row := m.renderModuleRow(mod); strings.Contains(row, "init error") {
		t.Errorf("init error still shown after a successful init: %q", row)
	}
}
