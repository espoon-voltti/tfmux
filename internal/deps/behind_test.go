// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package deps

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/espoon-voltti/tfmux/internal/domain"
	"github.com/espoon-voltti/tfmux/internal/gittest"
)

func TestNormalizeRepoURL(t *testing.T) {
	for _, u := range []string{
		"https://github.com/espoon-voltti/evaka-infra-shared",
		"https://github.com/espoon-voltti/evaka-infra-shared.git",
		"git@github.com:espoon-voltti/evaka-infra-shared.git",
		"ssh://git@github.com/espoon-voltti/evaka-infra-shared.git",
		"git::https://github.com/espoon-voltti/evaka-infra-shared.git/",
	} {
		if got := NormalizeRepoURL(u); got != "github.com/espoon-voltti/evaka-infra-shared" {
			t.Errorf("%s → %q", u, got)
		}
	}
}

// sharedRepo builds a checkout with modules/waf and modules/other: v1 tags
// the initial state, v2 tags a waf change, then other changes unreleased and
// waf gets a dirty edit.
func sharedRepo(t *testing.T) string {
	t.Helper()
	dir := gittest.Init(t)
	gittest.WriteFile(t, filepath.Join(dir, "modules/waf/main.tf"), "a")
	gittest.WriteFile(t, filepath.Join(dir, "modules/other/main.tf"), "a")
	gittest.CommitAll(t, dir, "v1")
	gittest.Run(t, dir, "tag", "v1")
	gittest.WriteFile(t, filepath.Join(dir, "modules/waf/main.tf"), "b")
	gittest.Run(t, dir, "commit", "-q", "-am", "v2")
	gittest.Run(t, dir, "tag", "v2")
	gittest.WriteFile(t, filepath.Join(dir, "modules/other/main.tf"), "b")
	gittest.Run(t, dir, "commit", "-q", "-am", "unreleased")
	gittest.WriteFile(t, filepath.Join(dir, "modules/waf/main.tf"), "c")
	return dir
}

func TestCheckRefs(t *testing.T) {
	dir := sharedRepo(t)
	ref := func(subdir, r string) domain.GitRef {
		return domain.GitRef{Repo: "github.com/org/shared", Subdir: subdir, Ref: r}
	}
	refs := []domain.GitRef{
		ref("modules/waf", "v1"), ref("modules/waf", "v2"), ref("modules/other", "v2"), ref("modules/other", "v1"),
		ref("modules/waf", "v9"), ref("modules/waf", ""),
		// A branch pin already contains every tag: never behind one, and
		// never "unreleased" against itself.
		ref("modules/waf", "main"), ref("modules/other", "main"),
		// A module that is the whole repo.
		ref("", "v1"),
	}
	got := CheckRefs(context.Background(), dir, refs)
	want := map[string]Behind{
		"github.com/org/shared//modules/waf@v1":     {Ref: "v1", NewerTag: "v2", Dirty: true},
		"github.com/org/shared//modules/waf@v2":     {Ref: "v2", Dirty: true},
		"github.com/org/shared//modules/other@v2":   {Ref: "v2", Unreleased: true},
		"github.com/org/shared//modules/other@v1":   {Ref: "v1", Unreleased: true},
		"github.com/org/shared//modules/waf@v9":     {Ref: "v9", RefMissing: true, Dirty: true},
		"github.com/org/shared//modules/waf@main":   {Ref: "main", Dirty: true},
		"github.com/org/shared//modules/other@main": {Ref: "main"},
		"github.com/org/shared@v1":                  {Ref: "v1", NewerTag: "v2", Unreleased: true, Dirty: true},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d results: %+v", len(got), got)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: got %+v, want %+v", k, got[k], w)
		}
	}
	if s := got["github.com/org/shared//modules/waf@v1"].String(); s != "v1→v2, uncommitted" {
		t.Errorf("String = %q", s)
	}
}
