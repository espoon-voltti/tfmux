// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package report

import (
	"time"

	"github.com/espoon-voltti/tfmux/internal/deps"
	"github.com/espoon-voltti/tfmux/internal/domain"
	"github.com/espoon-voltti/tfmux/internal/state"
)

// Downstream lists the workspaces whose plans read values that applying
// upstream (a plan of up) changed: every workspace of a root reading up's
// state, in the workspace the applied one maps to, when the plan changed
// outputs. One hop only: nothing cascades, so roots reading each other's
// state cannot loop. Workspaces come from the index's modules' Workspaces
// lists.
func Downstream(ix *deps.Index, up *domain.Module, upstream *state.RunRecord) []*domain.Workspace {
	if !upstream.OutputsChanged || up.Deps == nil || up.Deps.Backend == nil {
		return nil
	}
	var out []*domain.Workspace
	for _, down := range ix.DependentsOfState(up) {
		if down == up {
			continue
		}
		for _, ws := range down.Workspaces {
			if deps.ReadsWorkspace(down, up, ws.Name, upstream.Workspace) {
				out = append(out, ws)
			}
		}
	}
	return out
}

// InvalidationBy labels the applied workspace in a record's Invalidated.
func InvalidationBy(up *domain.Module, workspace string) string {
	return ModuleLabel(up) + "//" + workspace
}

// InvalidateDownstream marks the persisted records of every Downstream
// workspace that has one and is not already marked. Returns the marked
// workspace keys.
func InvalidateDownstream(store *state.Store, ix *deps.Index, up *domain.Module, upstream *state.RunRecord, now time.Time) []string {
	by := InvalidationBy(up, upstream.Workspace)
	var marked []string
	for _, ws := range Downstream(ix, up, upstream) {
		rec, err := store.LoadRun(ws.Module.Path, ws.Name)
		if err != nil || rec == nil || rec.Invalidated != nil {
			continue
		}
		rec.Invalidated = &state.Invalidation{By: by, At: now}
		if store.SaveRun(rec) == nil {
			marked = append(marked, ws.Key())
		}
	}
	return marked
}
