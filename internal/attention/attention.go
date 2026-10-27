// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

// Package attention decides what about a workspace needs the operator's
// attention, from its persisted run record and the module's current git
// fingerprint. The TUI's status cell and `tfmux pending` both read this so
// they never disagree.
package attention

import (
	"fmt"
	"path"

	"github.com/espoon-voltti/tfmux/internal/deps"
	"github.com/espoon-voltti/tfmux/internal/domain"
	"github.com/espoon-voltti/tfmux/internal/state"
	"github.com/espoon-voltti/tfmux/internal/tfexec"
)

// Code names one way a workspace needs attention.
type Code string

const (
	Unplanned    Code = "unplanned"    // never planned on this machine
	Changed      Code = "changed"      // module content changed since the last plan
	Unknown      Code = "unknown"      // the record predates fingerprints; comparability unknown
	PlanError    Code = "plan-error"   // the last plan failed
	Ready        Code = "ready"        // a plan with changes is saved and current
	Expired      Code = "plan-expired" // the last plan had changes but its file is gone
	ApplyFailed  Code = "apply-failed" // the last apply exited non-zero
	ApplyAborted Code = "apply-aborted"
	// UpstreamApplied: a remote state this plan read was applied after the
	// plan was taken, so the plan carries stale values.
	UpstreamApplied Code = "upstream-applied"
)

// Reason is one Code with a human detail, e.g. the change summary for Ready.
type Reason struct {
	Code   Code   `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// Assess returns why a workspace needs attention, most urgent first, or nil
// when it is planned clean (or applied) and nothing changed since.
// fingerprint is the module's current fingerprint; "" means it could not be
// computed, which never yields Changed or Unknown.
func Assess(rec *state.RunRecord, planFile bool, fingerprint string) []Reason {
	if rec == nil {
		return []Reason{{Code: Unplanned}}
	}
	var rs []Reason
	applied := false
	if a := rec.Apply; a != nil {
		switch {
		case a.Aborted:
			rs = append(rs, Reason{Code: ApplyAborted, Detail: "apply interrupted before finishing"})
		case a.ExitCode != nil && *a.ExitCode != 0:
			rs = append(rs, Reason{Code: ApplyFailed, Detail: fmt.Sprintf("exit %d", *a.ExitCode)})
		case a.ExitCode != nil:
			applied = true
		}
	}
	if rec.PlanExitCode == tfexec.PlanError {
		rs = append(rs, Reason{Code: PlanError, Detail: rec.PlanErrorKind})
	}
	if rec.Invalidated != nil {
		rs = append(rs, Reason{Code: UpstreamApplied, Detail: "upstream " + rec.Invalidated.By + " applied since plan"})
	}
	if fingerprint != "" {
		switch {
		case rec.Fingerprint == "":
			rs = append(rs, Reason{Code: Unknown, Detail: "planned by an older tfmux"})
		case rec.Fingerprint != fingerprint:
			rs = append(rs, Reason{Code: Changed, Detail: "module changed since plan"})
		}
	}
	if rec.PlanExitCode == tfexec.PlanChanges && !applied {
		switch {
		case !planFile:
			rs = append(rs, Reason{Code: Expired, Detail: "plan file expired or discarded"})
		case !Stale(rs):
			rs = append(rs, Reason{Code: Ready, Detail: rec.Summary.String()})
		}
	}
	return rs
}

// Has reports whether rs contains code.
func Has(rs []Reason, code Code) bool {
	for _, r := range rs {
		if r.Code == code {
			return true
		}
	}
	return false
}

// NeedsPlan reports whether the right next step is a new plan, as opposed to
// an apply or a look at an error.
func NeedsPlan(rs []Reason) bool {
	for _, r := range rs {
		switch r.Code {
		case Unplanned, Changed, Unknown, Expired, ApplyAborted, UpstreamApplied:
			return true
		}
	}
	return false
}

// Stale reports whether a saved plan must not be applied: the module content
// it was taken against is no longer what is on disk, or an upstream state it
// read has changed.
func Stale(rs []Reason) bool { return Has(rs, Changed) || Has(rs, Unknown) || Has(rs, UpstreamApplied) }

// ModuleBehind is a module-level reason: a git module the root pins has
// newer content in the shared repo's checkout.
const ModuleBehind Code = "module-behind"

// Module returns module-level reasons, one ModuleBehind per pinned git ref
// that is behind. behind is keyed by deps.RefKey; pins absent from it are
// unknown and yield nothing.
func Module(d *domain.Deps, behind map[string]deps.Behind) []Reason {
	if d == nil {
		return nil
	}
	var rs []Reason
	for _, g := range d.Git {
		b, ok := behind[deps.RefKey(g)]
		if !ok || !b.Any() {
			continue
		}
		rs = append(rs, Reason{Code: ModuleBehind, Detail: fmt.Sprintf("%s %s (via %s)", path.Base(g.Subdir), b, g.Via)})
	}
	return rs
}

// ApplyBlocker reports why a workspace's saved plan can't be applied right
// now, or "" when it can: a plan with outstanding changes, its file on disk,
// nothing in flight for the workspace, and not stale.
func ApplyBlocker(rec *state.RunRecord, planFile, planRunning, applyRunning bool, fingerprint string) string {
	switch {
	case planRunning:
		return "plan still running"
	case rec == nil || rec.PlanExitCode != tfexec.PlanChanges:
		return "nothing to apply — run a plan with changes first"
	case !planFile:
		return "plan file expired or discarded — re-plan first"
	case applyRunning:
		return "apply already queued/running — press enter to attach"
	case Stale(Assess(rec, planFile, fingerprint)):
		return "plan is STALE (module or upstream state changed since plan) — re-plan, or attach and apply manually"
	}
	return ""
}
