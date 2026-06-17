// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package ui

import (
	"strings"
	"testing"

	"github.com/espoon-voltti/tfmux/internal/domain"
	"github.com/espoon-voltti/tfmux/internal/runner"
)

// renderLog feeds updateLog a log longer than the viewport so AtTop/AtBottom
// distinguish the initial scroll position.
func renderLog(m *Model, id string) {
	m.detail.Width, m.detail.Height = 100, 10
	body := strings.Repeat("Refreshing state...\n", 100)
	m.updateLog(logMsg{id: id, content: body + "Plan: 1 to add, 0 to change, 0 to destroy."})
}

// openLog flags a plan log to start at the bottom; a module log does not.
func TestOpenLogStartsBottomForPlanOnly(t *testing.T) {
	m, mod := fixtureModel(t)
	ws := &domain.Workspace{Module: mod, Name: "prod"}

	m.openLog(runner.KindPlan, ws.Key())
	if !m.detailBottom {
		t.Error("plan log should be flagged to open at the bottom")
	}

	m.openLog(runner.KindEnumerate, mod.Path)
	if m.detailBottom {
		t.Error("module (enumerate) log should open at the top, not the bottom")
	}
}

// A completed plan log lands on its add/change/destroy summary at the end.
func TestPlanLogRendersAtBottom(t *testing.T) {
	m, mod := fixtureModel(t)
	id := runner.TaskID(runner.KindPlan, mod.Path+"//prod")
	m.detailBottom = true
	m.detailFollow = ""
	renderLog(m, id)
	if !m.detail.AtBottom() {
		t.Fatal("plan log should open scrolled to the bottom")
	}
}

// A module log still opens at the top.
func TestModuleLogRendersAtTop(t *testing.T) {
	m, mod := fixtureModel(t)
	id := runner.TaskID(runner.KindEnumerate, mod.Path)
	m.detailBottom = false
	m.detailFollow = ""
	renderLog(m, id)
	if !m.detail.AtTop() {
		t.Fatal("module log should open at the top")
	}
}
