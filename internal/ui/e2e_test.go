// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package ui

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/espoon-voltti/tfmux/internal/config"
	"github.com/espoon-voltti/tfmux/internal/gittest"
	"github.com/espoon-voltti/tfmux/internal/state"
	"github.com/espoon-voltti/tfmux/internal/tftest"
)

// TestEndToEnd drives the real program loop: discovery of a fixture git repo
// (itself the configured root),
// workspace enumeration through the runner with the fake terraform binary,
// planning a workspace, and quitting.
func TestEndToEnd(t *testing.T) {
	repo := gittest.Init(t)
	gittest.WriteFile(t, filepath.Join(repo, "envs", "prod", "main.tf"), "terraform {\n  backend \"s3\" {}\n}\n")
	bin := tftest.Write(t, t.TempDir())
	t.Setenv("TFMUX_FAKE_PLAN_EXIT", "2")
	showJSON := filepath.Join(t.TempDir(), "show.json")
	if err := os.WriteFile(showJSON,
		[]byte(`{"format_version":"1.2","resource_changes":[{"address":"a","change":{"actions":["create"]}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TFMUX_FAKE_SHOW_JSON", showJSON)

	cfg := config.Default()
	cfg.Roots = []string{repo}
	cfg.TerraformBin = bin
	model := NewModel(cfg, state.New(t.TempDir()))

	tm := teatest.NewTestModel(t, model, teatest.WithInitialTermSize(110, 30))

	// discovery + enumeration: repo, module and workspaces appear
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte("envs/prod")) && bytes.Contains(b, []byte("staging"))
	}, teatest.WithDuration(15*time.Second))

	// move to the first workspace row (repo, module, ws...) and plan it
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})

	// outstanding changes badge appears once the plan lands
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte("+1 ~0 -0"))
	}, teatest.WithDuration(15*time.Second))

	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	tm.WaitFinished(t, teatest.WithFinalTimeout(10*time.Second))
}
