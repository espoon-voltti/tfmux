// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package tmuxctl

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type call struct{ args []string }

// fakeRunner records tmux invocations and serves canned responses.
type fakeRunner struct {
	calls      []call
	hasSession bool
	windows    []string
}

func (f *fakeRunner) run(args ...string) ([]byte, error) {
	f.calls = append(f.calls, call{args: args})
	switch args[0] {
	case "has-session":
		if f.hasSession {
			return nil, nil
		}
		return nil, errors.New("no session")
	case "new-session":
		f.hasSession = true
		return nil, nil
	case "new-window":
		return []byte("@7\n"), nil
	case "list-windows":
		return []byte(strings.Join(f.windows, "\n")), nil
	}
	return nil, nil
}

func TestLaunchApplyCreatesSessionAndWindow(t *testing.T) {
	f := &fakeRunner{}
	c := NewWithRunner("tfmux", f.run)
	exitFile := filepath.Join(t.TempDir(), "apply.exit")
	id, err := c.LaunchApply(ApplySpec{
		ModuleDir: "/work/iac/repo/envs/prod",
		Workspace: "prod",
		TFBin:     "terraform",
		PlanFile:  "/state/plan.tfplan",
		ExitFile:  exitFile,
		Name:      "repo/prod",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "@7" {
		t.Errorf("window id = %q", id)
	}
	var kinds []string
	for _, c := range f.calls {
		kinds = append(kinds, c.args[0])
	}
	want := "has-session,new-session,new-window"
	if strings.Join(kinds, ",") != want {
		t.Errorf("calls = %v, want %s", kinds, want)
	}
	script := f.calls[2].args[len(f.calls[2].args)-1]
	for _, frag := range []string{
		"cd '/work/iac/repo/envs/prod'",
		"TF_WORKSPACE='prod'",
		"'terraform' apply -input=false '/state/plan.tfplan'",
		exitFile + ".tmp",
		"read _",
	} {
		if !strings.Contains(script, frag) {
			t.Errorf("script missing %q:\n%s", frag, script)
		}
	}
}

func TestLaunchApplyRemovesStaleExitFile(t *testing.T) {
	f := &fakeRunner{hasSession: true}
	c := NewWithRunner("tfmux", f.run)
	exitFile := filepath.Join(t.TempDir(), "apply.exit")
	if err := os.WriteFile(exitFile, []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.LaunchApply(ApplySpec{ExitFile: exitFile, Workspace: "w"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(exitFile); !os.IsNotExist(err) {
		t.Error("stale exit file not removed")
	}
}

func TestListWindowIDs(t *testing.T) {
	f := &fakeRunner{hasSession: true, windows: []string{"@1", "@3"}}
	c := NewWithRunner("tfmux", f.run)
	ids, err := c.ListWindowIDs()
	if err != nil {
		t.Fatal(err)
	}
	if !ids["@1"] || !ids["@3"] || len(ids) != 2 {
		t.Errorf("ids = %v", ids)
	}
}

func TestListWindowIDsNoSession(t *testing.T) {
	f := &fakeRunner{hasSession: false}
	c := NewWithRunner("tfmux", f.run)
	ids, err := c.ListWindowIDs()
	if err != nil || len(ids) != 0 {
		t.Errorf("ids = %v, err = %v", ids, err)
	}
}

func TestShellQuoting(t *testing.T) {
	got := shq(`it's a "test" $HOME`)
	want := `'it'\''s a "test" $HOME'`
	if got != want {
		t.Errorf("shq = %s, want %s", got, want)
	}
}
