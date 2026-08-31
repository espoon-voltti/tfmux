// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package runner

import "testing"

// A runner built without tmux (no tmux on the machine, or any test fixture)
// still gets re-adoption attempts for every apply record left unfinished, so
// EnqueueApplyPoll must report the apply as over instead of dereferencing the
// tmux it doesn't have.
func TestEnqueueApplyPollWithoutTmux(t *testing.T) {
	f := newFixture(t, 1)
	m := f.newModule(t, "mod1")

	if !f.runner.EnqueueApplyPoll(m.Path, "prod", m.Path+"//prod@1") {
		t.Fatal("apply poll enqueue refused")
	}

	ev := waitFor(t, f.runner.Events, func(e Event) bool {
		return e.Kind == KindApply && e.Phase.Terminal()
	})
	if ev.Phase != PhaseDone || !ev.Aborted {
		t.Errorf("phase = %v, aborted = %v, err = %q; want a done+aborted apply",
			ev.Phase, ev.Aborted, ev.Err)
	}
}
