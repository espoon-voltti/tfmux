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
