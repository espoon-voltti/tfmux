// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/espoon-voltti/tfmux/internal/config"
	"github.com/espoon-voltti/tfmux/internal/runner"
	"github.com/espoon-voltti/tfmux/internal/state"
	"github.com/espoon-voltti/tfmux/internal/tftest"
)

const baseTF = `terraform {
  backend "s3" {
    bucket = "tf"
    key    = "base.tfstate"
  }
}
`

const serviceTF = `terraform {
  backend "s3" {
    bucket = "tf"
    key    = "service.tfstate"
  }
}
data "terraform_remote_state" "base" {
  backend   = "s3"
  workspace = terraform.workspace
  config = {
    bucket = "tf"
    key    = "base.tfstate"
  }
}
module "waf" {
  source = "git::https://github.com/org/shared.git//modules/waf?ref=v1"
}
`

func git(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// estate builds one repo with base and service root modules (service reads
// base's state), a cached workspace list for each, and returns the config
// and store a server needs. The fake terraform binary stands in for the
// real one.
type estate struct {
	cfg   *config.Config
	store *state.Store
	repo  string
}

func newEstate(t *testing.T) *estate {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "demo-infra")
	for name, tf := range map[string]string{"base": baseTF, "service": serviceTF} {
		dir := filepath.Join(repo, "terraform", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(tf), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "init")

	bin := tftest.Write(t, t.TempDir())
	t.Setenv("TFMUX_FAKE_LOG", filepath.Join(t.TempDir(), "calls.log"))
	cfg := config.Default()
	cfg.Roots = []string{root}
	cfg.TerraformBin = bin
	store := state.New(t.TempDir())
	for _, name := range []string{"base", "service"} {
		if err := store.SaveWorkspaces(filepath.Join(repo, "terraform", name), []string{"prod", "staging"}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	return &estate{cfg: cfg, store: store, repo: repo}
}

// connect starts a server over an in-memory transport and returns a client
// session to it.
func connect(t *testing.T, e *estate, all bool) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	s, err := New(ctx, Options{Cfg: e.cfg, Store: e.store, AllTools: all, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ct, st := mcp.NewInMemoryTransports()
	ss, err := s.MCP().Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func toolNames(t *testing.T, cs *mcp.ClientSession) map[string]bool {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	return names
}

// call invokes a tool and returns its text and structured content.
func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, map[string]any, *mcp.CallToolResult) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var text string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	var structured map[string]any
	if res.StructuredContent != nil {
		raw, _ := json.Marshal(res.StructuredContent)
		_ = json.Unmarshal(raw, &structured)
	}
	return text, structured, res
}

func TestReadOnlyToolSet(t *testing.T) {
	cs := connect(t, newEstate(t), false)
	names := toolNames(t, cs)
	for _, want := range []string{"list", "pending", "deps", "plan_log", "plan", "output", "enumerate_workspaces", "refresh"} {
		if !names[want] {
			t.Errorf("missing read-only tool %s", want)
		}
	}
	for _, absent := range []string{"apply", "init_upgrade", "discard_plan"} {
		if names[absent] {
			t.Errorf("write tool %s exposed on a read-only server", absent)
		}
	}
}

func TestAllToolSet(t *testing.T) {
	cs := connect(t, newEstate(t), true)
	names := toolNames(t, cs)
	for _, want := range []string{"apply", "init_upgrade", "discard_plan", "plan"} {
		if !names[want] {
			t.Errorf("missing tool %s", want)
		}
	}
}

func TestListPendingDeps(t *testing.T) {
	cs := connect(t, newEstate(t), false)
	text, structured, _ := call(t, cs, "list", nil)
	if !strings.Contains(text, "demo-infra/terraform/service") || !strings.Contains(text, "prod  never planned") {
		t.Errorf("list text:\n%s", text)
	}
	if repos, ok := structured["repos"].([]any); !ok || len(repos) != 1 {
		t.Errorf("list structured: %v", structured)
	}

	text, _, _ = call(t, cs, "pending", map[string]any{"needs_plan_only": true})
	if strings.Count(text, "unplanned") != 4 {
		t.Errorf("pending text:\n%s", text)
	}

	text, structured, _ = call(t, cs, "deps", map[string]any{"module": "demo-infra/terraform/service"})
	if !strings.Contains(text, "state   demo-infra/terraform/base  (base, workspace: same)") {
		t.Errorf("deps forward:\n%s", text)
	}
	if mods, _ := structured["modules"].([]any); len(mods) != 1 {
		t.Errorf("deps structured: %v", structured)
	}
	text, structured, _ = call(t, cs, "deps", map[string]any{"reverse": true, "filter": "shared//modules/waf"})
	if !strings.Contains(text, "demo-infra/terraform/service @v1") {
		t.Errorf("deps reverse:\n%s", text)
	}
	deps, _ := structured["dependents"].(map[string]any)
	if len(deps["github.com/org/shared//modules/waf"].([]any)) != 1 {
		t.Errorf("reverse structured: %v", structured)
	}
}

func TestUnknownModuleNamesCandidates(t *testing.T) {
	cs := connect(t, newEstate(t), false)
	_, _, res := call(t, cs, "plan_log", map[string]any{"module": "terraform/serv", "workspace": "prod"})
	if !res.IsError {
		t.Fatal("expected a tool error")
	}
	msg := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(msg, "did you mean: demo-infra/terraform/service") {
		t.Errorf("error = %q", msg)
	}
	_, _, res = call(t, cs, "plan_log", map[string]any{"module": "demo-infra/terraform/service", "workspace": "nope"})
	if !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "has: prod, staging") {
		t.Errorf("workspace error = %+v", res)
	}
}

func TestPlanThenPlanLogAndOutput(t *testing.T) {
	e := newEstate(t)
	t.Setenv("TFMUX_FAKE_PLAN_EXIT", "2")
	showJSON := filepath.Join(t.TempDir(), "show.json")
	if err := os.WriteFile(showJSON, []byte(`{"format_version":"1.2","resource_changes":[{"address":"a","change":{"actions":["create"]}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TFMUX_FAKE_SHOW_JSON", showJSON)
	cs := connect(t, e, false)

	args := map[string]any{"module": "demo-infra/terraform/service", "workspace": "prod"}
	text, structured, res := call(t, cs, "plan", args)
	if res.IsError {
		t.Fatalf("plan error: %s", text)
	}
	if structured["plan_exit_code"].(float64) != 2 || structured["summary"] != "+1 ~0 -0" || structured["plan_file"] != true {
		t.Errorf("plan structured: %v", structured)
	}
	if !strings.Contains(text, "changes +1 ~0 -0") || !strings.Contains(text, "Plan: 1 to add") {
		t.Errorf("plan text:\n%s", text)
	}
	reasons := structured["reasons"].([]any)
	if len(reasons) != 1 || reasons[0].(map[string]any)["code"] != "ready" {
		t.Errorf("reasons = %v", reasons)
	}
	modPath := filepath.Join(e.repo, "terraform/service")
	if rec, _ := e.store.LoadRun(modPath, "prod"); rec == nil || rec.Fingerprint == "" {
		t.Error("plan should persist a run record with a fingerprint")
	}

	text, _, _ = call(t, cs, "plan_log", map[string]any{"module": "demo-infra/terraform/service", "workspace": "prod", "tail": 1})
	if !strings.HasSuffix(strings.TrimSpace(text), "Plan: 1 to add, 0 to change, 0 to destroy.") {
		t.Errorf("tail should keep only the last line:\n%s", text)
	}

	text, _, res = call(t, cs, "output", args)
	if res.IsError || !strings.Contains(text, `greeting = "hello"`) {
		t.Errorf("output: err=%v text=%q", res.IsError, text)
	}
}

func TestEnumerateAndRefresh(t *testing.T) {
	e := newEstate(t)
	cs := connect(t, e, false)
	text, structured, res := call(t, cs, "enumerate_workspaces", map[string]any{"module": "demo-infra/terraform/base"})
	if res.IsError {
		t.Fatalf("enumerate: %s", text)
	}
	if ws := structured["workspaces"].([]any); len(ws) != 3 {
		t.Errorf("workspaces = %v", ws)
	}
	// The server's own estate learned the new list without a refresh.
	_, _, res = call(t, cs, "plan_log", map[string]any{"module": "demo-infra/terraform/base", "workspace": "default"})
	if res.IsError {
		t.Errorf("workspace from enumeration not usable: %+v", res.Content)
	}
	_, structured, _ = call(t, cs, "refresh", nil)
	if structured["modules"].(float64) != 2 {
		t.Errorf("refresh = %v", structured)
	}
}

func TestApplyRefusedWithoutReadyPlan(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	e := newEstate(t)
	cs := connect(t, e, true)
	text, _, res := call(t, cs, "apply", map[string]any{"module": "demo-infra/terraform/service", "workspace": "prod"})
	if !res.IsError || !strings.Contains(text, "nothing to apply") {
		t.Errorf("apply without plan: err=%v %q", res.IsError, text)
	}
}

func TestDiscardPlan(t *testing.T) {
	e := newEstate(t)
	modPath := filepath.Join(e.repo, "terraform/service")
	planFile, _ := e.store.PlanFilePath(modPath, "prod")
	if err := os.WriteFile(planFile, []byte("plan"), 0o600); err != nil {
		t.Fatal(err)
	}
	cs := connect(t, e, true)
	_, structured, _ := call(t, cs, "discard_plan", map[string]any{"module": "demo-infra/terraform/service", "workspace": "prod"})
	if structured["discarded"] != true || e.store.HasPlanFile(modPath, "prod") {
		t.Errorf("discard: %v, file present=%v", structured, e.store.HasPlanFile(modPath, "prod"))
	}
}

func TestCancelledCallCancelsTask(t *testing.T) {
	e := newEstate(t)
	t.Setenv("TFMUX_FAKE_SLEEP", "5")
	ctx := context.Background()
	s, err := New(ctx, Options{Cfg: e.cfg, Store: e.store})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ws, err := s.resolveWorkspace("demo-infra/terraform/service", "prod")
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = s.await(cctx, runner.KindPlan, ws.Key(), func() bool { return s.runner.EnqueuePlan(ws) })
	if err == nil || time.Since(start) > 3*time.Second {
		t.Errorf("await should return on context end: err=%v after %v", err, time.Since(start))
	}
	deadline := time.Now().Add(3 * time.Second)
	for s.runner.Running(ws.Key()) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if s.runner.Running(ws.Key()) {
		t.Error("the plan task should have been cancelled")
	}
}

func TestStripProgress(t *testing.T) {
	log := `module.a.aws_thing.x: Refreshing state... [id=abc]
data.aws_caller_identity.current: Reading...
data.aws_caller_identity.current: Read complete after 0s [id=123]

Terraform used the selected providers to generate the following execution
plan.

Plan: 1 to add, 0 to change, 0 to destroy.
`
	got := stripProgress(log)
	if strings.Contains(got, "Refreshing") || strings.Contains(got, "Reading") || strings.Contains(got, "Read complete") {
		t.Errorf("progress lines kept:\n%s", got)
	}
	if !strings.HasPrefix(got, "Terraform used") || !strings.Contains(got, "Plan: 1 to add") {
		t.Errorf("outcome lines lost:\n%s", got)
	}
}
