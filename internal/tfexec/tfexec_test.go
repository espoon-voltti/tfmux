// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package tfexec

import (
	"context"
	"os"
	"path/filepath"
	"slices"
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
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
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
	res, got, err := tf.WorkspaceList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d, output:\n%s", res.ExitCode, res.Output)
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

// Regression: an init-shaped failure must surface via ExitCode/Output, not
// be misread as an empty workspace list.
func TestWorkspaceListNeedsInit(t *testing.T) {
	tf, _ := newTF(t)
	tf.Env = append(tf.Env, "TFMUX_FAKE_NEED_INIT=1")
	if err := os.MkdirAll(filepath.Join(tf.Dir, ".terraform"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, got, err := tf.WorkspaceList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("expected a failing exit code, got 0 with output:\n%s", res.Output)
	}
	if got != nil {
		t.Errorf("workspaces = %v, want nil", got)
	}
	if !NeedsInit(res.Output) {
		t.Errorf("output did not look init-shaped: %q", res.Output)
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

// Regression: Plan runs exactly once and reports an init-shaped failure
// as-is — retrying belongs to the caller (see internal/runner), not tfexec.
func TestPlanNeedsInitNoAutoRetry(t *testing.T) {
	tf, logFile := newTF(t)
	tf.Env = append(tf.Env, "TFMUX_FAKE_NEED_INIT=1")
	if err := os.MkdirAll(filepath.Join(tf.Dir, ".terraform"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := tf.Plan(context.Background(), "prod", filepath.Join(t.TempDir(), "p"))
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("expected a failing exit code, got 0 with output:\n%s", res.Output)
	}
	if !NeedsInit(res.Output) {
		t.Errorf("output did not look init-shaped: %q", res.Output)
	}
	cs := calls(t, logFile)
	if len(cs) != 1 {
		t.Fatalf("expected exactly one plan call, got %v", cs)
	}
}

func TestOutput(t *testing.T) {
	tf, logFile := newTF(t)
	if err := os.MkdirAll(filepath.Join(tf.Dir, ".terraform"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := tf.Output(context.Background(), "prod")
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d, output:\n%s", res.ExitCode, res.Output)
	}
	if !strings.Contains(string(res.Output), "hello") {
		t.Errorf("output = %q", res.Output)
	}
	cs := calls(t, logFile)
	if len(cs) != 1 || !strings.Contains(cs[0], "output") || !strings.Contains(cs[0], " prod ") {
		t.Errorf("calls = %v", cs)
	}
}

// Regression: Output on an uninitialized module fails outright — it's the
// caller's job to check Initialized() and init first (see internal/runner).
func TestOutputFailsWhenMissingInit(t *testing.T) {
	tf, logFile := newTF(t)
	tf.Env = append(tf.Env, "TFMUX_FAKE_NEED_INIT=1")
	res, err := tf.Output(context.Background(), "prod")
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("expected a failing exit code, got 0 with output:\n%s", res.Output)
	}
	if !NeedsInit(res.Output) {
		t.Errorf("output did not look init-shaped: %q", res.Output)
	}
	cs := calls(t, logFile)
	if len(cs) != 1 || !strings.Contains(cs[0], "output") {
		t.Errorf("expected exactly one output call, got %v", cs)
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

func TestClassifyPlanError(t *testing.T) {
	for out, want := range map[string]PlanErrorKind{
		`Error: Inconsistent dependency lock file`:                       PlanErrorUpgradeNeeded,
		"Error: Error acquiring the state lock\n\nLock Info:\n  ID: abc": PlanErrorStateLocked,
		`Error: Invalid resource type`:                                   "",
		``:                                                               "",
	} {
		if got := ClassifyPlanError([]byte(out)); got != want {
			t.Errorf("ClassifyPlanError(%q) = %q, want %q", out, got, want)
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

// Regression: a TF_WORKSPACE left over in the invoking shell must not reach
// a command tfexec itself runs with no workspace (here Version), which would
// otherwise leak in unfiltered via os.Environ().
func TestVersionIgnoresInheritedWorkspace(t *testing.T) {
	tf, logFile := newTF(t)
	t.Setenv("TF_WORKSPACE", "leaked")
	if _, err := tf.Version(context.Background()); err != nil {
		t.Fatal(err)
	}
	cs := calls(t, logFile)
	if len(cs) != 1 || !strings.Contains(cs[0], " <none> version") {
		t.Errorf("inherited TF_WORKSPACE leaked into subprocess env: %v", cs)
	}
}

func TestFilteredEnvironStripsSessionVars(t *testing.T) {
	blocked := []string{"TF_WORKSPACE", "TF_DATA_DIR", "TF_INPUT", "TF_LOG", "TF_LOG_PATH", "TF_LOG_CORE", "TF_LOG_PROVIDER", "TF_CLI_ARGS", "TF_CLI_ARGS_apply"}
	for _, k := range blocked {
		t.Setenv(k, "leaked")
	}
	t.Setenv("TF_VAR_keep_me", "kept")

	env := filteredEnviron()
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		for _, b := range blocked {
			if key == b {
				t.Errorf("filteredEnviron() leaked %q", kv)
			}
		}
	}
	if !slices.Contains(env, "TF_VAR_keep_me=kept") {
		t.Error("filteredEnviron() dropped an unrelated var")
	}
}
