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

// fakeRunner records tmux invocations and serves canned responses. windowOpts
// simulates the per-window @tfmux_apply option set-option/list-windows read
// and write, keyed by window id.
type fakeRunner struct {
	calls       []call
	hasSession  bool
	windows     []string
	windowOpts  map[string]string
	selectFails bool
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
	case "set-option":
		if f.windowOpts == nil {
			f.windowOpts = map[string]string{}
		}
		// args: set-option -w -t <id> <opt> <value>
		f.windowOpts[args[3]] = args[5]
		return nil, nil
	case "list-windows":
		format := args[len(args)-1]
		if strings.Contains(format, tokenOpt) {
			var lines []string
			for id, tok := range f.windowOpts {
				lines = append(lines, id+" "+tok)
			}
			return []byte(strings.Join(lines, "\n")), nil
		}
		return []byte(strings.Join(f.windows, "\n")), nil
	case "select-window":
		if f.selectFails {
			return nil, errors.New("no such window")
		}
		return nil, nil
	}
	return nil, nil
}

func TestLaunchApplyCreatesSessionAndWindow(t *testing.T) {
	f := &fakeRunner{}
	c := NewWithRunner("tfmux", f.run)
	exitFile := filepath.Join(t.TempDir(), "apply.exit")
	id, token, err := c.LaunchApply(ApplySpec{
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
	if token == "" {
		t.Error("expected a non-empty token")
	}
	if got := f.windowOpts["@7"]; got != token {
		t.Errorf("window not stamped with the returned token: got %q, want %q", got, token)
	}
	var kinds []string
	for _, c := range f.calls {
		kinds = append(kinds, c.args[0])
	}
	want := "has-session,new-session,new-window,set-option"
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
	if _, _, err := c.LaunchApply(ApplySpec{ExitFile: exitFile, Workspace: "w"}); err != nil {
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

func TestWindowForFindsStampedWindow(t *testing.T) {
	f := &fakeRunner{hasSession: true, windowOpts: map[string]string{"@7": "mod//ws@123"}}
	c := NewWithRunner("tfmux", f.run)
	id, ok := c.WindowFor("mod//ws@123")
	if !ok || id != "@7" {
		t.Errorf("WindowFor = %q, %v", id, ok)
	}
}

// A window id can be recycled by a fresh tmux server and now belong to an
// unrelated window — WindowFor must not be fooled by the id existing, only
// by the token actually matching.
func TestWindowForRejectsRecycledID(t *testing.T) {
	f := &fakeRunner{hasSession: true, windowOpts: map[string]string{"@7": "some-other-apply@999"}}
	c := NewWithRunner("tfmux", f.run)
	if _, ok := c.WindowFor("mod//ws@123"); ok {
		t.Error("must not match a window carrying a different token")
	}
}

func TestWindowForNoSession(t *testing.T) {
	f := &fakeRunner{hasSession: false}
	c := NewWithRunner("tfmux", f.run)
	if _, ok := c.WindowFor("anything"); ok {
		t.Error("no session means no window")
	}
}

func TestAttachCmdFailsLoudlyWhenWindowGone(t *testing.T) {
	f := &fakeRunner{hasSession: true, selectFails: true}
	c := NewWithRunner("tfmux", f.run)
	if _, err := c.AttachCmd("@7"); err == nil {
		t.Error("expected an error when the window can't be selected")
	}
}

// Both branches must carry the window, and which branch runs depends on $TMUX
// — so pin it rather than inheriting whatever session the test process happens
// to run under. attach-session is the branch the original bug hid in: it used
// to attach to the bare session, landing the user on whatever window was
// selected there.
func TestAttachCmdCarriesWindowTarget(t *testing.T) {
	for _, tc := range []struct {
		name, tmuxEnv, wantVerb string
	}{
		{"outside tmux", "", "attach-session"},
		{"inside tmux", "/tmp/tmux-501/default,1,0", "switch-client"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TMUX", tc.tmuxEnv)
			f := &fakeRunner{hasSession: true}
			c := NewWithRunner("tfmux", f.run)
			cmd, err := c.AttachCmd("@7")
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(cmd.Args, " ")
			if !strings.Contains(joined, tc.wantVerb) {
				t.Errorf("want the %s branch, got: %v", tc.wantVerb, cmd.Args)
			}
			if !strings.Contains(joined, "tfmux:@7") {
				t.Errorf("attach command doesn't target the window: %v", cmd.Args)
			}
		})
	}
}

func TestShellQuoting(t *testing.T) {
	got := shq(`it's a "test" $HOME`)
	want := `'it'\''s a "test" $HOME'`
	if got != want {
		t.Errorf("shq = %s, want %s", got, want)
	}
}
