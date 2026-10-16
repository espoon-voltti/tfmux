// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package mcpserver

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/espoon-voltti/tfmux/internal/attention"
	"github.com/espoon-voltti/tfmux/internal/domain"
	"github.com/espoon-voltti/tfmux/internal/report"
	"github.com/espoon-voltti/tfmux/internal/runner"
	"github.com/espoon-voltti/tfmux/internal/state"
	"github.com/espoon-voltti/tfmux/internal/tfexec"
	"github.com/espoon-voltti/tfmux/internal/tmuxctl"
)

// register declares every tool. This is the one place that decides which
// tools exist: the write tools are added only when allTools is set.
func (s *Server) register() {
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "list", Annotations: ro,
		Description: "Every repo, root module and workspace with its latest plan/apply outcome and why it needs attention. Module labels here are what the other tools take."}, s.list)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "pending", Annotations: ro,
		Description: "Workspaces that need attention (never planned, changed since plan, plan ready to apply, errors) and modules pinning a git module that is behind its shared repo's checkout."}, s.pending)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "deps", Annotations: ro,
		Description: "What root modules depend on: git and local module calls (with how far behind each pin is) and remote states resolved to the root module holding them. reverse=true lists dependents of each shared module/state instead; filter substring-matches."}, s.deps)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "plan_log", Annotations: ro,
		Description: "The last saved plan of a workspace: exit code, change summary, when it ran, attention reasons and the plan's text output."}, s.planLog)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "plan", Annotations: ro,
		Description: "Run `terraform plan` for a workspace on the host and wait for it. Changes nothing; the plan file is saved for the operator to apply."}, s.plan)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "output", Annotations: ro,
		Description: "Run `terraform output` for a workspace and return its text (sensitive outputs stay redacted)."}, s.output)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "enumerate_workspaces", Annotations: ro,
		Description: "List a module's workspaces from its backend (slow, rate-limited; the result is cached). Refused for modules whose repo manifest lists their workspaces."}, s.enumerate)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "refresh", Annotations: ro,
		Description: "Re-discover repos, modules, git status and workspace lists, e.g. after a git pull."}, s.refresh)
	if !s.allTools {
		return
	}
	destructive := &mcp.ToolAnnotations{DestructiveHint: boolp(true)}
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "apply", Annotations: destructive,
		Description: "Apply a workspace's saved plan (must have changes, be on disk and not stale). Runs in tmux on the host and waits for the exit code."}, s.apply)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "init_upgrade", Annotations: destructive,
		Description: "Run `terraform init -upgrade` for a module (rewrites its lock file)."}, s.initUpgrade)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "discard_plan", Annotations: destructive,
		Description: "Delete a workspace's saved plan file."}, s.discardPlan)
}

func boolp(b bool) *bool { return &b }

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// --- inspection ---

type ListIn struct {
	Repo string `json:"repo,omitempty" jsonschema:"only this repo (by name)"`
}
type ListOut struct {
	Repos []report.EstateRepo `json:"repos"`
}

func (s *Server) list(ctx context.Context, _ *mcp.CallToolRequest, in ListIn) (*mcp.CallToolResult, ListOut, error) {
	repos, ignore, _ := s.snapshot()
	all := report.Estate(ctx, s.store, ignore, repos)
	var out ListOut
	for _, r := range all {
		if in.Repo == "" || r.Name == in.Repo {
			out.Repos = append(out.Repos, r)
		}
	}
	if out.Repos == nil {
		out.Repos = []report.EstateRepo{}
	}
	var b strings.Builder
	for _, r := range out.Repos {
		fmt.Fprintf(&b, "%s  %s\n", r.Name, r.Branch)
		for _, m := range r.Modules {
			fmt.Fprintf(&b, "  %s/%s", r.Name, m.Module)
			if !m.WorkspacesKnown {
				b.WriteString("  (workspaces unknown)")
			}
			for _, reason := range m.Behind {
				fmt.Fprintf(&b, "  [%s: %s]", reason.Code, reason.Detail)
			}
			b.WriteString("\n")
			for _, ws := range m.Workspaces {
				fmt.Fprintf(&b, "    %s  %s\n", ws.Name, describeWorkspace(ws))
			}
		}
	}
	s.logf("list repo=%q", in.Repo)
	return text(b.String()), out, nil
}

func describeWorkspace(ws report.EstateWorkspace) string {
	if ws.PlanExitCode == nil {
		return "never planned"
	}
	var parts []string
	switch *ws.PlanExitCode {
	case tfexec.PlanClean:
		parts = append(parts, "clean")
	case tfexec.PlanChanges:
		parts = append(parts, "changes "+ws.Summary)
	default:
		parts = append(parts, "plan error")
	}
	if ws.PlanFinished != nil {
		parts = append(parts, "planned "+report.Ago(time.Since(*ws.PlanFinished)))
	}
	for _, r := range ws.Reasons {
		parts = append(parts, string(r.Code))
	}
	return strings.Join(parts, ", ")
}

type PendingIn struct {
	NeedsPlanOnly bool `json:"needs_plan_only,omitempty" jsonschema:"only workspaces whose next step is a plan"`
}
type PendingOut struct {
	Rows  []report.PendingRow `json:"rows"`
	Notes []string            `json:"notes,omitempty"`
}

func (s *Server) pending(_ context.Context, _ *mcp.CallToolRequest, in PendingIn) (*mcp.CallToolResult, PendingOut, error) {
	repos, ignore, _ := s.snapshot()
	rows, notes := report.Pending(s.store, ignore, repos, in.NeedsPlanOnly)
	if rows == nil {
		rows = []report.PendingRow{}
	}
	var b strings.Builder
	report.PrintPending(&b, rows, time.Now())
	if b.Len() == 0 {
		b.WriteString("nothing pending\n")
	}
	for _, n := range notes {
		b.WriteString("note: " + n + "\n")
	}
	s.logf("pending needs_plan_only=%v rows=%d", in.NeedsPlanOnly, len(rows))
	return text(b.String()), PendingOut{Rows: rows, Notes: notes}, nil
}

type DepsIn struct {
	Module  string `json:"module,omitempty" jsonschema:"one module label; omit for all"`
	Reverse bool   `json:"reverse,omitempty" jsonschema:"list dependents of each git module / remote state instead"`
	Filter  string `json:"filter,omitempty" jsonschema:"substring filter on module labels (forward) or on shared-module/state keys (reverse)"`
}
type DepsOut struct {
	Modules    []report.DepsModule `json:"modules,omitempty"`
	Dependents map[string][]string `json:"dependents,omitempty"` // reverse: key -> dependent module labels
}

func (s *Server) deps(_ context.Context, _ *mcp.CallToolRequest, in DepsIn) (*mcp.CallToolResult, DepsOut, error) {
	repos, ignore, _ := s.snapshot()
	mods, ix, behind := report.Deps(ignore, repos)
	var out DepsOut
	var b strings.Builder
	if in.Reverse {
		report.PrintDepsReverse(&b, ix, behind, in.Filter)
		out.Dependents = map[string][]string{}
		for _, key := range ix.GitKeys() {
			if in.Filter == "" || strings.Contains(key, in.Filter) {
				for _, m := range ix.DependentsOfGit(key) {
					out.Dependents[key] = append(out.Dependents[key], report.ModuleLabel(m))
				}
			}
		}
		for _, addr := range ix.StateKeys() {
			key := addr.String()
			if in.Filter == "" || strings.Contains(key, in.Filter) {
				for _, m := range ix.DependentsOfAddress(addr) {
					out.Dependents[key] = append(out.Dependents[key], report.ModuleLabel(m))
				}
			}
		}
	} else {
		filter := in.Filter
		if in.Module != "" {
			mod, err := s.resolveModule(in.Module)
			if err != nil {
				return nil, out, err
			}
			filter = report.ModuleLabel(mod)
		}
		report.PrintDepsForward(&b, mods, behind, filter)
		for _, dm := range mods {
			label := dm.Repo + "/" + dm.Module
			if filter == "" || strings.Contains(label, filter) {
				out.Modules = append(out.Modules, dm)
			}
		}
	}
	if b.Len() == 0 {
		b.WriteString("nothing matched\n")
	}
	s.logf("deps module=%q reverse=%v filter=%q", in.Module, in.Reverse, in.Filter)
	return text(b.String()), out, nil
}

// --- plans and outputs ---

type WorkspaceIn struct {
	Module    string `json:"module" jsonschema:"module label, repo/relative/path"`
	Workspace string `json:"workspace" jsonschema:"workspace name"`
}

type PlanLogIn struct {
	WorkspaceIn
	Tail    int  `json:"tail,omitempty" jsonschema:"return only the last N lines of the log"`
	Verbose bool `json:"verbose,omitempty" jsonschema:"keep terraform's per-resource refresh progress lines, which are dropped by default"`
}

type PlanIn struct {
	WorkspaceIn
	Verbose bool `json:"verbose,omitempty" jsonschema:"keep terraform's per-resource refresh progress lines, which are dropped by default"`
}

// PlanOut describes one workspace's latest plan.
type PlanOut struct {
	Module       string             `json:"module"`
	Workspace    string             `json:"workspace"`
	PlanExitCode *int               `json:"plan_exit_code,omitempty"` // 0 clean, 1 error, 2 changes
	Summary      string             `json:"summary,omitempty"`
	PlanFinished *time.Time         `json:"plan_finished,omitempty"`
	PlanFile     bool               `json:"plan_file"`
	Reasons      []attention.Reason `json:"reasons,omitempty"`
	Log          string             `json:"log,omitempty"`
}

func (s *Server) planOut(ctx context.Context, mod string, ws *domain.Workspace, tail int, verbose bool) PlanOut {
	fp := fingerprint(ctx, ws.Module)
	ew := report.AssessWorkspace(s.store, ws.Module, ws.Name, fp)
	out := PlanOut{Module: mod, Workspace: ws.Name, PlanExitCode: ew.PlanExitCode, Summary: ew.Summary, PlanFinished: ew.PlanFinished, PlanFile: ew.PlanFile, Reasons: ew.Reasons}
	if path, err := s.store.PlanLogPath(ws.Module.Path, ws.Name); err == nil {
		if data, err := os.ReadFile(path); err == nil {
			log := string(data)
			if !verbose {
				log = stripProgress(log)
			}
			out.Log = tailLines(log, tail)
		}
	}
	return out
}

// stripProgress drops terraform's per-resource progress lines ("…:
// Refreshing state...", "…: Reading...", "…: Read complete after …"), which
// say nothing about the outcome and can outnumber it a hundred to one.
func stripProgress(log string) string {
	var kept []string
	for _, line := range strings.Split(log, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasSuffix(trimmed, ": Refreshing state...") || strings.HasSuffix(trimmed, ": Reading...") ||
			strings.Contains(trimmed, ": Read complete after ") || strings.Contains(trimmed, ": Refreshing state... [id=") ||
			strings.Contains(trimmed, ": Reading... [id=") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimLeft(strings.Join(kept, "\n"), "\n")
}

func tailLines(s string, n int) string {
	if n <= 0 {
		return s
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n") + "\n"
}

func (o PlanOut) text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s//%s: ", o.Module, o.Workspace)
	switch {
	case o.PlanExitCode == nil:
		b.WriteString("never planned")
	case *o.PlanExitCode == tfexec.PlanClean:
		b.WriteString("clean")
	case *o.PlanExitCode == tfexec.PlanChanges:
		b.WriteString("changes " + o.Summary)
	default:
		b.WriteString("plan error")
	}
	if o.PlanFinished != nil {
		b.WriteString(", planned " + report.Ago(time.Since(*o.PlanFinished)))
	}
	for _, r := range o.Reasons {
		b.WriteString(", " + string(r.Code))
		if r.Detail != "" {
			b.WriteString(" (" + r.Detail + ")")
		}
	}
	b.WriteString("\n")
	if o.Log != "" {
		b.WriteString("\n" + o.Log)
	}
	return b.String()
}

func (s *Server) planLog(ctx context.Context, _ *mcp.CallToolRequest, in PlanLogIn) (*mcp.CallToolResult, PlanOut, error) {
	ws, err := s.workspace(in.WorkspaceIn)
	if err != nil {
		return nil, PlanOut{}, err
	}
	out := s.planOut(ctx, in.Module, ws, in.Tail, in.Verbose)
	s.logf("plan_log %s//%s", in.Module, in.Workspace)
	return text(out.text()), out, nil
}

func (s *Server) plan(ctx context.Context, _ *mcp.CallToolRequest, in PlanIn) (*mcp.CallToolResult, PlanOut, error) {
	ws, err := s.workspace(in.WorkspaceIn)
	if err != nil {
		return nil, PlanOut{}, err
	}
	s.logf("plan %s//%s: start", in.Module, in.Workspace)
	ev, err := s.await(ctx, runner.KindPlan, ws.Key(), func() bool { return s.runner.EnqueuePlan(ws) })
	if err != nil {
		return nil, PlanOut{}, err
	}
	if ev.Phase != runner.PhaseDone {
		s.logf("plan %s//%s: %s %s", in.Module, in.Workspace, ev.Phase, firstLine(ev.Err))
		return nil, PlanOut{}, fmt.Errorf("plan %s: %s", ev.Phase, ev.Err)
	}
	out := s.planOut(ctx, in.Module, ws, 0, in.Verbose)
	s.logf("plan %s//%s: exit %v %s", in.Module, in.Workspace, deref(out.PlanExitCode), out.Summary)
	return text(out.text()), out, nil
}

type OutputOut struct {
	Module    string `json:"module"`
	Workspace string `json:"workspace"`
	Output    string `json:"output"`
}

func (s *Server) output(ctx context.Context, _ *mcp.CallToolRequest, in WorkspaceIn) (*mcp.CallToolResult, OutputOut, error) {
	ws, err := s.workspace(in)
	if err != nil {
		return nil, OutputOut{}, err
	}
	ev, err := s.await(ctx, runner.KindOutput, ws.Key(), func() bool { return s.runner.EnqueueOutput(ws) })
	if err != nil {
		return nil, OutputOut{}, err
	}
	if ev.Phase != runner.PhaseDone {
		s.logf("output %s//%s: %s", in.Module, in.Workspace, firstLine(ev.Err))
		return nil, OutputOut{}, fmt.Errorf("output %s: %s", ev.Phase, ev.Err)
	}
	var data []byte
	if path, perr := s.store.OutputLogPath(ws.Module.Path, ws.Name); perr == nil {
		data, _ = os.ReadFile(path)
	}
	s.logf("output %s//%s: ok", in.Module, in.Workspace)
	out := OutputOut{Module: in.Module, Workspace: in.Workspace, Output: string(data)}
	return text(out.Output), out, nil
}

type ModuleIn struct {
	Module string `json:"module" jsonschema:"module label, repo/relative/path"`
}
type EnumerateOut struct {
	Module     string   `json:"module"`
	Workspaces []string `json:"workspaces"`
}

func (s *Server) enumerate(ctx context.Context, _ *mcp.CallToolRequest, in ModuleIn) (*mcp.CallToolResult, EnumerateOut, error) {
	mod, err := s.resolveModule(in.Module)
	if err != nil {
		return nil, EnumerateOut{}, err
	}
	if mod.ManifestListed {
		return nil, EnumerateOut{}, fmt.Errorf("%s: workspaces come from the repo manifest (%v); edit the manifest instead", in.Module, mod.ManifestWorkspaces)
	}
	ev, err := s.await(ctx, runner.KindEnumerate, mod.Path, func() bool { return s.runner.EnqueueEnumerate(mod) })
	if err != nil {
		return nil, EnumerateOut{}, err
	}
	if ev.Phase != runner.PhaseDone {
		return nil, EnumerateOut{}, fmt.Errorf("enumerate %s: %s", ev.Phase, ev.Err)
	}
	s.logf("enumerate_workspaces %s: %v", in.Module, ev.Workspaces)
	out := EnumerateOut{Module: in.Module, Workspaces: ev.Workspaces}
	return text(strings.Join(ev.Workspaces, "\n") + "\n"), out, nil
}

type RefreshOut struct {
	Repos      int `json:"repos"`
	Modules    int `json:"modules"`
	Workspaces int `json:"workspaces"`
}

func (s *Server) refresh(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, RefreshOut, error) {
	if err := s.discover(ctx); err != nil {
		return nil, RefreshOut{}, err
	}
	repos, ignore, _ := s.snapshot()
	var out RefreshOut
	for _, repo := range report.VisibleRepos(ignore, repos) {
		out.Repos++
		for _, mod := range repo.Modules {
			out.Modules++
			out.Workspaces += len(mod.Workspaces)
		}
	}
	s.logf("refresh: %d repos, %d modules, %d workspaces", out.Repos, out.Modules, out.Workspaces)
	return text(fmt.Sprintf("%d repos, %d root modules, %d workspaces\n", out.Repos, out.Modules, out.Workspaces)), out, nil
}

// --- write tools ---

type ApplyOut struct {
	Module    string `json:"module"`
	Workspace string `json:"workspace"`
	ExitCode  *int   `json:"exit_code,omitempty"` // nil when the window vanished without finishing
	Status    string `json:"status"`
}

func (s *Server) apply(ctx context.Context, _ *mcp.CallToolRequest, in WorkspaceIn) (*mcp.CallToolResult, ApplyOut, error) {
	ws, err := s.workspace(in)
	if err != nil {
		return nil, ApplyOut{}, err
	}
	if !tmuxctl.Available() {
		return nil, ApplyOut{}, fmt.Errorf("tmux not found on the host — applies run in tmux windows")
	}
	mod := ws.Module
	if s.runner.Running(ws.Key()) {
		return nil, ApplyOut{}, fmt.Errorf("%s//%s: a task is already in flight for this workspace", in.Module, in.Workspace)
	}
	rec, _ := s.store.LoadRun(mod.Path, ws.Name)
	if reason := attention.ApplyBlocker(rec, s.store.HasPlanFile(mod.Path, ws.Name), false, false, fingerprint(ctx, mod)); reason != "" {
		return nil, ApplyOut{}, fmt.Errorf("%s//%s: %s", in.Module, in.Workspace, reason)
	}
	s.logf("apply %s//%s: start", in.Module, in.Workspace)
	ev, err := s.awaitApply(ctx, ws, rec)
	if err != nil {
		return nil, ApplyOut{}, err
	}
	out := ApplyOut{Module: in.Module, Workspace: in.Workspace}
	rec, _ = s.store.LoadRun(mod.Path, ws.Name)
	now := time.Now()
	switch {
	case ev.Phase == runner.PhaseFailed || ev.Phase == runner.PhaseCanceled:
		out.Status = fmt.Sprintf("apply %s: %s", ev.Phase, ev.Err)
	case ev.Aborted:
		out.Status = "apply window closed without finishing — state unknown, re-plan"
		if rec != nil && rec.Apply != nil {
			rec.Apply.Finished, rec.Apply.Aborted = &now, true
			_ = s.store.SaveRun(rec)
		}
	case ev.ApplyExit != nil:
		out.ExitCode = ev.ApplyExit
		if rec != nil && rec.Apply != nil {
			rec.Apply.Finished, rec.Apply.ExitCode = &now, ev.ApplyExit
			_ = s.store.SaveRun(rec)
		}
		if *ev.ApplyExit == 0 {
			out.Status = "applied"
			_ = s.store.DiscardPlan(mod.Path, ws.Name)
			_, _, ix := s.snapshot()
			if rec != nil {
				if marked := report.InvalidateDownstream(s.store, ix, mod, rec, now); len(marked) > 0 {
					out.Status += fmt.Sprintf("; %d downstream workspace(s) now need a plan: %s", len(marked), strings.Join(marked, ", "))
				}
			}
		} else {
			out.Status = fmt.Sprintf("apply failed (exit %d) — its tmux window is left open for inspection", *ev.ApplyExit)
		}
	}
	s.logf("apply %s//%s: %s", in.Module, in.Workspace, out.Status)
	return text(out.Status + "\n"), out, nil
}

// awaitApply runs an apply and records its launch (window id, token) in the
// run record the way the TUI does, so a restart can re-adopt it.
func (s *Server) awaitApply(ctx context.Context, ws *domain.Workspace, rec *state.RunRecord) (runner.Event, error) {
	s.launchHooks.Store(ws.Key(), func(ev runner.Event) {
		if rec != nil && ev.Token != "" {
			rec.Apply = &state.ApplyRecord{Started: time.Now(), WindowID: ev.WindowID, Token: ev.Token}
			_ = s.store.SaveRun(rec)
		}
	})
	defer s.launchHooks.Delete(ws.Key())
	version := ""
	if rec != nil {
		version = rec.TFBinVersion
	}
	// Cancelling a running apply only detaches from it: terraform keeps
	// running in its tmux window, as when the TUI quits.
	return s.await(ctx, runner.KindApply, ws.Key(), func() bool { return s.runner.EnqueueApply(ws, version) })
}

type InitOut struct {
	Module string `json:"module"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}

func (s *Server) initUpgrade(ctx context.Context, _ *mcp.CallToolRequest, in ModuleIn) (*mcp.CallToolResult, InitOut, error) {
	mod, err := s.resolveModule(in.Module)
	if err != nil {
		return nil, InitOut{}, err
	}
	ev, err := s.await(ctx, runner.KindInit, mod.Path, func() bool { return s.runner.EnqueueInitUpgrade(mod) })
	if err != nil {
		return nil, InitOut{}, err
	}
	out := InitOut{Module: in.Module, OK: ev.Phase == runner.PhaseDone, Error: ev.Err}
	s.logf("init_upgrade %s: ok=%v", in.Module, out.OK)
	if !out.OK {
		return nil, out, fmt.Errorf("init -upgrade %s: %s", ev.Phase, ev.Err)
	}
	return text("initialized\n"), out, nil
}

type DiscardOut struct {
	Module    string `json:"module"`
	Workspace string `json:"workspace"`
	Discarded bool   `json:"discarded"`
}

func (s *Server) discardPlan(_ context.Context, _ *mcp.CallToolRequest, in WorkspaceIn) (*mcp.CallToolResult, DiscardOut, error) {
	ws, err := s.workspace(in)
	if err != nil {
		return nil, DiscardOut{}, err
	}
	had := s.store.HasPlanFile(ws.Module.Path, ws.Name)
	if err := s.store.DiscardPlan(ws.Module.Path, ws.Name); err != nil {
		return nil, DiscardOut{}, err
	}
	s.logf("discard_plan %s//%s: had=%v", in.Module, in.Workspace, had)
	out := DiscardOut{Module: in.Module, Workspace: in.Workspace, Discarded: had}
	return text(fmt.Sprintf("discarded: %v\n", had)), out, nil
}

// --- helpers ---

func (s *Server) workspace(in WorkspaceIn) (*domain.Workspace, error) {
	return s.resolveWorkspace(in.Module, in.Workspace)
}

func deref(p *int) any {
	if p == nil {
		return "-"
	}
	return *p
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
