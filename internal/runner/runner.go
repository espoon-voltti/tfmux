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
//     KindInit task, and go back to the ready queue at their original
//     position — see deferForInit. This keeps a task from holding its
//     slot/module lock while merely waiting for init, and lets a global cap
//     (config `init_parallelism`) serialize init across modules when a shared
//     Terraform/OpenTofu plugin cache requires it, without touching
//     `parallelism`. A queued or running init also holds back every other
//     kind of task in that module (an apply excepted, since it preempts), so
//     a throttled init can't be overtaken by the plan waiting on it. A failed init cancels the tasks queued behind it in its module
//     rather than letting them run against a module it left broken.
//   - a module whose manifest init template depends on the workspace can be
//     initialised for one workspace at a time. The state store's
//     init.workspace marker records which; plan/output/apply for another
//     workspace defer to a fresh init first. The single per-module init task
//     picks its workspace when it runs, from the earliest task waiting on it,
//     so a plan can never be told "already initialised" for the wrong one.
//
// Every task is a child process of this one and ends with it: Shutdown
// interrupts whatever is running and waits for terraform to stop, so a quit
// never leaves a state lock held. Nothing survives tfmux exiting.
package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/espoon-voltti/tfmux/internal/domain"
	"github.com/espoon-voltti/tfmux/internal/gitstatus"
	"github.com/espoon-voltti/tfmux/internal/state"
	"github.com/espoon-voltti/tfmux/internal/tfexec"
)

// Kind classifies a task.
type Kind int

const (
	KindEnumerate Kind = iota // list a module's workspaces
	KindInit                  // terraform init -upgrade
	KindPlan                  // plan one workspace
	KindApply                 // apply one workspace's saved plan
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

// Phase is a task's position in its lifecycle.
type Phase int

const (
	PhaseRunning  Phase = iota // dispatched: a slot + module are held
	PhaseDone                  // finished successfully
	PhaseFailed                // finished with an infrastructure error (see Err)
	PhaseCanceled              // canceled before or during execution
	PhaseQueued                // gave its slot back to wait for an init (see deferForInit)
)

// Terminal reports whether the phase ends the task.
func (p Phase) Terminal() bool { return p != PhaseRunning && p != PhaseQueued }

func (p Phase) String() string {
	switch p {
	case PhaseRunning:
		return "running"
	case PhaseQueued:
		return "queued"
	case PhaseDone:
		return "done"
	case PhaseFailed:
		return "failed"
	case PhaseCanceled:
		return "canceled"
	}
	return fmt.Sprintf("phase(%d)", int(p))
}

// Event reports a task lifecycle transition. Kind/Key/ModulePath identify the
// task (queued state is owned by the caller; the runner reports running and
// terminal transitions). Payload fields are populated per kind.
type Event struct {
	Kind       Kind
	Key        string // module path (enumerate/init) or workspace key (plan/apply)
	ModulePath string
	Phase      Phase

	Workspaces []string         // KindEnumerate, PhaseDone
	Record     *state.RunRecord // KindPlan and KindApply, terminal: the saved record (nil when terraform never ran)
	Err        string           // PhaseFailed; for PhaseCanceled, the failed init that dropped the task, when that is why

	// CanceledDependents counts the module's queued tasks dropped because they
	// were waiting on this failed init (KindInit, PhaseFailed). Each still
	// gets its own PhaseCanceled event carrying this init's Err.
	CanceledDependents int

	// deferral, when set, means the task discovered it needs init instead of
	// finishing: execute() queues the init and puts the task back in the
	// ready queue, emitting PhaseQueued rather than a terminal event. See
	// deferForInit.
	deferral *deferral
}

// deferral is a task's request to run again once its module has been
// initialised: the init to queue and the body to run on the retry.
type deferral struct {
	module *domain.Module
	wantWS string
	retry  jobFunc
}

// TaskID is a task's stable identity: kind-scoped, so a plan and an apply for
// the same workspace are distinct tasks.
func TaskID(kind Kind, key string) string { return kind.String() + ":" + key }

// TaskID returns the event's task identity.
func (e Event) TaskID() string { return TaskID(e.Kind, e.Key) }

// jobFunc runs a task body. It may report intermediate state through emit.
// The returned Event carries the terminal payload; the scheduler stamps its
// Phase (Done, Failed when Err is set, or Canceled when the context was
// canceled, keeping only Record).
type jobFunc func(ctx context.Context, emit func(Event)) Event

type task struct {
	kind       Kind
	key        string
	modulePath string
	wantWS     string // workspace the task needs the module initialised for; "" if any
	seq        int
	ctx        context.Context
	cancel     context.CancelFunc
	job        jobFunc
	started    bool
	deferred   bool // went back to the queue to wait for an init (see deferLocked)
}

func (t *task) id() string { return TaskID(t.kind, t.key) }

// Runner is a priority-scheduled worker pool shared by every task kind.
// Construct with New.
type Runner struct {
	Events chan Event

	store *state.Store

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
func New(parallelism, initParallelism int, store *state.Store) *Runner {
	if parallelism < 1 {
		parallelism = 1
	}
	r := &Runner{
		Events:      make(chan Event, 64),
		store:       store,
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
		if t.kind == KindInit {
			if r.initLimit > 0 && r.activeInits >= r.initLimit {
				continue
			}
		} else if (t.kind != KindApply || t.deferred) && r.initQueuedLocked(t.modulePath) {
			// An apply is exempt unless it asked for that init itself: it
			// preempts, and its plan file was made against the module's
			// current lock file, not the upgraded one.
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

// initQueuedLocked reports whether an init task for the module is queued or
// running. Nothing else may enter that module first: the init exists because
// the user asked for an upgrade or because another task deferred to it, and
// priority/FIFO order alone doesn't hold a plan back while initLimit keeps the
// init waiting.
func (r *Runner) initQueuedLocked(modulePath string) bool {
	_, ok := r.inflight[TaskID(KindInit, modulePath)]
	return ok
}

func (r *Runner) execute(t *task) {
	out := r.runJob(t)

	r.mu.Lock()
	r.active--
	if t.kind == KindInit {
		r.activeInits--
	}
	delete(r.busyModule, t.modulePath)
	var dependents []*task
	if out.deferral != nil {
		// Readied and the init queued under the same lock that frees the
		// module, so the scheduler can't slip another task into the module
		// (or this one back in) before the init exists to hold them back.
		r.deferLocked(t, out.deferral)
	} else {
		delete(r.inflight, t.id())
		// Dropped under the same lock that frees the module, so the scheduler
		// can't dispatch a dependent in between.
		if t.kind == KindInit && out.Phase == PhaseFailed {
			dependents = r.dropInitDependentsLocked(t.modulePath)
		}
	}
	r.cond.Signal()
	r.mu.Unlock()

	if out.deferral != nil {
		r.emit(Event{Kind: t.kind, Key: t.key, ModulePath: t.modulePath, Phase: PhaseQueued})
		return
	}
	out.Kind, out.Key, out.ModulePath = t.kind, t.key, t.modulePath
	out.CanceledDependents = len(dependents)
	r.emit(out)
	for _, d := range dependents {
		r.emit(Event{Kind: d.kind, Key: d.key, ModulePath: d.modulePath, Phase: PhaseCanceled, Err: "terraform init failed:\n" + out.Err})
	}
}

// deferLocked returns t to the ready queue with its identity, seq and context
// intact, so it keeps its place in FIFO order ahead of tasks queued after it,
// and queues a plain init for its module unless one is already in flight.
func (r *Runner) deferLocked(t *task, d *deferral) {
	t.job = d.retry
	t.started = false
	t.deferred = true
	r.ready = append(r.ready, t)
	r.enqueueLocked(KindInit, d.module.Path, d.module.Path, "", r.initJob(d.module, false, d.wantWS))
}

// dropInitDependentsLocked removes the queued tasks that initQueuedLocked was
// holding back in the module: everything but applies that never deferred.
func (r *Runner) dropInitDependentsLocked(modulePath string) []*task {
	var dropped []*task
	kept := r.ready[:0]
	for _, q := range r.ready {
		if q.modulePath == modulePath && (q.kind != KindApply || q.deferred) {
			delete(r.inflight, q.id())
			q.cancel()
			dropped = append(dropped, q)
			continue
		}
		kept = append(kept, q)
	}
	r.ready = kept
	return dropped
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
		return Event{Phase: PhaseCanceled, Record: out.Record}
	case out.deferral != nil:
		return out // not terminal — execute() requeues instead of emitting
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
func (r *Runner) enqueue(kind Kind, key, modulePath, wantWS string, job jobFunc) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.enqueueLocked(kind, key, modulePath, wantWS, job)
}

func (r *Runner) enqueueLocked(kind Kind, key, modulePath, wantWS string, job jobFunc) bool {
	id := TaskID(kind, key)
	if _, ok := r.inflight[id]; ok {
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.seq++
	r.inflight[id] = &task{
		kind: kind, key: key, modulePath: modulePath, wantWS: wantWS,
		seq: r.seq, ctx: ctx, cancel: cancel, job: job,
	}
	r.ready = append(r.ready, r.inflight[id])
	r.cond.Signal()
	return true
}

// Cancel stops the task of one kind with the given key. A queued task is
// dropped before it runs (emitting Canceled); a running task's terraform
// receives SIGINT and the task ends Canceled once it has stopped.
func (r *Runner) Cancel(kind Kind, key string) {
	r.cancel(func(t *task) bool { return t.kind == kind && t.key == key })
}

// CancelKey stops every task with the given key, whatever its kind: a
// workspace's plan and apply, or a module's enumerate and init.
func (r *Runner) CancelKey(key string) {
	r.cancel(func(t *task) bool { return t.key == key })
}

func (r *Runner) cancel(match func(*task) bool) {
	var dropped []*task
	r.mu.Lock()
	for id, t := range r.inflight {
		if !match(t) {
			continue
		}
		if t.started {
			t.cancel()
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

// Shutdown drops every queued task, interrupts every running one and waits
// up to timeout for them to stop. It returns how many were still running
// when it gave up. No events are emitted for the dropped queue: the caller
// is exiting.
func (r *Runner) Shutdown(timeout time.Duration) int {
	r.mu.Lock()
	for id, t := range r.inflight {
		if t.started {
			t.cancel()
			continue
		}
		r.removeReadyLocked(t)
		delete(r.inflight, id)
	}
	r.mu.Unlock()
	deadline := time.Now().Add(timeout)
	for {
		r.mu.Lock()
		n := r.active
		r.mu.Unlock()
		if n == 0 || time.Now().After(deadline) {
			return n
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ModuleBusy reports whether some tfmux process currently runs a task in the
// module: its cross-process lock is held. A false answer is a snapshot.
func (r *Runner) ModuleBusy(modulePath string) bool {
	path, err := r.store.ModuleLockPath(modulePath)
	if err != nil {
		return false
	}
	return flockHeld(path)
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
func (r *Runner) EnqueueEnumerate(m *domain.Module) bool {
	return r.enqueue(KindEnumerate, m.Path, m.Path, "", r.enumerateJob(m, false))
}

func (r *Runner) enumerateJob(m *domain.Module, afterInit bool) jobFunc {
	return func(ctx context.Context, _ func(Event)) Event {
		tf := tfFor(m)
		tf.Out = r.taskLog(m.Path, "enumerate")
		if c, ok := tf.Out.(io.Closer); ok {
			defer c.Close()
		}
		if !tf.Initialized() {
			if afterInit {
				return Event{Err: "workspace list: module not initialized (a preceding terraform init failed)"}
			}
			return r.deferForInit(m, "", r.enumerateJob(m, true))
		}
		res, workspaces, err := tf.WorkspaceList(ctx)
		if err != nil {
			return Event{Err: err.Error()}
		}
		if res.ExitCode != 0 {
			if !afterInit && tfexec.NeedsInit(res.Output) {
				return r.deferForInit(m, "", r.enumerateJob(m, true))
			}
			return Event{Err: fmt.Sprintf("workspace list in %s failed:\n%s", m.Path, res.Output)}
		}
		// cache the (slow, rate-limited) enumeration for next launch
		_ = r.store.SaveWorkspaces(m.Path, workspaces, time.Now())
		return Event{Workspaces: workspaces}
	}
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
func (r *Runner) EnqueueInitUpgrade(m *domain.Module) bool { return r.enqueueInit(m, true, "") }

// enqueueInit runs terraform init, either the explicit user-triggered upgrade
// or a plain lazy init a Plan/Output/Enumerate task deferred to. Both share
// TaskID(KindInit, m.Path): if a lazy init for the module is already queued
// the instant an explicit upgrade is requested, the upgrade request is
// dropped (the queued plain init wins) — a rare, harmless race; it never
// produces an unintended upgrade, and pressing `I` again after it completes
// fixes it.
//
// hint is the workspace the caller needs; a template module's init resolves
// its actual workspace only when it runs (see initWorkspace).
func (r *Runner) enqueueInit(m *domain.Module, upgrade bool, hint string) bool {
	return r.enqueue(KindInit, m.Path, m.Path, "", r.initJob(m, upgrade, hint))
}

func (r *Runner) initJob(m *domain.Module, upgrade bool, hint string) jobFunc {
	return func(ctx context.Context, _ func(Event)) Event {
		tf := tfFor(m)
		tf.Out = r.taskLog(m.Path, "init")
		if c, ok := tf.Out.(io.Closer); ok {
			defer c.Close()
		}
		ws := ""
		if m.Templated() {
			ws = r.initWorkspace(m, hint)
		}
		dependent := m.WorkspaceDependentInit()
		if dependent {
			if err := r.store.ClearInitWorkspace(m.Path); err != nil {
				return Event{Err: "clear init workspace: " + err.Error()}
			}
		}
		res, err := tf.Init(ctx, ws, upgrade)
		if err != nil {
			return Event{Err: err.Error()}
		}
		if res.ExitCode != 0 {
			return Event{Err: string(res.Output)}
		}
		if dependent {
			if err := r.store.SaveInitWorkspace(m.Path, ws); err != nil {
				return Event{Err: "record init workspace: " + err.Error()}
			}
		}
		return Event{}
	}
}

// initWorkspace picks the workspace a template module's init runs for: the
// one the earliest-queued task waiting in the module needs (the one the
// scheduler will dispatch right after the init), else the caller's hint, else
// the one the module dir is already initialised for, else the module's first
// manifest workspace.
func (r *Runner) initWorkspace(m *domain.Module, hint string) string {
	r.mu.Lock()
	var first *task
	for _, t := range r.ready {
		if t.modulePath == m.Path && t.wantWS != "" && (first == nil || t.seq < first.seq) {
			first = t
		}
	}
	r.mu.Unlock()
	if first != nil {
		return first.wantWS
	}
	if hint != "" {
		return hint
	}
	if ws, ok := r.store.LoadInitWorkspace(m.Path); ok {
		return ws
	}
	if len(m.ManifestWorkspaces) > 0 {
		return m.ManifestWorkspaces[0]
	}
	return ""
}

// initMismatch reports whether the module dir is known to be initialised for
// a workspace other than ws.
func (r *Runner) initMismatch(m *domain.Module, ws string) bool {
	if !m.WorkspaceDependentInit() {
		return false
	}
	cur, _ := r.store.LoadInitWorkspace(m.Path)
	return cur != ws
}

// deferForInit ends the current task body without a terminal event: execute()
// ensures a plain init is queued for m (a no-op if one is already in flight,
// including an explicit upgrade) and returns the task to the ready queue,
// where retry runs as its body next time. The task keeps its queue position,
// so a template module's init serves it rather than a workspace queued later.
//
// Ordering needs no extra plumbing: pickLocked won't dispatch the task again
// while an init for its module is queued or running.
func (r *Runner) deferForInit(m *domain.Module, wantWS string, retry jobFunc) Event {
	return Event{deferral: &deferral{module: m, wantWS: wantWS, retry: retry}}
}

func tfFor(m *domain.Module) tfexec.TF {
	return tfexec.TF{Bin: m.TFBin, Dir: m.Path, Templates: m.Templates}
}

// EnqueuePlan plans one workspace, persisting the RunRecord + plan file + log.
// Returns false if already in flight.
func (r *Runner) EnqueuePlan(w *domain.Workspace) bool {
	return r.enqueue(KindPlan, w.Key(), w.Module.Path, w.Name, r.planJob(w, false))
}

func (r *Runner) planJob(w *domain.Workspace, afterInit bool) jobFunc {
	m := w.Module
	tf := tfFor(m)
	return func(ctx context.Context, _ func(Event)) Event {
		if !tf.Initialized() {
			if afterInit {
				return Event{Err: "plan: module not initialized (a preceding terraform init failed)"}
			}
			return r.deferForInit(m, w.Name, r.planJob(w, true))
		}
		// A workspace mismatch re-defers even after an init: that init may
		// have served an earlier task's workspace, and the next one retires
		// this task's.
		if r.initMismatch(m, w.Name) {
			return r.deferForInit(m, w.Name, r.planJob(w, afterInit))
		}
		rec, needsInit, err := r.plan(ctx, tf, m, w.Name)
		if !afterInit && needsInit {
			return r.deferForInit(m, w.Name, r.planJob(w, true))
		}
		ev := Event{Record: rec}
		if err != nil {
			ev.Err = err.Error()
		}
		return ev
	}
}

// EnqueueOutput runs `terraform output` for one workspace, capturing it to a
// log the UI can show. Returns false if already in flight.
func (r *Runner) EnqueueOutput(w *domain.Workspace) bool {
	return r.enqueue(KindOutput, w.Key(), w.Module.Path, w.Name, r.outputJob(w, false))
}

func (r *Runner) outputJob(w *domain.Workspace, afterInit bool) jobFunc {
	m := w.Module
	return func(ctx context.Context, _ func(Event)) Event {
		tf := tfFor(m)
		if !tf.Initialized() {
			if afterInit {
				return Event{Err: "output: module not initialized (a preceding terraform init failed)"}
			}
			return r.deferForInit(m, w.Name, r.outputJob(w, true))
		}
		if r.initMismatch(m, w.Name) {
			return r.deferForInit(m, w.Name, r.outputJob(w, afterInit))
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
				return r.deferForInit(m, w.Name, r.outputJob(w, true))
			}
			return Event{Err: string(res.Output)}
		}
		return Event{}
	}
}

// EnqueueApply applies one workspace's saved plan, persisting its outcome in
// the workspace's RunRecord and its output in the apply log. plannedVersion
// guards against applying a plan made with a different terraform binary.
// Returns false if already in flight.
func (r *Runner) EnqueueApply(w *domain.Workspace, plannedVersion string) bool {
	return r.enqueue(KindApply, w.Key(), w.Module.Path, w.Name, r.applyJob(w, plannedVersion, false))
}

func (r *Runner) applyJob(w *domain.Workspace, plannedVersion string, afterInit bool) jobFunc {
	m := w.Module
	return func(ctx context.Context, _ func(Event)) Event {
		tf := tfFor(m)
		if !tf.Initialized() {
			if afterInit {
				return Event{Err: "apply: module not initialized (a preceding terraform init failed)"}
			}
			return r.deferForInit(m, w.Name, r.applyJob(w, plannedVersion, true))
		}
		if r.initMismatch(m, w.Name) {
			return r.deferForInit(m, w.Name, r.applyJob(w, plannedVersion, afterInit))
		}
		if plannedVersion != "" {
			if cur, err := tf.Version(ctx); err == nil && cur != plannedVersion {
				return Event{Err: fmt.Sprintf(
					"refusing to apply: plan made with %s %s, current is %s — re-plan",
					m.TFBin, plannedVersion, cur)}
			}
		}
		rec, err := r.apply(ctx, tf, m, w.Name)
		ev := Event{Record: rec}
		if err != nil {
			ev.Err = err.Error()
		}
		return ev
	}
}

// apply runs terraform apply on the saved plan. The run record is saved with
// the apply started before terraform runs, so a tfmux that dies mid-apply
// leaves evidence of it, and saved again with the outcome. A canceled apply
// is recorded as aborted: terraform stopped gracefully, but the plan may be
// partially applied. The record is re-read before the final save so a change
// made to it meanwhile (a downstream invalidation) survives.
func (r *Runner) apply(ctx context.Context, tf tfexec.TF, m *domain.Module, workspace string) (*state.RunRecord, error) {
	planFile, err := r.store.PlanFilePath(m.Path, workspace)
	if err != nil {
		return nil, err
	}
	rec, err := r.store.LoadRun(m.Path, workspace)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, fmt.Errorf("apply: no plan record for %s", workspace)
	}
	a := &state.ApplyRecord{Started: time.Now()}
	rec.Apply = a
	if err := r.store.SaveRun(rec); err != nil {
		return nil, err
	}

	logPath, logErr := r.store.ApplyLogPath(m.Path, workspace)
	if logErr == nil {
		if logFile, ferr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); ferr == nil {
			tf.Out = logFile
			defer logFile.Close()
		}
	}
	res, runErr := tf.Apply(ctx, workspace, planFile)
	now := time.Now()
	a.Finished = &now
	if tf.Out == nil && logErr == nil {
		_ = os.WriteFile(logPath, res.Output, 0o600) // fallback if streaming setup failed
	}

	if fresh, err := r.store.LoadRun(m.Path, workspace); err == nil && fresh != nil {
		rec = fresh
	}
	switch {
	case runErr != nil && ctx.Err() != nil:
		a.Aborted = true
		rec.Apply = a
	case runErr != nil:
		rec.Apply = nil // terraform never ran
	default:
		a.ExitCode = &res.ExitCode
		rec.Apply = a
		if res.ExitCode == 0 {
			_ = r.store.DiscardPlan(m.Path, workspace)
		}
	}
	if err := r.store.SaveRun(rec); err != nil && runErr == nil {
		return rec, err
	}
	return rec, runErr
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
	// Taken before the plan runs, so an edit made while it runs already
	// counts as a change against it.
	if fp, err := gitstatus.Fingerprint(ctx, m.Repo.Path, m.FingerprintPaths()); err == nil {
		rec.Fingerprint = fp
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
			rec.OutputsChanged = outputsChanged(plan)
		}
	}
	if res.ExitCode != tfexec.PlanError {
		if v, err := tf.Version(ctx); err == nil {
			rec.TFBinVersion = v
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
