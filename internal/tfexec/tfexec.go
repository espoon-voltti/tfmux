// Package tfexec constructs and runs terraform (or OpenTofu) commands.
//
// Invariants enforced here:
//   - workspaces are selected via the TF_WORKSPACE env var, never
//     `terraform workspace select` (which would mutate .terraform/environment
//     shared with the user's shell and other tfmux jobs)
//   - every command runs with -input=false so credential prompts fail fast
//     instead of hanging a worker
//   - cancellation sends SIGINT first (terraform releases state locks on
//     SIGINT; SIGKILL leaks them), with a kill after a grace period
//
// Callers must hold the per-module-directory lock (see internal/runner) while
// invoking anything here: init mutates .terraform/ and any command can turn
// into an init via the retry path.
package tfexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tfjson "github.com/hashicorp/terraform-json"
)

// TF runs terraform commands in one module directory.
type TF struct {
	Bin string // terraform binary (name on PATH or absolute)
	Dir string // module directory (cmd.Dir, so tfenv shims resolve locally)
	Env []string // extra env entries appended to os.Environ()
}

// Result is the outcome of one terraform invocation.
type Result struct {
	ExitCode int
	Output   []byte // combined stdout+stderr, in arrival order
}

// run executes one terraform command. err is non-nil only for failures to
// run at all (binary missing, context canceled); terraform's own non-zero
// exits are reported via Result.ExitCode.
func (t TF) run(ctx context.Context, workspace string, args ...string) (Result, error) {
	cmd := exec.CommandContext(ctx, t.Bin, args...)
	cmd.Dir = t.Dir
	cmd.Env = append(os.Environ(), "TF_IN_AUTOMATION=1")
	cmd.Env = append(cmd.Env, t.Env...)
	if workspace != "" {
		cmd.Env = append(cmd.Env, "TF_WORKSPACE="+workspace)
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGINT)
	}
	cmd.WaitDelay = 15 * time.Second // SIGKILL if SIGINT didn't work

	err := cmd.Run()
	res := Result{Output: buf.Bytes()}
	if err == nil {
		return res, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	return res, fmt.Errorf("%s %s in %s: %w", t.Bin, strings.Join(args, " "), t.Dir, err)
}

// initSignatures mark errors that `terraform init -input=false` may fix.
// Matched case-insensitively ("Module not installed" vs "module not
// installed" varies across versions).
var initSignatures = []string{
	`terraform init`, // 'please run "terraform init"' and friends
	`tofu init`,
	`backend initialization required`,
	`inconsistent dependency lock file`,
	`module not installed`,
	`plugin reinitialization required`,
}

// NeedsInit reports whether output looks like a missing/stale-init failure.
func NeedsInit(output []byte) bool {
	s := strings.ToLower(string(output))
	for _, sig := range initSignatures {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}

// Initialized reports whether the module dir has a .terraform directory.
func (t TF) Initialized() bool {
	info, err := os.Stat(filepath.Join(t.Dir, ".terraform"))
	return err == nil && info.IsDir()
}

// Init runs terraform init. upgrade additionally passes -upgrade, which
// mutates .terraform.lock.hcl — never set it automatically.
func (t TF) Init(ctx context.Context, upgrade bool) (Result, error) {
	args := []string{"init", "-input=false", "-no-color"}
	if upgrade {
		args = append(args, "-upgrade")
	}
	return t.run(ctx, "", args...)
}

// runWithInitRetry runs fn; on exit 1 with an init-shaped error it runs init
// once and retries fn once. Init output is prepended so failures show the
// whole story.
func (t TF) runWithInitRetry(ctx context.Context, fn func() (Result, error)) (Result, error) {
	if !t.Initialized() {
		if res, err := t.Init(ctx, false); err != nil || res.ExitCode != 0 {
			return res, err
		}
	}
	res, err := fn()
	if err != nil || res.ExitCode != 1 || !NeedsInit(res.Output) {
		return res, err
	}
	initRes, err := t.Init(ctx, false)
	if err != nil || initRes.ExitCode != 0 {
		initRes.Output = append(res.Output, initRes.Output...)
		return initRes, err
	}
	retry, err := fn()
	retry.Output = append(initRes.Output, retry.Output...)
	return retry, err
}

// WorkspaceList enumerates the module's workspaces, lazily initializing.
func (t TF) WorkspaceList(ctx context.Context) ([]string, error) {
	res, err := t.runWithInitRetry(ctx, func() (Result, error) {
		return t.run(ctx, "", "workspace", "list", "-no-color")
	})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("workspace list in %s failed:\n%s", t.Dir, res.Output)
	}
	var workspaces []string
	for _, line := range strings.Split(string(res.Output), "\n") {
		ws := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "*"))
		if ws != "" {
			workspaces = append(workspaces, ws)
		}
	}
	return workspaces, nil
}

// Plan exit codes with -detailed-exitcode.
const (
	PlanClean   = 0
	PlanError   = 1
	PlanChanges = 2
)

// Plan runs terraform plan for one workspace, writing the plan to outFile.
// The returned ExitCode follows -detailed-exitcode semantics; an init-shaped
// failure triggers one init+retry.
func (t TF) Plan(ctx context.Context, workspace, outFile string) (Result, error) {
	return t.runWithInitRetry(ctx, func() (Result, error) {
		return t.run(ctx, workspace,
			"plan", "-input=false", "-no-color", "-detailed-exitcode", "-out="+outFile)
	})
}

// Apply applies a saved plan file. Used only for constructing the tmux
// command line; tfmux itself never runs apply headless.
func (t TF) ApplyArgs(planFile string) []string {
	return []string{"apply", "-input=false", planFile}
}

// ShowPlan decodes a saved plan file via `terraform show -json`.
func (t TF) ShowPlan(ctx context.Context, planFile string) (*tfjson.Plan, error) {
	res, err := t.run(ctx, "", "show", "-json", planFile)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("show -json in %s failed:\n%s", t.Dir, res.Output)
	}
	var plan tfjson.Plan
	if err := plan.UnmarshalJSON(res.Output); err != nil {
		return nil, fmt.Errorf("decode plan json: %w", err)
	}
	return &plan, nil
}

// Version returns the terraform/tofu version string, e.g. "1.9.0".
func (t TF) Version(ctx context.Context) (string, error) {
	res, err := t.run(ctx, "", "version", "-json")
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("version in %s failed:\n%s", t.Dir, res.Output)
	}
	var v struct {
		TerraformVersion string `json:"terraform_version"`
	}
	if err := json.Unmarshal(res.Output, &v); err != nil {
		return "", err
	}
	return v.TerraformVersion, nil
}
