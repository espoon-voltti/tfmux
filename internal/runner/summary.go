// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package runner

import (
	tfjson "github.com/hashicorp/terraform-json"

	"github.com/espoon-voltti/tfmux/internal/state"
)

// summarize counts resource changes the way terraform's own
// "Plan: X to add, Y to change, Z to destroy" line does: a replace counts as
// one add and one destroy.
func summarize(plan *tfjson.Plan) state.ChangeSummary {
	var s state.ChangeSummary
	for _, rc := range plan.ResourceChanges {
		if rc.Change == nil {
			continue
		}
		a := rc.Change.Actions
		switch {
		case a.Replace():
			s.Add++
			s.Destroy++
		case a.Create():
			s.Add++
		case a.Delete():
			s.Destroy++
		case a.Update():
			s.Change++
		}
	}
	return s
}
