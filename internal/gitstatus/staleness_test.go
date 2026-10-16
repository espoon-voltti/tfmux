// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package gitstatus

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// gitRepo creates a throwaway repo and returns its path with a helper that
// runs git in it.
func gitRepo(t *testing.T) (string, func(args ...string)) {
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
	run("init", "-b", "main")
	return dir, run
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFingerprintTracksOnlyGivenPaths(t *testing.T) {
	dir, run := gitRepo(t)
	writeFile(t, filepath.Join(dir, "envs/prod/main.tf"), "a")
	writeFile(t, filepath.Join(dir, "envs/staging/main.tf"), "a")
	writeFile(t, filepath.Join(dir, "modules/x/main.tf"), "a")
	run("add", ".")
	run("commit", "-m", "init")
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
	writeFile(t, filepath.Join(dir, "envs/staging/main.tf"), "b")
	run("commit", "-am", "staging")
	if fp("envs/prod") != base {
		t.Error("unrelated commit changed the fingerprint")
	}

	// An uncommitted edit in the module changes it; committing it keeps it
	// changed (the tree differs from the original).
	writeFile(t, filepath.Join(dir, "envs/prod/main.tf"), "b")
	dirty := fp("envs/prod")
	if dirty == base {
		t.Error("dirty edit did not change the fingerprint")
	}
	run("commit", "-am", "prod")
	committed := fp("envs/prod")
	if committed == base || committed == dirty {
		t.Error("committed edit should differ from both the original and the dirty state")
	}

	// An untracked file inside the module counts.
	writeFile(t, filepath.Join(dir, "envs/prod/new.tf"), "c")
	if fp("envs/prod") == committed {
		t.Error("untracked file did not change the fingerprint")
	}

	// A dependency path is part of the fingerprint when listed.
	withDep := fp("envs/prod", "modules/x")
	writeFile(t, filepath.Join(dir, "modules/x/main.tf"), "b")
	if fp("envs/prod", "modules/x") == withDep {
		t.Error("edit in a listed dependency path did not change the fingerprint")
	}
}

func TestFingerprintUnbornBranchAndMissingPath(t *testing.T) {
	dir, _ := gitRepo(t)
	writeFile(t, filepath.Join(dir, "envs/prod/main.tf"), "a")
	s, err := Fingerprint(context.Background(), dir, []string{"envs/prod", "nope"})
	if err != nil {
		t.Fatal(err)
	}
	if s == "" {
		t.Error("expected a fingerprint even before the first commit")
	}
}

func TestRefsPlumbing(t *testing.T) {
	dir, run := gitRepo(t)
	writeFile(t, filepath.Join(dir, "modules/waf/main.tf"), "a")
	run("add", ".")
	run("commit", "-m", "v1")
	run("tag", "v1")
	writeFile(t, filepath.Join(dir, "modules/waf/main.tf"), "b")
	run("commit", "-am", "v2")
	run("tag", "v2")
	run("tag", "v10")
	run("remote", "add", "origin", "https://github.com/org/shared")
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
	if dirty, _ := HasChanges(ctx, dir, []string{"modules/waf"}); dirty {
		t.Error("clean checkout reported dirty")
	}
	writeFile(t, filepath.Join(dir, "modules/waf/main.tf"), "c")
	if dirty, _ := HasChanges(ctx, dir, []string{"modules/waf"}); !dirty {
		t.Error("edit not reported")
	}
	if url := RemoteURL(ctx, dir); url != "https://github.com/org/shared" {
		t.Errorf("remote = %q", url)
	}
	if st := (CLI{}).Status(ctx, dir); st.RemoteURL != "https://github.com/org/shared" {
		t.Errorf("Status.RemoteURL = %q", st.RemoteURL)
	}
}
