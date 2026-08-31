// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

// Package tmuxctl drives a tmux server for terraform applies.
//
// Applies are the interactive, long-running, must-not-die operations: each
// runs in its own tmux window so the user can attach, answer prompts, and
// the run survives tfmux exiting. Completion is reported through an exit
// file written atomically by a shell wrapper; tfmux polls for it.
package tmuxctl

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// tokenOpt is the tmux window option carrying an apply's durable identity —
// see LaunchApply and WindowFor.
const tokenOpt = "@tfmux_apply"

// Ctl manages one tmux session. The zero value is unusable; call New.
type Ctl struct {
	Session string
	Bin     string

	// run is swappable for tests; production uses exec.Command.
	run func(args ...string) ([]byte, error)
}

func New(session string) *Ctl {
	c := &Ctl{Session: session, Bin: "tmux"}
	c.run = func(args ...string) ([]byte, error) {
		out, err := exec.Command(c.Bin, args...).CombinedOutput()
		if err != nil {
			return out, fmt.Errorf("tmux %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return out, nil
	}
	return c
}

// NewWithRunner constructs a Ctl with a fake command runner, for tests.
func NewWithRunner(session string, run func(args ...string) ([]byte, error)) *Ctl {
	return &Ctl{Session: session, Bin: "tmux", run: run}
}

// Available reports whether the tmux binary is on PATH.
func Available() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

// ensureSession creates the detached session if it doesn't exist.
func (c *Ctl) ensureSession() error {
	if _, err := c.run("has-session", "-t", "="+c.Session); err == nil {
		return nil
	}
	_, err := c.run("new-session", "-d", "-s", c.Session)
	return err
}

// shq single-quotes s for /bin/sh.
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ApplySpec describes one apply to launch.
type ApplySpec struct {
	ModuleDir string
	Workspace string
	TFBin     string
	PlanFile  string
	ExitFile  string // removed before launch; appears atomically when done
	Name      string // window name, display only
}

// applyScript builds the wrapper: run apply, write the exit code atomically,
// keep the window open on failure so the error stays inspectable. On success
// the window closes itself.
func applyScript(s ApplySpec) string {
	exit, tmp := shq(s.ExitFile), shq(s.ExitFile+".tmp")
	return fmt.Sprintf(
		`cd %s || { echo "tfmux: cd failed"; printf '%%s' 127 > %s && mv %s %s; read _; exit 127; }
TF_WORKSPACE=%s %s apply -input=false %s
ec=$?
printf '%%s' "$ec" > %s && mv %s %s
if [ "$ec" -ne 0 ]; then printf '\ntfmux: apply FAILED (exit %%s) — press Enter to close\n' "$ec"; read _; fi`,
		shq(s.ModuleDir), tmp, tmp, exit,
		shq(s.Workspace), shq(s.TFBin), shq(s.PlanFile),
		tmp, tmp, exit,
	)
}

// LaunchApply opens a new window running the apply and returns its tmux
// window ID and a durable token identifying it.
//
// The window ID alone isn't enough to re-find this apply later: tmux window
// IDs (@N) are unique only for the life of one tmux server, so they get
// reused after a server restart, and a stale persisted ID can then resolve
// to an entirely unrelated window. The token — stamped onto the window as a
// tmux option — survives that: WindowFor looks a window up by token, not by
// ID, so a recycled ID can never be mistaken for this apply.
func (c *Ctl) LaunchApply(spec ApplySpec) (windowID, token string, err error) {
	if err := os.Remove(spec.ExitFile); err != nil && !os.IsNotExist(err) {
		return "", "", err
	}
	if err := c.ensureSession(); err != nil {
		return "", "", err
	}
	name := spec.Name
	if name == "" {
		name = spec.Workspace
	}
	out, err := c.run("new-window", "-t", c.Session+":", "-n", name,
		"-P", "-F", "#{window_id}", applyScript(spec))
	if err != nil {
		return "", "", err
	}
	windowID = strings.TrimSpace(string(out))
	token = fmt.Sprintf("%s//%s@%d", spec.ModuleDir, spec.Workspace, time.Now().UnixNano())
	if _, err := c.run("set-option", "-w", "-t", windowID, tokenOpt, token); err != nil {
		_ = c.KillWindow(windowID)
		return "", "", fmt.Errorf("stamping apply window: %w", err)
	}
	return windowID, token, nil
}

// WindowFor returns the id of the window currently carrying this token, if
// one exists. An apply's window can only ever be found this way — never by
// its (possibly stale, possibly recycled) window ID alone.
func (c *Ctl) WindowFor(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	if _, err := c.run("has-session", "-t", "="+c.Session); err != nil {
		return "", false
	}
	out, err := c.run("list-windows", "-t", c.Session, "-F", "#{window_id} #{"+tokenOpt+"}")
	if err != nil {
		return "", false
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		id, tok, ok := strings.Cut(line, " ")
		if ok && tok == token {
			return id, true
		}
	}
	return "", false
}

// ListWindowIDs returns the IDs of the session's current windows. A missing
// session yields an empty set (all windows gone).
func (c *Ctl) ListWindowIDs() (map[string]bool, error) {
	if _, err := c.run("has-session", "-t", "="+c.Session); err != nil {
		return map[string]bool{}, nil
	}
	out, err := c.run("list-windows", "-t", c.Session, "-F", "#{window_id}")
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			ids[line] = true
		}
	}
	return ids, nil
}

// KillWindow closes a window by id, terminating whatever runs in it. A
// missing window (already closed) is not an error.
func (c *Ctl) KillWindow(windowID string) error {
	if windowID == "" {
		return nil
	}
	if _, err := c.run("kill-window", "-t", windowID); err != nil {
		// the window may have already exited; that's fine
		if ids, lerr := c.ListWindowIDs(); lerr == nil && !ids[windowID] {
			return nil
		}
		return err
	}
	return nil
}

// AttachCmd returns the command that brings the user to the session,
// optionally focused on a window. Inside tmux ($TMUX set) attaching would
// nest, so switch the client instead.
//
// windowID must be freshly resolved (e.g. via WindowFor) by the caller —
// AttachCmd itself only selects it and fails loudly if that's no longer
// possible, rather than silently falling back to "whatever window happens to
// be selected in the session right now" (which is how a stale apply's
// leftover window used to get shown in place of the one actually asked for).
func (c *Ctl) AttachCmd(windowID string) (*exec.Cmd, error) {
	target := c.Session
	if windowID != "" {
		if _, err := c.run("select-window", "-t", windowID); err != nil {
			return nil, fmt.Errorf("apply window %s is gone: %w", windowID, err)
		}
		target = c.Session + ":" + windowID
	}
	if os.Getenv("TMUX") != "" {
		return exec.Command(c.Bin, "switch-client", "-t", target), nil
	}
	return exec.Command(c.Bin, "attach-session", "-t", target), nil
}
