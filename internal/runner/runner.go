// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

// Package runner executes terraform work as scheduled tasks with bounded,
// prioritized parallelism.
//
// Every unit of work — workspace enumeration, init -upgrade, plan, and apply —
// is a task with a uniform lifecycle (Queued → Running → Done/Failed/Canceled)
// reported on Runner.Events. A single scheduler shares one worker pool across
// all kinds:
//
//   - a global slot count caps concurrent work (config `parallelism`)
//   - per-module serialization: at most one task runs in a module directory at
//     a time, because any task can lazily `terraform init` (mutating
//     .terraform/ and the lock file). Within a process this is the busyModule
//     check; across concurrent tfmux instances it's an flock on the module's
//     state-dir lock file, held for the duration of each task.
//   - a priority queue picks the most valuable ready task whose module is free:
//     applies preempt plans, plans preempt enumerations
//   - workspaces are selected via TF_WORKSPACE inside tfexec, never
//     `workspace select`, so cross-workspace jobs don't race
//   - init is always a first-class KindInit task, never run inline by another
//     task: enumerate/plan/output discover they need one (module not
//     initialized, or a run failed with an init-shaped error), enqueue a
//     KindInit task, and re-enqueue themselves — see deferForInit. This keeps
//     a task from holding its slot/module lock while merely waiting for init,
//     and lets a global cap (config `init_parallelism`) serialize init across
//     modules when a shared Terraform/OpenTofu plugin cache requires it,
//     without touching `parallelism`.
//
// Applies run in a tmux window (they're interactive, long-running, and must
// outlive tfmux) but still occupy a pool slot: the task launches the window,
// reports its id, then polls the exit file until it completes. On restart an
// in-flight apply is re-adopted as a poll-only task.
package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/espoon-voltti/tfmux/internal/domain"
	"github.com/espoon-voltti/tfmux/internal/gitstatus"
	"github.com/espoon-voltti/tfmux/internal/state"
	"github.com/espoon-voltti/tfmux/internal/tfexec"
	"github.com/espoon-voltti/tfmux/internal/tmuxctl"
)

// Kind classifies a task.
type Kind int

const (
	KindEnumerate Kind = iota // list a module's workspaces
	KindInit                  // terraform init -upgrade
	KindPlan                  // plan one workspace
	KindApply                 // apply one workspace (runs in tmux)
	KindOutput                // terraform output for one workspace
)

func (k Kind) String() string {
	switch k {
	case KindEnumerate:
		return "enumerate"
	case KindInit:
		return "init"
	case KindPlan:
		return "plan"
	case KindApply:
		return "apply"
	case KindOutput:
		return "output"
	}
	return "?"
}

// Priority orders the ready queue (and the task pane). Higher wins; equal
// priorities run FIFO.
func (k Kind) Priority() int {
	switch k {
	case KindApply:
		return 3
	case KindPlan, KindInit, KindOutput:
		return 2
	case KindEnumerate:
		return 1
	}
	return 0
}

// Attachable reports whether a running task of this kind has a tmux window the
// user can attach to.
func (k Kind) Attachable() bool { return k == KindApply }

// Phase is a task's position in its lifecycle.
type Phase int

const (
	PhaseRunning  Phase = iota // dispatched: a slot + module are held
	PhaseDone                  // finished successfully
	PhaseFailed                // finished with an infrastructure error (see Err)
	PhaseCanceled              // canceled before or during execution
)

// Terminal reports whether the phase ends the task.
func (p Phase) Terminal() bool { return p != PhaseRunning }

// Event reports a task lifecycle transition. Kind/Key/ModulePath identify the
// task (queued state is owned by the caller; the runner reports running and
// terminal transitions). Payload fields are populated per kind.
type Event struct {
	Kind       Kind
	Key        string // module path (enumerate/init) or workspace key (plan/apply)
	ModulePath string
	Phase      Phase

	Workspaces []string         // KindEnumerate, PhaseDone
	Record     *state.RunRecord // KindPlan, terminal
	WindowID   string           // KindApply, PhaseRunning (once the window is up)
	ApplyExit  *int             // KindApply, PhaseDone (exit code; nil when aborted)
	Aborted    bool             // KindApply, PhaseDone (window vanished, outcome unknown)
	Err        string           // PhaseFailed

	// requeueFn, when set, means the task discovered it needs init instead of
	// finishing: execute() runs it (after releasing this task's slot/module/
	// inflight bookkeeping) instead of emitting a terminal event, so the UI
	// never sees a spurious failure for what's really a deferred retry. See
	// deferForInit.
	requeueFn func()
}

// TaskID is a task's stable identity: kind-scoped, so a plan and an apply for
// the same workspace are distinct tasks.
func TaskID(kind Kind, key string) string { return kind.String() + ":" + key }

// TaskID returns the event's task identity.
func (e Event) TaskID() string { return TaskID(e.Kind, e.Key) }

// jobFunc runs a task body. It may report intermediate state (e.g. an apply's
// window id) through emit. The returned Event carries the terminal payload; the
// scheduler stamps its Phase (Done, Failed when Err is set, or Canceled when
// the context was canceled).
type jobFunc func(ctx context.Context, emit func(Event)) Event

type task struct {
	kind       Kind
	key        string
	modulePath string
	seq        int
	ctx        context.Context
	cancel     context.CancelFunc
	job        jobFunc
	started    bool
}

func (t *task) id() string { return TaskID(t.kind, t.key) }

// Runner is a priority-scheduled worker pool shared by every task kind.
// Construct with New.
type Runner struct {
	Events chan Event

	store *state.Store
	tmux  *tmuxctl.Ctl

	mu          sync.Mutex
	cond        *sync.Cond
	parallelism int
	active      int
	initLimit   int // max concurrent KindInit tasks; 0 = unlimited
	activeInits int
	busyModule  map[string]bool  // module path -> a task is running there
	inflight    map[string]*task // id -> task (queued or running)
	ready       []*task
	seq         int

	// Decoupled emission: callers (including the UI thread via Cancel) never
	// block on the Events channel, even when a burst of events outruns the UI.
	emitMu   sync.Mutex
	emitCond *sync.Cond
	pending  []Event
}

// initParallelism caps concurrent KindInit tasks; 0 means unlimited.
func New(parallelism, initParallelism int, store *state.Store, tmux *tmuxctl.Ctl) *Runner {
	if parallelism < 1 {
		parallelism = 1
	}
	r := &Runner{
		Events:      make(chan Event, 64),
		store:       store,
		tmux:        tmux,
		parallelism: parallelism,
		initLimit:   initParallelism,
		busyModule:  map[string]bool{},
		inflight:    map[string]*task{},
	}
	r.cond = sync.NewCond(&r.mu)
	r.emitCond = sync.NewCond(&r.emitMu)
	go r.schedule()
	go r.pump()
	return r
}

// --- scheduling ---

func (r *Runner) schedule() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		for {
			t := r.pickLocked()
			if t == nil {
				break
			}
			r.active++
			if t.kind == KindInit {
				r.activeInits++
			}
			r.busyModule[t.modulePath] = true
			t.started = true
			r.mu.Unlock()
			go r.execute(t)
			r.mu.Lock()
		}
		r.cond.Wait()
	}
}

// pickLocked returns the highest-priority ready task whose module is free, or
// nil if none can run right now. The chosen task is removed from the queue.
func (r *Runner) pickLocked() *task {
	if r.active >= r.parallelism {
		return nil
	}
	best, bi := -1, -1
	for i, t := range r.ready {
		if r.busyModule[t.modulePath] {
			continue
		}
		if t.kind == KindInit && r.initLimit > 0 && r.activeInits >= r.initLimit {
			continue
		}
		if best < 0 {
			best, bi = i, i
			continue
		}
		b := r.ready[best]
		if t.kind.Priority() > b.kind.Priority() ||
			(t.kind.Priority() == b.kind.Priority() && t.seq < b.seq) {
			best, bi = i, i
		}
	}
	if best < 0 {
		return nil
	}
	t := r.ready[bi]
	r.ready = append(r.ready[:bi], r.ready[bi+1:]...)
	return t
}

func (r *Runner) execute(t *task) {
	out := r.runJob(t)

	r.mu.Lock()
	r.active--
	if t.kind == KindInit {
		r.activeInits--
	}
	delete(r.busyModule, t.modulePath)
	delete(r.inflight, t.id())
	r.cond.Signal()
	r.mu.Unlock()

	if out.requeueFn != nil {
		out.requeueFn()
		return
	}
	out.Kind, out.Key, out.ModulePath = t.kind, t.key, t.modulePath
	r.emit(out)
}

// runJob acquires the cross-process module lock, runs the task body, and
// returns its terminal event (Phase stamped; Kind/Key/ModulePath filled in by
// execute). PhaseRunning is emitted only once the lock is held — a task waiting
// on another instance stays "queued" to the UI until it can actually run. The
// caller owns the in-process slot/busyModule bookkeeping.
func (r *Runner) runJob(t *task) Event {
	if t.ctx.Err() != nil {
		return Event{Phase: PhaseCanceled}
	}
	lock, err := r.lockModule(t.ctx, t.modulePath)
	if err != nil {
		if t.ctx.Err() != nil {
			return Event{Phase: PhaseCanceled} // canceled while waiting for the lock
		}
		return Event{Phase: PhaseFailed, Err: "module lock: " + err.Error()}
	}
	defer lock.Close() // releases the flock

	r.emit(Event{Kind: t.kind, Key: t.key, ModulePath: t.modulePath, Phase: PhaseRunning})
	out := t.job(t.ctx, func(e Event) {
		e.Kind, e.Key, e.ModulePath = t.kind, t.key, t.modulePath
		r.emit(e)
	})
	switch {
	case t.ctx.Err() != nil:
		return Event{Phase: PhaseCanceled}
	case out.requeueFn != nil:
		return out // not terminal — execute() runs requeueFn instead of emitting
	case out.Err != "":
		out.Phase = PhaseFailed
	default:
		out.Phase = PhaseDone
	}
	return out
}

// lockModule takes the module's cross-process exclusive lock (held until the
// returned file is closed). The busyModule check already guarantees no other
// task in this process is in the same module dir, so this only ever blocks on a
// different tfmux instance — in which case the wait is exactly the
// serialization we want.
func (r *Runner) lockModule(ctx context.Context, modulePath string) (*os.File, error) {
	path, err := r.store.ModuleLockPath(modulePath)
	if err != nil {
		return nil, err
	}
	return flockExclusive(ctx, path)
}

// enqueue registers a task and wakes the scheduler. Returns false when a task
// with the same identity is already queued or running (dedup).
func (r *Runner) enqueue(kind Kind, key, modulePath string, job jobFunc) bool {
	id := TaskID(kind, key)
	r.mu.Lock()
	if _, ok := r.inflight[id]; ok {
		r.mu.Unlock()
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.seq++
	r.inflight[id] = &task{
		kind: kind, key: key, modulePath: modulePath,
		seq: r.seq, ctx: ctx, cancel: cancel, job: job,
	}
	r.ready = append(r.ready, r.inflight[id])
	r.cond.Signal()
	r.mu.Unlock()
	return true
}

// Cancel stops every task with the given key. A queued task is dropped before
// it runs (emitting Canceled); a running non-apply task receives SIGINT. A
// running apply is left alone — its terraform lives in tmux, so canceling our
// poll wouldn't stop it (killing it is a separate, explicit action).
func (r *Runner) Cancel(key string) {
	var dropped []*task
	r.mu.Lock()
	for id, t := range r.inflight {
		if t.key != key {
			continue
		}
		if t.started {
			if t.kind != KindApply {
				t.cancel()
			}
			continue
		}
		r.removeReadyLocked(t)
		delete(r.inflight, id)
		dropped = append(dropped, t)
	}
	r.mu.Unlock()
	for _, t := range dropped {
		r.emit(Event{Kind: t.kind, Key: t.key, ModulePath: t.modulePath, Phase: PhaseCanceled})
	}
}

func (r *Runner) removeReadyLocked(t *task) {
	for i, q := range r.ready {
		if q == t {
			r.ready = append(r.ready[:i], r.ready[i+1:]...)
			return
		}
	}
}

// Running reports whether a task with the key is queued or running.
func (r *Runner) Running(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.inflight {
		if t.key == key {
			return true
		}
	}
	return false
}

// --- event pump ---

func (r *Runner) emit(e Event) {
	r.emitMu.Lock()
	r.pending = append(r.pending, e)
	r.emitCond.Signal()
	r.emitMu.Unlock()
}

func (r *Runner) pump() {
	for {
		r.emitMu.Lock()
		for len(r.pending) == 0 {
			r.emitCond.Wait()
		}
		batch := r.pending
		r.pending = nil
		r.emitMu.Unlock()
		for _, e := range batch {
			r.Events <- e
		}
	}
}

// --- task kinds ---

// EnqueueEnumerate lists the module's workspaces (lazily initializing) and
// caches the result. Returns false if already in flight.
func (r *Runner) EnqueueEnumerate(m *domain.Module) bool { return r.enqueueEnumerate(m, false) }

func (r *Runner) enqueueEnumerate(m *domain.Module, afterInit bool) bool {
	return r.enqueue(KindEnumerate, m.Path, m.Path, func(ctx context.Context, _ func(Event)) Event {
		tf := tfexec.TF{Bin: m.TFBin, Dir: m.Path, Out: r.taskLog(m.Path, "enumerate")}
		if c, ok := tf.Out.(io.Closer); ok {
			defer c.Close()
		}
		if !tf.Initialized() {
			if afterInit {
				return Event{Err: "workspace list: module not initialized (a preceding terraform init failed)"}
			}
			return r.deferForInit(m, func() { r.enqueueEnumerate(m, true) })
		}
		res, workspaces, err := tf.WorkspaceList(ctx)
		if err != nil {
			return Event{Err: err.Error()}
		}
		if res.ExitCode != 0 {
			if !afterInit && tfexec.NeedsInit(res.Output) {
				return r.deferForInit(m, func() { r.enqueueEnumerate(m, true) })
			}
			return Event{Err: fmt.Sprintf("workspace list in %s failed:\n%s", m.Path, res.Output)}
		}
		// cache the (slow, rate-limited) enumeration for next launch
		_ = r.store.SaveWorkspaces(m.Path, workspaces, time.Now())
		return Event{Workspaces: workspaces}
	})
}

// taskLog opens (truncating) a module-level log file for live streaming, or
// returns nil if it can't be created (streaming is best-effort).
func (r *Runner) taskLog(modulePath, name string) io.Writer {
	path, err := r.store.ModuleLogPath(modulePath, name)
	if err != nil {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil
	}
	return f
}

// EnqueueInitUpgrade runs `terraform init -upgrade` (explicit user action — it
// mutates .terraform.lock.hcl). Returns false if already in flight.
func (r *Runner) EnqueueInitUpgrade(m *domain.Module) bool { return r.enqueueInit(m, true) }

// enqueueInit runs terraform init, either the explicit user-triggered upgrade
// or a plain lazy init a Plan/Output/Enumerate task deferred to. Both share
// TaskID(KindInit, m.Path): if a lazy init for the module is already queued
// the instant an explicit upgrade is requested, the upgrade request is
// dropped (the queued plain init wins) — a rare, harmless race; it never
// produces an unintended upgrade, and pressing `I` again after it completes
// fixes it.
func (r *Runner) enqueueInit(m *domain.Module, upgrade bool) bool {
	return r.enqueue(KindInit, m.Path, m.Path, func(ctx context.Context, _ func(Event)) Event {
		tf := tfexec.TF{Bin: m.TFBin, Dir: m.Path, Out: r.taskLog(m.Path, "init")}
		if c, ok := tf.Out.(io.Closer); ok {
			defer c.Close()
		}
		res, err := tf.Init(ctx, upgrade)
		if err != nil {
			return Event{Err: err.Error()}
		}
		if res.ExitCode != 0 {
			return Event{Err: string(res.Output)}
		}
		return Event{}
	})
}

// deferForInit ends the current task without a terminal event: it ensures a
// plain init is queued for m (a no-op if one is already in flight, including
// an explicit upgrade) and re-enqueues the dependent task via retry.
// execute() runs retry once this task's slot/module/inflight bookkeeping is
// cleared, so the re-enqueue never collides with this task's own still-
// registered id.
//
// Ordering is guaranteed without extra plumbing: enqueueInit is called before
// retry, so the init task always gets a strictly lower seq (enqueue
// increments seq under r.mu); KindInit's priority is >= every kind that can
// defer to it, so pickLocked always picks the init first when both are ready
// for the same module, and once picked, busyModule excludes the dependent
// task until the init's execute() clears it.
func (r *Runner) deferForInit(m *domain.Module, retry func()) Event {
	return Event{requeueFn: func() {
		r.enqueueInit(m, false)
		retry()
	}}
}

// EnqueuePlan plans one workspace, persisting the RunRecord + plan file + log.
// Returns false if already in flight.
func (r *Runner) EnqueuePlan(w *domain.Workspace) bool { return r.enqueuePlan(w, false) }

func (r *Runner) enqueuePlan(w *domain.Workspace, afterInit bool) bool {
	m := w.Module
	tf := tfexec.TF{Bin: m.TFBin, Dir: m.Path}
	return r.enqueue(KindPlan, w.Key(), m.Path, func(ctx context.Context, _ func(Event)) Event {
		if !tf.Initialized() {
			if afterInit {
				return Event{Err: "plan: module not initialized (a preceding terraform init failed)"}
			}
			return r.deferForInit(m, func() { r.enqueuePlan(w, true) })
		}
		rec, needsInit, err := r.plan(ctx, tf, m, w.Name)
		if !afterInit && needsInit {
			return r.deferForInit(m, func() { r.enqueuePlan(w, true) })
		}
		ev := Event{Record: rec}
		if err != nil {
			ev.Err = err.Error()
		}
		return ev
	})
}

// EnqueueOutput runs `terraform output` for one workspace, capturing it to a
// log the UI can show. Returns false if already in flight.
func (r *Runner) EnqueueOutput(w *domain.Workspace) bool { return r.enqueueOutput(w, false) }

func (r *Runner) enqueueOutput(w *domain.Workspace, afterInit bool) bool {
	m := w.Module
	return r.enqueue(KindOutput, w.Key(), m.Path, func(ctx context.Context, _ func(Event)) Event {
		tf := tfexec.TF{Bin: m.TFBin, Dir: m.Path}
		if !tf.Initialized() {
			if afterInit {
				return Event{Err: "output: module not initialized (a preceding terraform init failed)"}
			}
			return r.deferForInit(m, func() { r.enqueueOutput(w, true) })
		}
		logPath, logErr := r.store.OutputLogPath(m.Path, w.Name)
		if logErr == nil {
			if f, ferr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); ferr == nil {
				tf.Out = f
				defer f.Close()
			}
		}
		res, err := tf.Output(ctx, w.Name)
		if tf.Out == nil && logErr == nil {
			_ = os.WriteFile(logPath, res.Output, 0o600) // fallback if streaming setup failed
		}
		if err != nil {
			return Event{Err: err.Error()}
		}
		if res.ExitCode != 0 {
			if !afterInit && tfexec.NeedsInit(res.Output) {
				return r.deferForInit(m, func() { r.enqueueOutput(w, true) })
			}
			return Event{Err: string(res.Output)}
		}
		return Event{}
	})
}

// EnqueueApply launches an apply for one workspace in tmux and watches it to
// completion, holding a pool slot the whole time. plannedVersion guards against
// applying a plan made with a different terraform binary. Returns false if
// already in flight.
//
// The module lock is held (by runJob) for as long as we watch the apply, so a
// concurrent instance can't touch the module mid-apply. The one gap: the
// apply's terraform runs in tmux and outlives tfmux, so quitting tfmux during
// an apply drops the lock early. A restart re-adopts the apply (EnqueueApplyPoll)
// and re-takes the lock; Terraform's own backend state lock covers the gap.
func (r *Runner) EnqueueApply(w *domain.Workspace, plannedVersion string) bool {
	m := w.Module
	return r.enqueue(KindApply, w.Key(), m.Path, func(ctx context.Context, emit func(Event)) Event {
		if plannedVersion != "" {
			if cur, err := (tfexec.TF{Bin: m.TFBin, Dir: m.Path}).Version(ctx); err == nil && cur != plannedVersion {
				return Event{Err: fmt.Sprintf(
					"refusing to apply: plan made with %s %s, current is %s — re-plan",
					m.TFBin, plannedVersion, cur)}
			}
		}
		planFile, err := r.store.PlanFilePath(m.Path, w.Name)
		if err != nil {
			return Event{Err: err.Error()}
		}
		exitFile, err := r.store.ApplyExitPath(m.Path, w.Name)
		if err != nil {
			return Event{Err: err.Error()}
		}
		windowID, err := r.tmux.LaunchApply(tmuxctl.ApplySpec{
			ModuleDir: m.Path,
			Workspace: w.Name,
			TFBin:     m.TFBin,
			PlanFile:  planFile,
			ExitFile:  exitFile,
			Name:      m.Repo.Name + "/" + w.Name,
		})
		if err != nil {
			return Event{Err: err.Error()}
		}
		emit(Event{Phase: PhaseRunning, WindowID: windowID})
		return r.pollApply(ctx, m.Path, w.Name, windowID, exitFile)
	})
}

// EnqueueApplyPoll re-adopts an apply that was already running in tmux when
// tfmux last exited: it polls the existing window/exit file without relaunching.
func (r *Runner) EnqueueApplyPoll(modulePath, workspace, windowID string) bool {
	return r.enqueue(KindApply, modulePath+"//"+workspace, modulePath, func(ctx context.Context, emit func(Event)) Event {
		exitFile, err := r.store.ApplyExitPath(modulePath, workspace)
		if err != nil {
			return Event{Err: err.Error()}
		}
		emit(Event{Phase: PhaseRunning, WindowID: windowID})
		return r.pollApply(ctx, modulePath, workspace, windowID, exitFile)
	})
}

// pollApply watches the apply: it returns once the exit file appears (the
// wrapper finished) or the window vanishes (aborted). Canceling the context
// stops the poll only — the tmux window keeps running.
func (r *Runner) pollApply(ctx context.Context, modulePath, workspace, windowID, exitFile string) Event {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if data, err := os.ReadFile(exitFile); err == nil {
			code := parseExit(data)
			return Event{ApplyExit: &code}
		}
		if r.tmux != nil && windowID != "" {
			if ids, err := r.tmux.ListWindowIDs(); err == nil && !ids[windowID] {
				return Event{Aborted: true}
			}
		}
		select {
		case <-ctx.Done():
			return Event{}
		case <-ticker.C:
		}
	}
}

func parseExit(b []byte) int {
	var code int
	fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &code)
	return code
}

// plan runs terraform plan for one workspace. The returned needsInit reports
// whether the run failed with an init-shaped error, regardless of err —
// callers decide whether to act on it (see enqueuePlan's afterInit).
func (r *Runner) plan(ctx context.Context, tf tfexec.TF, m *domain.Module, workspace string) (rec *state.RunRecord, needsInit bool, err error) {
	planFile, err := r.store.PlanFilePath(m.Path, workspace)
	if err != nil {
		return nil, false, err
	}
	rec = &state.RunRecord{
		ModulePath:  m.Path,
		Workspace:   workspace,
		PlanStarted: time.Now(),
	}

	// Stream the plan's output to the log file as it runs, so the UI can
	// follow it live. The file is truncated up front (so a stale log never
	// shows) and exists the moment the task starts. A separate TF carries the
	// tee so the show/version enrichment calls below don't pollute the log.
	planTF := tf
	logPath, logErr := r.store.PlanLogPath(m.Path, workspace)
	if logErr == nil {
		if logFile, ferr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); ferr == nil {
			planTF.Out = logFile
			defer logFile.Close()
		}
	}

	res, err := planTF.Plan(ctx, workspace, planFile)
	rec.PlanFinished = time.Now()
	if planTF.Out == nil && logErr == nil {
		_ = os.WriteFile(logPath, res.Output, 0o600) // fallback if streaming setup failed
	}
	if err != nil {
		return nil, false, err
	}
	needsInit = res.ExitCode == tfexec.PlanError && tfexec.NeedsInit(res.Output)
	rec.PlanExitCode = res.ExitCode
	if res.ExitCode == tfexec.PlanError {
		rec.PlanErrorKind = string(tfexec.ClassifyPlanError(res.Output))
	}
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
		return rec, needsInit, err
	}
	return rec, needsInit, nil
}
