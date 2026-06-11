// Package runner executes terraform jobs with bounded parallelism.
//
// Concurrency policy:
//   - a global semaphore caps concurrent terraform subprocesses (config
//     `parallelism`)
//   - a per-module-directory mutex serializes jobs within one module: any
//     job can lazily turn into a `terraform init`, which mutates .terraform/
//     and the lock file, so same-module concurrency is never safe
//   - workspaces are selected via TF_WORKSPACE inside tfexec, never
//     `workspace select`, so cross-workspace jobs don't race on
//     .terraform/environment
//
// Every job emits Events; the TUI bridges the channel into tea.Msgs.
package runner

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/japsu/tfmux/internal/domain"
	"github.com/japsu/tfmux/internal/gitstatus"
	"github.com/japsu/tfmux/internal/state"
	"github.com/japsu/tfmux/internal/tfexec"
)

// Event is delivered on Runner.Events. All event types are value structs.
type Event any

type EnumStarted struct{ ModulePath string }
type EnumFinished struct {
	ModulePath string
	Workspaces []string
	Err        string // empty on success
}

type PlanStarted struct{ Key string } // Key = modulePath + "//" + workspace
type PlanFinished struct {
	Key    string
	Record *state.RunRecord
	Err    string // infrastructure failure (not plan exit 1, which is in Record)
}

type InitStarted struct{ ModulePath string }
type InitFinished struct {
	ModulePath string
	Err        string
}

// Runner owns the worker pool. Construct with New.
type Runner struct {
	Events chan Event

	store *state.Store
	sem   chan struct{}

	mu        sync.Mutex
	perModule map[string]*sync.Mutex
	inflight  map[string]context.CancelFunc // job key -> cancel
}

func New(parallelism int, store *state.Store) *Runner {
	return &Runner{
		Events:    make(chan Event, 64),
		store:     store,
		sem:       make(chan struct{}, parallelism),
		perModule: map[string]*sync.Mutex{},
		inflight:  map[string]context.CancelFunc{},
	}
}

func (r *Runner) moduleLock(path string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.perModule[path]
	if !ok {
		m = &sync.Mutex{}
		r.perModule[path] = m
	}
	return m
}

// claim registers a cancellable job under key. Returns false when a job with
// the same key is already queued or running (dedup).
func (r *Runner) claim(key string) (context.Context, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.inflight[key]; exists {
		return nil, false
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.inflight[key] = cancel
	return ctx, true
}

func (r *Runner) release(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cancel, ok := r.inflight[key]; ok {
		cancel()
		delete(r.inflight, key)
	}
}

// Cancel aborts the queued/running job with the given key (module path for
// enumeration, workspace key for plans). The subprocess receives SIGINT.
func (r *Runner) Cancel(key string) {
	r.mu.Lock()
	cancel, ok := r.inflight[key]
	r.mu.Unlock()
	if ok {
		cancel()
	}
}

// Running reports whether a job with the key is queued or running.
func (r *Runner) Running(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.inflight[key]
	return ok
}

// run executes job with the standard slot + module lock acquisition.
func (r *Runner) run(key, modulePath string, job func(ctx context.Context)) bool {
	ctx, ok := r.claim(key)
	if !ok {
		return false
	}
	go func() {
		defer r.release(key)
		select {
		case r.sem <- struct{}{}:
			defer func() { <-r.sem }()
		case <-ctx.Done():
			return
		}
		lock := r.moduleLock(modulePath)
		lock.Lock()
		defer lock.Unlock()
		if ctx.Err() != nil {
			return
		}
		job(ctx)
	}()
	return true
}

// EnqueueEnumerate lists the module's workspaces (lazily initializing) and
// emits EnumFinished. Returns false if already in flight.
func (r *Runner) EnqueueEnumerate(m *domain.Module) bool {
	tf := tfexec.TF{Bin: m.TFBin, Dir: m.Path}
	return r.run(m.Path, m.Path, func(ctx context.Context) {
		r.Events <- EnumStarted{ModulePath: m.Path}
		workspaces, err := tf.WorkspaceList(ctx)
		ev := EnumFinished{ModulePath: m.Path, Workspaces: workspaces}
		if err != nil {
			ev.Err = err.Error()
		}
		r.Events <- ev
	})
}

// EnqueueInitUpgrade runs `terraform init -upgrade` (explicit user action —
// it mutates .terraform.lock.hcl) and emits InitFinished.
func (r *Runner) EnqueueInitUpgrade(m *domain.Module) bool {
	tf := tfexec.TF{Bin: m.TFBin, Dir: m.Path}
	return r.run(m.Path+"#init", m.Path, func(ctx context.Context) {
		r.Events <- InitStarted{ModulePath: m.Path}
		res, err := tf.Init(ctx, true)
		ev := InitFinished{ModulePath: m.Path}
		if err != nil {
			ev.Err = err.Error()
		} else if res.ExitCode != 0 {
			ev.Err = string(res.Output)
		}
		r.Events <- ev
	})
}

// EnqueuePlan plans one workspace, persists the RunRecord + plan file + log,
// and emits PlanFinished. Returns false if already in flight.
func (r *Runner) EnqueuePlan(w *domain.Workspace) bool {
	m := w.Module
	key := w.Key()
	tf := tfexec.TF{Bin: m.TFBin, Dir: m.Path}
	return r.run(key, m.Path, func(ctx context.Context) {
		r.Events <- PlanStarted{Key: key}
		rec, err := r.plan(ctx, tf, m, w.Name)
		ev := PlanFinished{Key: key, Record: rec}
		if err != nil {
			ev.Err = err.Error()
		}
		r.Events <- ev
	})
}

func (r *Runner) plan(ctx context.Context, tf tfexec.TF, m *domain.Module, workspace string) (*state.RunRecord, error) {
	planFile, err := r.store.PlanFilePath(m.Path, workspace)
	if err != nil {
		return nil, err
	}
	rec := &state.RunRecord{
		ModulePath:  m.Path,
		Workspace:   workspace,
		PlanStarted: time.Now(),
	}

	res, err := tf.Plan(ctx, workspace, planFile)
	rec.PlanFinished = time.Now()
	if logPath, lerr := r.store.PlanLogPath(m.Path, workspace); lerr == nil {
		_ = os.WriteFile(logPath, res.Output, 0o600)
	}
	if err != nil {
		return nil, err
	}
	rec.PlanExitCode = res.ExitCode
	_ = os.Chmod(planFile, 0o600) // terraform writes 0644 by default

	// Best-effort enrichment; failures here must not fail the plan.
	if res.ExitCode == tfexec.PlanChanges {
		if plan, err := tf.ShowPlan(ctx, planFile); err == nil {
			rec.Summary = summarize(plan)
		}
	}
	if res.ExitCode != tfexec.PlanError {
		if v, err := tf.Version(ctx); err == nil {
			rec.TFBinVersion = v
		}
		if head, err := gitstatus.Head(ctx, m.Repo.Path); err == nil {
			rec.GitHead = head
		}
		if dh, err := gitstatus.DirtyHash(ctx, m.Repo.Path, m.RelPath); err == nil {
			rec.DirtyHash = dh
		}
	}
	// A clean or failed plan leaves no plan file worth applying.
	if res.ExitCode != tfexec.PlanChanges {
		_ = r.store.DiscardPlan(m.Path, workspace)
	}
	if err := r.store.SaveRun(rec); err != nil {
		return rec, err
	}
	return rec, nil
}
