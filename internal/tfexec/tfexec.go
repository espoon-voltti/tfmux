// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

// Package tfexec constructs and runs terraform (or OpenTofu) commands.
//
// Invariants enforced here:
//   - workspaces are selected via the TF_WORKSPACE env var, never
//     `terraform workspace select` (which would mutate .terraform/environment
//     shared with the user's shell and other tfmux jobs). A module with
//     manifest command templates (TF.Templates) is the one exception: its
//     workspace is a logical name exported as TFMUX_WORKSPACE, and
//     TF_WORKSPACE is never set for it
//   - templates run under /bin/sh with tfmux's own flags appended after the
//     snippet (see InitCommand, PlanCommand, ApplyCommand), so every template
//     must end with the terraform subcommand it names
//   - session-dependent Terraform env vars (TF_WORKSPACE and friends, see
//     sessionEnvVars) are stripped from the inherited environment before
//     each command runs, so a value left over in the invoking shell can't
//     silently override what tfmux itself decides
//   - every command runs with -input=false so credential prompts fail fast
//     instead of hanging a worker
//   - cancellation sends SIGINT first (terraform releases state locks on
//     SIGINT; SIGKILL leaks them), with a kill after a grace period
//
// Callers must hold the per-module-directory lock (see internal/runner)
// while invoking anything here: init mutates .terraform/. Deciding when a
// command needs init, and retrying after it, is the caller's job (see
// Initialized and NeedsInit) — this package only builds and runs commands.
package tfexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tfjson "github.com/hashicorp/terraform-json"

	"github.com/espoon-voltti/tfmux/internal/domain"
)

// TF runs terraform commands in one module directory.
type TF struct {
	Bin string   // terraform binary (name on PATH or absolute)
	Dir string   // module directory (cmd.Dir, so tfenv shims resolve locally)
	Env []string // extra env entries appended to os.Environ()

	// Templates, when set, replaces the init/plan/apply command lines with
	// the module's manifest templates (see the package doc). nil keeps the
	// default commands.
	Templates *domain.CommandTemplates

	// Out, when set, receives the command's combined output as it is produced
	// (in addition to the buffered Result.Output), so callers can stream a
	// live log. Writes happen from the command's I/O goroutine.
	Out io.Writer
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
	cmd.Env = t.baseEnv()
	if workspace != "" {
		cmd.Env = append(cmd.Env, "TF_WORKSPACE="+workspace)
	}
	return t.runCmd(ctx, cmd, t.Bin+" "+strings.Join(args, " "))
}

// runTemplate executes a manifest command template under /bin/sh with the
// workspace exported as TFMUX_WORKSPACE. The shell gets its own process
// group so cancellation reaches terraform, not just sh.
func (t TF) runTemplate(ctx context.Context, workspace, script string) (Result, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	cmd.Env = append(t.baseEnv(), "TFMUX_TF_BIN="+t.Bin, "TFMUX_WORKSPACE="+workspace)
	ownProcessGroup(cmd)
	return t.runCmd(ctx, cmd, "sh -c "+script)
}

func (t TF) baseEnv() []string {
	env := append(filteredEnviron(), "TF_IN_AUTOMATION=1")
	return append(env, t.Env...)
}

// runCmd runs a prepared command in the module dir, teeing output to Out.
// desc names the command in the error for a failure to run at all.
func (t TF) runCmd(ctx context.Context, cmd *exec.Cmd, desc string) (Result, error) {
	cmd.Dir = t.Dir
	var buf bytes.Buffer
	var sink io.Writer = &buf
	if t.Out != nil {
		sink = io.MultiWriter(&buf, t.Out) // tee live output to the caller
	}
	cmd.Stdout = sink
	cmd.Stderr = sink
	cmd.Cancel = func() error { return interrupt(cmd) }
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
	return res, fmt.Errorf("%s in %s: %w", desc, t.Dir, err)
}

// ShellQuote single-quotes s for /bin/sh.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// DefaultTemplate is the command a template module runs for a verb it has
// no template for.
func DefaultTemplate(verb string) string { return `"$TFMUX_TF_BIN" ` + verb }

// TemplateOr returns tmpl, or the default template for verb when tmpl is
// blank.
func TemplateOr(tmpl, verb string) string {
	if tmpl == "" {
		return DefaultTemplate(verb)
	}
	return tmpl
}

// InitCommand is the full shell line for an init template.
func InitCommand(tmpl string, upgrade bool) string {
	s := tmpl + " -input=false -no-color"
	if upgrade {
		s += " -upgrade"
	}
	return s
}

// PlanCommand is the full shell line for a plan template writing outFile.
func PlanCommand(tmpl, outFile string) string {
	return tmpl + " -input=false -no-color -detailed-exitcode -out=" + ShellQuote(outFile)
}

// ApplyCommand is the full shell line for an apply template applying
// planFile.
func ApplyCommand(tmpl, planFile string) string {
	return tmpl + " -input=false " + ShellQuote(planFile)
}

// OutputCommand is the full shell line for an output template.
func OutputCommand(tmpl string) string { return tmpl + " -no-color" }

// sessionEnvVars are Terraform env vars that carry state from the invoking
// shell session rather than a deliberate tfmux or user choice. tfmux decides
// each of these itself for every command it runs (workspace via the
// workspace parameter, -input=false unconditionally, no ambient CLI args, no
// ambient logging), so a value left over from the shell must not leak into
// the subprocess and silently override that.
var sessionEnvVars = []string{
	"TF_WORKSPACE",
	"TF_DATA_DIR",
	"TF_INPUT",
	"TF_LOG",
	"TF_LOG_PATH",
	"TF_LOG_CORE",
	"TF_LOG_PROVIDER",
}

// filteredEnviron returns os.Environ() with sessionEnvVars, and any
// TF_CLI_ARGS / TF_CLI_ARGS_<command> entries, removed.
func filteredEnviron() []string {
	blocked := make(map[string]bool, len(sessionEnvVars))
	for _, k := range sessionEnvVars {
		blocked[k] = true
	}
	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if blocked[key] || strings.HasPrefix(key, "TF_CLI_ARGS") {
			continue
		}
		filtered = append(filtered, kv)
	}
	return filtered
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
// mutates .terraform.lock.hcl — never set it automatically. workspace is
// only meaningful with Templates, where the init template may depend on it;
// the default init ignores it.
func (t TF) Init(ctx context.Context, workspace string, upgrade bool) (Result, error) {
	if t.Templates != nil {
		return t.runTemplate(ctx, workspace, InitCommand(TemplateOr(t.Templates.Init, "init"), upgrade))
	}
	args := []string{"init", "-input=false", "-no-color"}
	if upgrade {
		args = append(args, "-upgrade")
	}
	return t.run(ctx, "", args...)
}

// WorkspaceList enumerates the module's workspaces. Callers are responsible
// for initializing the module first (see Initialized/NeedsInit) — this runs
// `workspace list` exactly once and reports whatever it gets.
func (t TF) WorkspaceList(ctx context.Context) (Result, []string, error) {
	res, err := t.run(ctx, "", "workspace", "list", "-no-color")
	if err != nil || res.ExitCode != 0 {
		return res, nil, err
	}
	var workspaces []string
	for line := range strings.SplitSeq(string(res.Output), "\n") {
		ws := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "*"))
		if ws != "" {
			workspaces = append(workspaces, ws)
		}
	}
	return res, workspaces, nil
}

// Plan exit codes with -detailed-exitcode.
const (
	PlanClean   = 0
	PlanError   = 1
	PlanChanges = 2
)

// PlanErrorKind labels a well-known plan failure signature so the UI can call
// it out specifically instead of a bare "plan error". The zero value means no
// known signature matched.
type PlanErrorKind string

const (
	PlanErrorUpgradeNeeded PlanErrorKind = "init -upgrade required"
	PlanErrorStateLocked   PlanErrorKind = "state locked"
)

// upgradeSignatures mark errors that only `terraform init -upgrade` (not a
// plain init) can fix: the dependency lock file no longer matches the
// configuration's provider requirements.
var upgradeSignatures = []string{
	`inconsistent dependency lock file`,
}

// lockSignatures mark errors caused by another process holding the state
// lock.
var lockSignatures = []string{
	`error acquiring the state lock`,
}

// ClassifyPlanError inspects a failed plan's combined output for well-known
// failure signatures, returning "" when none match.
func ClassifyPlanError(output []byte) PlanErrorKind {
	s := strings.ToLower(string(output))
	for _, sig := range lockSignatures {
		if strings.Contains(s, sig) {
			return PlanErrorStateLocked
		}
	}
	for _, sig := range upgradeSignatures {
		if strings.Contains(s, sig) {
			return PlanErrorUpgradeNeeded
		}
	}
	return ""
}

// Plan runs terraform plan for one workspace, writing the plan to outFile.
// The returned ExitCode follows -detailed-exitcode semantics. Callers are
// responsible for initializing the module first and for retrying after an
// init-shaped failure (see Initialized/NeedsInit).
func (t TF) Plan(ctx context.Context, workspace, outFile string) (Result, error) {
	if t.Templates != nil {
		return t.runTemplate(ctx, workspace, PlanCommand(TemplateOr(t.Templates.Plan, "plan"), outFile))
	}
	return t.run(ctx, workspace,
		"plan", "-input=false", "-no-color", "-detailed-exitcode", "-out="+outFile)
}

// Output runs `terraform output` for one workspace, returning its plain-text
// listing. Callers are responsible for initializing the module first and for
// retrying after an init-shaped failure (see Initialized/NeedsInit).
func (t TF) Output(ctx context.Context, workspace string) (Result, error) {
	if t.Templates != nil {
		return t.runTemplate(ctx, workspace, OutputCommand(TemplateOr(t.Templates.Output, "output")))
	}
	return t.run(ctx, workspace, "output", "-no-color")
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
