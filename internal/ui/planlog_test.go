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

// A completed plan log lands on its add/change/destroy summary at the end; a
// module log opens at the top.
func TestLogRendersAtFlaggedEnd(t *testing.T) {
	for _, bottom := range []bool{true, false} {
		m, mod := fixtureModel(t)
		m.detailBottom = bottom
		m.detailFollow = ""
		renderLog(m, runner.TaskID(runner.KindPlan, mod.Path+"//prod"))
		if m.detail.AtBottom() != bottom || m.detail.AtTop() == bottom {
			t.Errorf("detailBottom=%v: AtBottom=%v AtTop=%v", bottom, m.detail.AtBottom(), m.detail.AtTop())
		}
	}
}
