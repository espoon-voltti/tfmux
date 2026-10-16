// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/espoon-voltti/tfmux/internal/deps"
	"github.com/espoon-voltti/tfmux/internal/domain"
	"github.com/espoon-voltti/tfmux/internal/runner"
	"github.com/espoon-voltti/tfmux/internal/tftest"
)

const wafV16 = "git::https://github.com/org/shared.git//modules/waf?ref=v16"

// outdatedPinFixture gives the fixture module a real directory whose main.tf
// pins waf at v16 while the shared repo's checkout is known to have v18.
func outdatedPinFixture(t *testing.T) (*Model, *domain.Module, string) {
	t.Helper()
	m, mod := fixtureModel(t)
	repoDir := t.TempDir()
	modDir := filepath.Join(repoDir, "envs", "prod")
	if err := os.MkdirAll(modDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mainTF := filepath.Join(modDir, "main.tf")
	if err := os.WriteFile(mainTF, []byte("module \"waf\" {\n  source = \""+wafV16+"\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mod.Repo.Path, mod.Path = repoDir, modDir
	mod.TFBin = tftest.Write(t, t.TempDir())
	mod.Deps = deps.Scan(modDir, repoDir)
	m.behind[deps.RefKey(mod.Deps.Git[0])] = deps.Behind{Ref: "v16", NewerTag: "v18"}
	m.reflow()
	m.cursor = 1 // module row
	return m, mod, mainTF
}

// I on a repo row queues init -upgrade for every module in the repo, or
// under a filter only for the modules left visible.
func TestInitUpgradeRepoQueuesVisibleModules(t *testing.T) {
	for _, c := range []struct {
		filter string
		want   []string
	}{{"", []string{"a", "b"}}, {"b", []string{"b"}}} {
		t.Run("filter "+c.filter, func(t *testing.T) {
			m := twoRepoModel(t)
			m.filterText = c.filter
			m.reflow()
			m.cursor = 0 // repo1 row
			m.initUpgradeCurrent()
			for _, mod := range m.repos[0].Modules {
				want := slices.Contains(c.want, mod.RelPath)
				if m.hasTask(runner.KindInit, mod.Path) != want {
					t.Errorf("init -upgrade queued for %s = %v, want %v", mod.RelPath, !want, want)
				}
			}
			if !strings.Contains(m.status, fmt.Sprintf("%d module", len(c.want))) {
				t.Errorf("status = %q", m.status)
			}
			drain(t, m, runner.KindInit, len(c.want))
		})
	}
}

func TestInitUpgradeOffersToBumpOutdatedPins(t *testing.T) {
	m, mod, mainTF := outdatedPinFixture(t)
	if cmd := keyPress(m, "I"); cmd != nil || m.confirmUpgrade == nil {
		t.Fatal("I on a module with an outdated pin should prompt, not queue")
	}
	if m.hasTask(runner.KindInit, mod.Path) {
		t.Fatal("init must wait for the answer")
	}
	if v := m.View(); !strings.Contains(v, "update 1 outdated module reference(s)") {
		t.Errorf("prompt missing from view:\n%s", v)
	}

	cmd := keyPress(m, "y")
	if cmd == nil {
		t.Fatal("y should rewrite the sources")
	}
	msg, ok := cmd().(refsUpdatedMsg)
	if !ok {
		t.Fatalf("rewrite produced %T", msg)
	}
	if msg.err != nil || msg.refs != 1 || msg.files != 1 {
		t.Fatalf("rewrite result: %+v", msg)
	}
	m.Update(msg)
	if got, _ := os.ReadFile(mainTF); !strings.Contains(string(got), "?ref=v18\"") {
		t.Errorf("main.tf not rewritten:\n%s", got)
	}
	if mod.Deps.Git[0].Ref != "v18" {
		t.Errorf("deps not rescanned: ref = %q", mod.Deps.Git[0].Ref)
	}
	if !m.hasTask(runner.KindInit, mod.Path) {
		t.Error("init -upgrade should be queued after the rewrite")
	}
	if v := m.View(); strings.Contains(v, "↑ waf") {
		t.Errorf("behind badge should be gone once the pin is bumped:\n%s", v)
	}
	if !strings.Contains(m.status, "updated 1 module reference(s)") || !strings.Contains(m.status, "init -upgrade queued") {
		t.Errorf("status = %q", m.status)
	}
	drain(t, m, runner.KindInit, 1)
}

func TestInitUpgradeDeclinedKeepsPins(t *testing.T) {
	m, mod, mainTF := outdatedPinFixture(t)
	keyPress(m, "I")
	keyPress(m, "n")
	if m.confirmUpgrade != nil {
		t.Error("prompt should be dismissed")
	}
	if !m.hasTask(runner.KindInit, mod.Path) {
		t.Error("declining should still queue the init -upgrade")
	}
	if got, _ := os.ReadFile(mainTF); !strings.Contains(string(got), "?ref=v16\"") {
		t.Errorf("main.tf must be untouched:\n%s", got)
	}
	drain(t, m, runner.KindInit, 1)
}

func TestInitUpgradeWithoutOutdatedPinsQueuesDirectly(t *testing.T) {
	m, mod, _ := outdatedPinFixture(t)
	m.behind = map[string]deps.Behind{}
	keyPress(m, "I")
	if m.confirmUpgrade != nil {
		t.Error("nothing to bump: no prompt expected")
	}
	if !m.hasTask(runner.KindInit, mod.Path) {
		t.Error("init -upgrade should be queued")
	}
	drain(t, m, runner.KindInit, 1)
}
