// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package attention

import (
	"reflect"
	"testing"

	"github.com/espoon-voltti/tfmux/internal/deps"
	"github.com/espoon-voltti/tfmux/internal/domain"
	"github.com/espoon-voltti/tfmux/internal/state"
	"github.com/espoon-voltti/tfmux/internal/tfexec"
)

func codes(rs []Reason) []Code {
	var cs []Code
	for _, r := range rs {
		cs = append(cs, r.Code)
	}
	return cs
}

func intp(i int) *int { return &i }

func TestAssess(t *testing.T) {
	const fp = "tree|dirty"
	cases := []struct {
		name     string
		rec      *state.RunRecord
		planFile bool
		current  string
		want     []Code
		detail   string // of the first reason, when checked
		plan     bool
	}{
		{"never planned", nil, false, fp, []Code{Unplanned}, "", true},
		{"clean and current", &state.RunRecord{PlanExitCode: tfexec.PlanClean, Fingerprint: fp}, false, fp, nil, "", false},
		{"clean but changed", &state.RunRecord{PlanExitCode: tfexec.PlanClean, Fingerprint: fp}, false, "other|x", []Code{Changed}, "", true},
		{"clean, fingerprint unavailable", &state.RunRecord{PlanExitCode: tfexec.PlanClean, Fingerprint: fp}, false, "", nil, "", false},
		{"legacy record", &state.RunRecord{PlanExitCode: tfexec.PlanClean}, false, fp, []Code{Unknown}, "", true},
		{"plan error", &state.RunRecord{PlanExitCode: tfexec.PlanError, PlanErrorKind: "state locked", Fingerprint: fp}, false, fp, []Code{PlanError}, "state locked", false},
		{"ready", &state.RunRecord{PlanExitCode: tfexec.PlanChanges, Fingerprint: fp, Summary: state.ChangeSummary{Add: 2, Change: 1}}, true, fp, []Code{Ready}, "+2 ~1 -0", false},
		{"stale plan", &state.RunRecord{PlanExitCode: tfexec.PlanChanges, Fingerprint: fp}, true, "other|x", []Code{Changed}, "", true},
		{"expired", &state.RunRecord{PlanExitCode: tfexec.PlanChanges, Fingerprint: fp}, false, fp, []Code{Expired}, "", true},
		{"applied", &state.RunRecord{PlanExitCode: tfexec.PlanChanges, Fingerprint: fp, Apply: &state.ApplyRecord{ExitCode: intp(0)}}, false, fp, nil, "", false},
		{"applied then changed", &state.RunRecord{PlanExitCode: tfexec.PlanChanges, Fingerprint: fp, Apply: &state.ApplyRecord{ExitCode: intp(0)}}, false, "other|x", []Code{Changed}, "", true},
		{"apply failed, plan kept", &state.RunRecord{PlanExitCode: tfexec.PlanChanges, Fingerprint: fp, Apply: &state.ApplyRecord{ExitCode: intp(1)}}, true, fp, []Code{ApplyFailed, Ready}, "exit 1", false},
		{"clean, upstream applied", &state.RunRecord{PlanExitCode: tfexec.PlanClean, Fingerprint: fp, Invalidated: &state.Invalidation{By: "x/base//prod"}}, false, fp, []Code{UpstreamApplied}, "upstream x/base//prod applied since plan", true},
		{"ready but upstream applied", &state.RunRecord{PlanExitCode: tfexec.PlanChanges, Fingerprint: fp, Invalidated: &state.Invalidation{By: "x/base//prod"}}, true, fp, []Code{UpstreamApplied}, "", true},
		{"apply aborted", &state.RunRecord{PlanExitCode: tfexec.PlanChanges, Fingerprint: fp, Apply: &state.ApplyRecord{Aborted: true}}, false, fp, []Code{ApplyAborted, Expired}, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Assess(c.rec, c.planFile, c.current)
			if !reflect.DeepEqual(codes(got), c.want) {
				t.Errorf("codes = %v, want %v (%+v)", codes(got), c.want, got)
			}
			if c.detail != "" && (len(got) == 0 || got[0].Detail != c.detail) {
				t.Errorf("detail = %+v, want %q", got, c.detail)
			}
			if NeedsPlan(got) != c.plan {
				t.Errorf("NeedsPlan = %v, want %v", NeedsPlan(got), c.plan)
			}
		})
	}
}

func TestModuleBehind(t *testing.T) {
	d := &domain.Deps{Git: []domain.GitRef{
		{Repo: "github.com/org/shared", Subdir: "modules/waf", Ref: "v16", Via: "waf"},
		{Repo: "github.com/org/shared", Subdir: "modules/s3", Ref: "v1", Via: "bucket"},
		{Repo: "github.com/org/other", Subdir: "x", Ref: "v1", Via: "x"},
	}}
	behind := map[string]deps.Behind{
		"github.com/org/shared//modules/waf@v16": {Ref: "v16", NewerTag: "v18"},
		"github.com/org/shared//modules/s3@v1":   {Ref: "v1"},
	}
	got := Module(d, behind)
	if len(got) != 1 || got[0].Code != ModuleBehind || got[0].Detail != "waf v16→v18 (via waf)" {
		t.Errorf("got %+v", got)
	}
	if Module(nil, behind) != nil {
		t.Error("nil deps should yield nothing")
	}
}
