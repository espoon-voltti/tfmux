// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package gitstatus

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/espoon-voltti/tfmux/internal/gittest"
)

func TestFingerprintTracksOnlyGivenPaths(t *testing.T) {
	dir := gittest.Init(t)
	gittest.WriteFile(t, filepath.Join(dir, "envs/prod/main.tf"), "a")
	gittest.WriteFile(t, filepath.Join(dir, "envs/staging/main.tf"), "a")
	gittest.WriteFile(t, filepath.Join(dir, "modules/x/main.tf"), "a")
	gittest.CommitAll(t, dir, "init")
	ctx := context.Background()
	fp := func(paths ...string) string {
		t.Helper()
		s, err := Fingerprint(ctx, dir, paths)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	base := fp("envs/prod")
	if fp("envs/prod") != base {
		t.Fatal("fingerprint must be deterministic")
	}

	// A commit elsewhere in the repo leaves the module's fingerprint alone.
	gittest.WriteFile(t, filepath.Join(dir, "envs/staging/main.tf"), "b")
	gittest.Run(t, dir, "commit", "-q", "-am", "staging")
	if fp("envs/prod") != base {
		t.Error("unrelated commit changed the fingerprint")
	}

	// An uncommitted edit in the module changes it; committing it keeps it
	// changed (the tree differs from the original).
	gittest.WriteFile(t, filepath.Join(dir, "envs/prod/main.tf"), "b")
	dirty := fp("envs/prod")
	if dirty == base {
		t.Error("dirty edit did not change the fingerprint")
	}
	gittest.Run(t, dir, "commit", "-q", "-am", "prod")
	committed := fp("envs/prod")
	if committed == base || committed == dirty {
		t.Error("committed edit should differ from both the original and the dirty state")
	}

	// An untracked file inside the module counts.
	gittest.WriteFile(t, filepath.Join(dir, "envs/prod/new.tf"), "c")
	if fp("envs/prod") == committed {
		t.Error("untracked file did not change the fingerprint")
	}

	// A dependency path is part of the fingerprint when listed.
	withDep := fp("envs/prod", "modules/x")
	gittest.WriteFile(t, filepath.Join(dir, "modules/x/main.tf"), "b")
	if fp("envs/prod", "modules/x") == withDep {
		t.Error("edit in a listed dependency path did not change the fingerprint")
	}
}

func TestFingerprintUnbornBranchAndMissingPath(t *testing.T) {
	dir := gittest.Init(t)
	gittest.WriteFile(t, filepath.Join(dir, "envs/prod/main.tf"), "a")
	s, err := Fingerprint(context.Background(), dir, []string{"envs/prod", "nope"})
	if err != nil {
		t.Fatal(err)
	}
	if s == "" {
		t.Error("expected a fingerprint even before the first commit")
	}
}

func TestRefsPlumbing(t *testing.T) {
	dir := gittest.Init(t)
	gittest.WriteFile(t, filepath.Join(dir, "modules/waf/main.tf"), "a")
	gittest.CommitAll(t, dir, "v1")
	gittest.Run(t, dir, "tag", "v1")
	gittest.WriteFile(t, filepath.Join(dir, "modules/waf/main.tf"), "b")
	gittest.Run(t, dir, "commit", "-q", "-am", "v2")
	gittest.Run(t, dir, "tag", "v2")
	gittest.Run(t, dir, "tag", "v10")
	gittest.Run(t, dir, "remote", "add", "origin", "https://github.com/org/shared")
	ctx := context.Background()

	v1, found, err := TreeID(ctx, dir, "v1", "modules/waf")
	if err != nil || !found || v1 == "" {
		t.Fatalf("v1: %q %v %v", v1, found, err)
	}
	v2, _, _ := TreeID(ctx, dir, "v2", "modules/waf")
	if v1 == v2 {
		t.Error("trees should differ between v1 and v2")
	}
	if _, found, err := TreeID(ctx, dir, "v9", "modules/waf"); err != nil || found {
		t.Errorf("missing ref: found=%v err=%v", found, err)
	}
	if _, found, _ := TreeID(ctx, dir, "v1", "modules/nope"); found {
		t.Error("missing path should not be found")
	}
	if tag, err := NewestTag(ctx, dir); err != nil || tag != "v10" {
		t.Errorf("newest tag = %q, %v (version sort expected)", tag, err)
	}
	for _, c := range []struct {
		ancestor, descendant string
		want                 bool
	}{{"v1", "main", true}, {"v2", "v1", false}, {"v2", "v2", true}, {"nope", "main", false}} {
		if got, err := IsAncestor(ctx, dir, c.ancestor, c.descendant); err != nil || got != c.want {
			t.Errorf("IsAncestor(%s, %s) = %v, %v; want %v", c.ancestor, c.descendant, got, err, c.want)
		}
	}
	if dirty, _ := HasChanges(ctx, dir, []string{"modules/waf"}); dirty {
		t.Error("clean checkout reported dirty")
	}
	gittest.WriteFile(t, filepath.Join(dir, "modules/waf/main.tf"), "c")
	if dirty, _ := HasChanges(ctx, dir, []string{"modules/waf"}); !dirty {
		t.Error("edit not reported")
	}
	if dirty, err := HasChanges(ctx, dir, []string{""}); err != nil || !dirty {
		t.Errorf("empty path should mean the whole tree: dirty=%v err=%v", dirty, err)
	}
	if url := RemoteURL(ctx, dir); url != "https://github.com/org/shared" {
		t.Errorf("remote = %q", url)
	}
	if st := (CLI{}).Status(ctx, dir); st.RemoteURL != "https://github.com/org/shared" {
		t.Errorf("Status.RemoteURL = %q", st.RemoteURL)
	}
}
