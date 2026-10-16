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

// InvalidateDownstream marks the persisted records of every workspace that
// reads up's state, in the workspace the applied one maps to, after an apply
// of upstream changed outputs. One hop only: marking is not an apply, so
// nothing cascades and roots reading each other's state cannot loop. Returns
// the marked workspace keys. Workspaces come from the index's modules'
// Workspaces lists.
func InvalidateDownstream(store *state.Store, ix *deps.Index, up *domain.Module, upstream *state.RunRecord, now time.Time) []string {
	if !upstream.OutputsChanged || up.Deps == nil || up.Deps.Backend == nil {
		return nil
	}
	by := ModuleLabel(up) + "//" + upstream.Workspace
	var marked []string
	for _, down := range ix.DependentsOfState(up) {
		if down == up {
			continue
		}
		for _, ws := range down.Workspaces {
			if !deps.ReadsWorkspace(down, up, ws.Name, upstream.Workspace) {
				continue
			}
			rec, err := store.LoadRun(down.Path, ws.Name)
			if err != nil || rec == nil || rec.Invalidated != nil {
				continue
			}
			rec.Invalidated = &state.Invalidation{By: by, At: now}
			if store.SaveRun(rec) == nil {
				marked = append(marked, ws.Key())
			}
		}
	}
	return marked
}
