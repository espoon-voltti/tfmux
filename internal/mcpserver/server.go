// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

// Package mcpserver exposes tfmux as an MCP server for an agent running
// somewhere that must not hold credentials, such as a sandbox. The read-only
// tool set inspects persisted state and runs plan/output/workspace
// enumeration; the write set (apply, init -upgrade, discard) is only
// registered when asked for. Everything goes through the same runner, store
// and assessment code as the TUI, so the two agree and can run at once.
package mcpserver

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/espoon-voltti/tfmux/internal/config"
	"github.com/espoon-voltti/tfmux/internal/deps"
	"github.com/espoon-voltti/tfmux/internal/domain"
	"github.com/espoon-voltti/tfmux/internal/gitstatus"
	"github.com/espoon-voltti/tfmux/internal/report"
	"github.com/espoon-voltti/tfmux/internal/runner"
	"github.com/espoon-voltti/tfmux/internal/state"
	"github.com/espoon-voltti/tfmux/internal/tfexec"
)

// Options configures New.
type Options struct {
	Cfg      *config.Config
	Store    *state.Store
	AllTools bool        // register the write tools too
	Version  string      // reported to clients
	Log      *log.Logger // tool calls and outcomes; nil silences
}

// Server owns the discovered estate, one runner, and the MCP server built
// over them.
type Server struct {
	cfg      *config.Config
	store    *state.Store
	runner   *runner.Runner
	allTools bool
	log      *log.Logger
	mcp      *mcp.Server

	mu     sync.Mutex // guards estate below
	repos  []*domain.Repo
	ignore state.Ignore
	index  *deps.Index

	waitMu        sync.Mutex
	waiters       map[string][]chan runner.Event
	applyOutcomes map[string]applyOutcome // workspace key -> outcome awaiting its tool call
}

// New discovers the estate, starts the runner and registers the tools.
func New(ctx context.Context, o Options) (*Server, error) {
	if o.Log == nil {
		o.Log = log.New(discard{}, "", 0)
	}
	s := &Server{
		cfg:           o.Cfg,
		store:         o.Store,
		allTools:      o.AllTools,
		log:           o.Log,
		waiters:       map[string][]chan runner.Event{},
		applyOutcomes: map[string]applyOutcome{},
	}
	if err := s.discover(ctx); err != nil {
		return nil, err
	}
	s.runner = runner.New(o.Cfg.Parallelism, o.Cfg.EffectiveInitParallelism(), o.Store)
	go s.pump()
	s.mcp = mcp.NewServer(&mcp.Implementation{Name: "tfmux", Version: o.Version}, &mcp.ServerOptions{
		Instructions: instructions(o.AllTools),
	})
	s.register()
	return s, nil
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// MCP returns the server to serve over a transport.
func (s *Server) MCP() *mcp.Server { return s.mcp }

// Close interrupts every task, including applies no call waits on any more,
// and waits for terraform to stop so no state lock is left held.
func (s *Server) Close() {
	if n := s.runner.Shutdown(tfexec.InterruptGrace + 5*time.Second); n > 0 {
		s.logf("shutdown: %d task(s) still running after %s", n, tfexec.InterruptGrace)
	}
}

func instructions(all bool) string {
	b := &strings.Builder{}
	b.WriteString("tfmux manages terraform root modules across git repos. Modules are addressed as \"repo/relative/path\" ")
	b.WriteString("(exactly as `list`, `pending` and `deps` print them) plus a workspace name. ")
	b.WriteString("`plan` and `output` run terraform on the host with its credentials and block until done; results are also saved for the tfmux TUI. ")
	if all {
		b.WriteString("Write tools (apply, init_upgrade, discard_plan) are enabled: apply only a plan you have just reviewed with plan_log.")
	} else {
		b.WriteString("This server is read-only: nothing here changes infrastructure. Ask the operator to apply.")
	}
	return b.String()
}

// --- estate ---

// discover (re)reads the repos, their git status, workspace lists, and
// per-module terraform binaries.
func (s *Server) discover(ctx context.Context) error {
	repos, err := report.DiscoverWithGit(ctx, s.cfg)
	if err != nil {
		return err
	}
	ignore, err := s.store.LoadIgnore()
	if err != nil {
		return err
	}
	for _, repo := range repos {
		for _, mod := range repo.Modules {
			mod.TFBin = s.cfg.BinFor(repo.Path)
			if names, ok := report.Workspaces(s.store, mod); ok {
				setWorkspaces(mod, names)
			}
		}
	}
	s.mu.Lock()
	s.repos, s.ignore = repos, ignore
	s.index = deps.Build(report.VisibleRepos(ignore, repos))
	s.mu.Unlock()
	return nil
}

func setWorkspaces(mod *domain.Module, names []string) {
	mod.WorkspaceState = domain.WorkspacesReady
	mod.WorkspaceErr = ""
	mod.Workspaces = nil
	for _, n := range names {
		mod.Workspaces = append(mod.Workspaces, &domain.Workspace{Module: mod, Name: n})
	}
}

// snapshot returns the current repos and ignore list.
func (s *Server) snapshot() ([]*domain.Repo, state.Ignore, *deps.Index) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.repos, s.ignore, s.index
}

// resolveModule finds a visible module by its "repo/relpath" label. The
// error for a miss names the closest labels so the caller can correct.
func (s *Server) resolveModule(label string) (*domain.Module, error) {
	repos, ignore, _ := s.snapshot()
	label = strings.TrimSuffix(strings.TrimSpace(label), "/")
	var candidates []string
	for _, repo := range repos {
		for _, mod := range repo.Modules {
			if ignore.HidesModule(mod) {
				continue
			}
			l := report.ModuleLabel(mod)
			if l == label {
				return mod, nil
			}
			if strings.Contains(l, label) || strings.Contains(label, l) {
				candidates = append(candidates, l)
			}
		}
	}
	sort.Strings(candidates)
	if len(candidates) > 0 {
		if len(candidates) > 8 {
			candidates = append(candidates[:8], "…")
		}
		return nil, fmt.Errorf("no module %q; did you mean: %s", label, strings.Join(candidates, ", "))
	}
	return nil, fmt.Errorf("no module %q (use `list` for the labels)", label)
}

// resolveWorkspace finds a module's workspace by name.
func (s *Server) resolveWorkspace(label, workspace string) (*domain.Workspace, error) {
	mod, err := s.resolveModule(label)
	if err != nil {
		return nil, err
	}
	// Workspace lists are replaced by pump under s.mu as enumerations finish.
	s.mu.Lock()
	defer s.mu.Unlock()
	if mod.WorkspaceState != domain.WorkspacesReady {
		return nil, fmt.Errorf("%s: workspaces unknown; call enumerate_workspaces first", label)
	}
	var names []string
	for _, ws := range mod.Workspaces {
		if ws.Name == workspace {
			return ws, nil
		}
		names = append(names, ws.Name)
	}
	return nil, fmt.Errorf("%s has no workspace %q (has: %s)", label, workspace, strings.Join(names, ", "))
}

// fingerprint computes a module's current content fingerprint.
func fingerprint(ctx context.Context, mod *domain.Module) string {
	fp, _ := gitstatus.Fingerprint(ctx, mod.Repo.Path, mod.FingerprintPaths())
	return fp
}

// --- runner plumbing ---

// pump delivers each terminal runner event to the tool calls waiting on
// that task, keeps the estate's workspace lists in step with enumerations,
// and records every apply's outcome. The bookkeeping happens here rather
// than in the tool call so that a client that disconnects mid-apply still
// leaves a complete run record behind.
func (s *Server) pump() {
	for ev := range s.runner.Events {
		if !ev.Phase.Terminal() {
			continue
		}
		switch {
		case ev.Kind == runner.KindEnumerate && ev.Phase == runner.PhaseDone:
			s.mu.Lock()
			if mod := s.findModule(ev.ModulePath); mod != nil {
				setWorkspaces(mod, ev.Workspaces)
			}
			s.mu.Unlock()
		case ev.Kind == runner.KindApply && ev.Phase != runner.PhaseFailed:
			outcome := s.finishApply(ev)
			s.logf("apply %s: %s", ev.Key, outcome.status)
			s.waitMu.Lock()
			if len(s.waiters[runner.TaskID(ev.Kind, ev.Key)]) > 0 {
				s.applyOutcomes[ev.Key] = outcome
			}
			s.waitMu.Unlock()
		}
		id := runner.TaskID(ev.Kind, ev.Key)
		s.waitMu.Lock()
		chans := s.waiters[id]
		delete(s.waiters, id)
		s.waitMu.Unlock()
		for _, ch := range chans {
			ch <- ev
		}
	}
}

// applyOutcome is what finishApply made of an apply's terminal event, for
// the tool call that launched it.
type applyOutcome struct {
	status string
	marked []string // downstream workspace keys now needing a plan
}

// finishApply describes an apply's end (the runner has already saved its
// record and discarded the plan file of a success) and, on success, marks
// downstream workspaces.
func (s *Server) finishApply(ev runner.Event) applyOutcome {
	rec := ev.Record
	var out applyOutcome
	switch {
	case rec == nil || rec.Apply == nil:
		out.status = "apply canceled before it started"
	case rec.Apply.Aborted:
		out.status = "apply interrupted — state unknown, re-plan"
	case rec.Apply.ExitCode == nil:
		out.status = "apply ended without an exit code"
	case *rec.Apply.ExitCode != 0:
		out.status = fmt.Sprintf("apply failed (exit %d); its output follows", *rec.Apply.ExitCode)
	default:
		out.status = "applied"
		s.mu.Lock()
		if up := s.findModule(ev.ModulePath); up != nil {
			out.marked = report.InvalidateDownstream(s.store, s.index, up, rec, *rec.Apply.Finished)
		}
		s.mu.Unlock()
		if len(out.marked) > 0 {
			out.status += fmt.Sprintf("; %d downstream workspace(s) now need a plan: %s", len(out.marked), strings.Join(out.marked, ", "))
		}
	}
	return out
}

// takeApplyOutcome returns and forgets the outcome stored for a workspace's
// apply while a tool call was waiting on it.
func (s *Server) takeApplyOutcome(key string) (applyOutcome, bool) {
	s.waitMu.Lock()
	defer s.waitMu.Unlock()
	out, ok := s.applyOutcomes[key]
	delete(s.applyOutcomes, key)
	return out, ok
}

// splitKey returns the workspace name of a workspace key.
func splitKey(key string) string {
	if i := strings.LastIndex(key, "//"); i >= 0 {
		return key[i+2:]
	}
	return key
}

// findModule must be called with s.mu held.
func (s *Server) findModule(path string) *domain.Module {
	for _, repo := range s.repos {
		for _, mod := range repo.Modules {
			if mod.Path == path {
				return mod
			}
		}
	}
	return nil
}

// await registers interest in a task's terminal event, enqueues it, and
// blocks until the event arrives or ctx ends, in which case the task is
// cancelled — except an apply, which a client going away must not interrupt
// mid-flight; it runs on and pump records its outcome. A task already in
// flight (enqueue returns false) is simply waited for.
func (s *Server) await(ctx context.Context, kind runner.Kind, key string, enqueue func() bool) (runner.Event, error) {
	id := runner.TaskID(kind, key)
	ch := make(chan runner.Event, 1)
	s.waitMu.Lock()
	s.waiters[id] = append(s.waiters[id], ch)
	s.waitMu.Unlock()
	enqueue()
	select {
	case ev := <-ch:
		return ev, nil
	case <-ctx.Done():
		s.waitMu.Lock()
		chans := s.waiters[id]
		for i, c := range chans {
			if c == ch {
				s.waiters[id] = append(chans[:i], chans[i+1:]...)
				break
			}
		}
		if len(s.waiters[id]) == 0 {
			delete(s.waiters, id)
			if kind != runner.KindApply {
				s.runner.Cancel(kind, key)
			}
		}
		s.waitMu.Unlock()
		return runner.Event{}, ctx.Err()
	}
}

func (s *Server) logf(format string, args ...any) {
	s.log.Printf(time.Now().Format("15:04:05")+" "+format, args...)
}
