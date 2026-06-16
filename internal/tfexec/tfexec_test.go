// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package tfexec

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/espoon-voltti/tfmux/internal/tftest"
)

func newTF(t *testing.T) (TF, string) {
	t.Helper()
	dir := t.TempDir()
	bin := tftest.Write(t, t.TempDir())
	logFile := filepath.Join(t.TempDir(), "calls.log")
	tf := TF{Bin: bin, Dir: dir, Env: []string{"TFMUX_FAKE_LOG=" + logFile}}
	return tf, logFile
}

func calls(t *testing.T, logFile string) []string {
	t.Helper()
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "start ") {
			out = append(out, line)
		}
	}
	return out
}

func TestWorkspaceList(t *testing.T) {
	tf, logFile := newTF(t)
	// .terraform exists => no init needed
	if err := os.MkdirAll(filepath.Join(tf.Dir, ".terraform"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := tf.WorkspaceList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"default", "prod", "staging"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("workspaces = %v, want %v", got, want)
	}
	cs := calls(t, logFile)
	if len(cs) != 1 || !strings.Contains(cs[0], "workspace list") {
		t.Errorf("calls = %v", cs)
	}
}

func TestWorkspaceListInitsWhenMissing(t *testing.T) {
	tf, logFile := newTF(t)
	if _, err := tf.WorkspaceList(context.Background()); err != nil {
		t.Fatal(err)
	}
	cs := calls(t, logFile)
	if len(cs) != 2 || !strings.Contains(cs[0], "init") || !strings.Contains(cs[1], "workspace list") {
		t.Errorf("expected init then workspace list, got %v", cs)
	}
}

func TestPlanWorkspaceEnvAndExitCodes(t *testing.T) {
	for _, exit := range []int{PlanClean, PlanError, PlanChanges} {
		tf, logFile := newTF(t)
		tf.Env = append(tf.Env, "TFMUX_FAKE_PLAN_EXIT="+string(rune('0'+exit)))
		if err := os.MkdirAll(filepath.Join(tf.Dir, ".terraform"), 0o755); err != nil {
			t.Fatal(err)
		}
		outFile := filepath.Join(t.TempDir(), "plan.tfplan")
		res, err := tf.Plan(context.Background(), "prod", outFile)
		if err != nil {
			t.Fatal(err)
		}
		if res.ExitCode != exit {
			t.Errorf("exit = %d, want %d", res.ExitCode, exit)
		}
		cs := calls(t, logFile)
		last := cs[len(cs)-1]
		if !strings.Contains(last, " prod ") {
			t.Errorf("TF_WORKSPACE not passed: %q", last)
		}
		for _, arg := range []string{"-input=false", "-detailed-exitcode", "-out=" + outFile} {
			if !strings.Contains(last, arg) {
				t.Errorf("missing arg %q in %q", arg, last)
			}
		}
	}
}

func TestPlanInitRetryOnce(t *testing.T) {
	tf, logFile := newTF(t)
	tf.Env = append(tf.Env, "TFMUX_FAKE_NEED_INIT=1")
	// fake an existing .terraform so the cheap pre-check passes and the
	// stderr-sniffing path is exercised
	if err := os.MkdirAll(filepath.Join(tf.Dir, ".terraform"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := tf.Plan(context.Background(), "prod", filepath.Join(t.TempDir(), "p"))
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d after init retry, output:\n%s", res.ExitCode, res.Output)
	}
	cs := calls(t, logFile)
	if len(cs) != 3 {
		t.Fatalf("expected plan, init, plan — got %v", cs)
	}
	if !strings.Contains(cs[0], "plan") || !strings.Contains(cs[1], "init") || !strings.Contains(cs[2], "plan") {
		t.Errorf("wrong sequence: %v", cs)
	}
}

func TestNeedsInit(t *testing.T) {
	for out, want := range map[string]bool{
		`Error: Backend initialization required, please run "terraform init"`: true,
		`Error: Inconsistent dependency lock file`:                            true,
		`Error: Module not installed`:                                         true,
		`Error: Invalid resource type`:                                        false,
		``:                                                                    false,
	} {
		if got := NeedsInit([]byte(out)); got != want {
			t.Errorf("NeedsInit(%q) = %v, want %v", out, got, want)
		}
	}
}

func TestVersion(t *testing.T) {
	tf, _ := newTF(t)
	v, err := tf.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != "1.9.9" {
		t.Errorf("version = %q", v)
	}
}

func TestOutTeesLiveOutput(t *testing.T) {
	tf, _ := newTF(t)
	var live strings.Builder
	tf.Out = &live
	v, err := tf.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != "1.9.9" {
		t.Errorf("version = %q", v)
	}
	if !strings.Contains(live.String(), "terraform_version") {
		t.Errorf("Out did not receive the live output: %q", live.String())
	}
}

func TestRunMissingBinary(t *testing.T) {
	tf := TF{Bin: "/nonexistent/terraform", Dir: t.TempDir()}
	if _, err := tf.Version(context.Background()); err == nil {
		t.Error("expected error for missing binary")
	}
}
