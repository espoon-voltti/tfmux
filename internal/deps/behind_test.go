// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package deps

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/espoon-voltti/tfmux/internal/domain"
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
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	write(t, filepath.Join(dir, "modules/waf/main.tf"), "a")
	write(t, filepath.Join(dir, "modules/other/main.tf"), "a")
	run("add", ".")
	run("commit", "-q", "-m", "v1")
	run("tag", "v1")
	write(t, filepath.Join(dir, "modules/waf/main.tf"), "b")
	run("commit", "-q", "-am", "v2")
	run("tag", "v2")
	write(t, filepath.Join(dir, "modules/other/main.tf"), "b")
	run("commit", "-q", "-am", "unreleased")
	write(t, filepath.Join(dir, "modules/waf/main.tf"), "c")
	return dir
}

func TestCheckRefs(t *testing.T) {
	dir := sharedRepo(t)
	ref := func(subdir, r string) domain.GitRef {
		return domain.GitRef{Repo: "github.com/org/shared", Subdir: subdir, Ref: r}
	}
	refs := []domain.GitRef{ref("modules/waf", "v1"), ref("modules/waf", "v2"), ref("modules/other", "v2"), ref("modules/other", "v1"), ref("modules/waf", "v9"), ref("modules/waf", "")}
	got := CheckRefs(context.Background(), dir, refs)
	want := map[string]Behind{
		"github.com/org/shared//modules/waf@v1":   {Ref: "v1", NewerTag: "v2", Dirty: true},
		"github.com/org/shared//modules/waf@v2":   {Ref: "v2", Dirty: true},
		"github.com/org/shared//modules/other@v2": {Ref: "v2", Unreleased: true},
		"github.com/org/shared//modules/other@v1": {Ref: "v1", Unreleased: true},
		"github.com/org/shared//modules/waf@v9":   {Ref: "v9", RefMissing: true, Dirty: true},
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
