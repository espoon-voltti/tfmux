// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func drainInitFinished(t *testing.T, m *Model) {
	t.Helper()
	timeout := time.After(15 * time.Second)
	for {
		select {
		case ev := <-m.runner.Events:
			if ev.Kind == runner.KindInit && ev.Phase.Terminal() {
				return
			}
		case <-timeout:
			t.Fatal("timed out draining init events")
		}
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
	drainInitFinished(t, m)
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
	drainInitFinished(t, m)
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
	drainInitFinished(t, m)
}
