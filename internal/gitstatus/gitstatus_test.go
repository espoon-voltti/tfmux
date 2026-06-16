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

func TestParseCleanWithUpstream(t *testing.T) {
	out := `# branch.oid 1234567890abcdef
# branch.head main
# branch.upstream origin/main
# branch.ab +2 -1
`
	st := Parse(out)
	if st.Branch != "main" || st.Dirty || st.Ahead != 2 || st.Behind != 1 || !st.HasUpstream {
		t.Errorf("unexpected: %+v", st)
	}
}

func TestParseDirtyDetached(t *testing.T) {
	out := `# branch.oid deadbeef
# branch.head (detached)
1 .M N... 100644 100644 100644 abc def main.tf
? untracked.tf
`
	st := Parse(out)
	if !st.Detached || st.Branch != "" || !st.Dirty {
		t.Errorf("unexpected: %+v", st)
	}
}

func TestParseInitialNoUpstream(t *testing.T) {
	out := `# branch.oid (initial)
# branch.head main
`
	st := Parse(out)
	if st.Branch != "main" || st.HasUpstream || st.Dirty || st.OID != "(initial)" {
		t.Errorf("unexpected: %+v", st)
	}
}

// TestCLIAgainstRealRepo builds a throwaway repo and reads its status.
func TestCLIAgainstRealRepo(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "-m", "init")

	st := CLI{}.Status(context.Background(), dir)
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	if st.Branch != "main" || st.Dirty {
		t.Errorf("clean repo: %+v", st)
	}

	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	st = CLI{}.Status(context.Background(), dir)
	if !st.Dirty {
		t.Errorf("expected dirty after untracked file: %+v", st)
	}
}

func TestCLINonRepo(t *testing.T) {
	st := CLI{}.Status(context.Background(), t.TempDir())
	if st.Err == nil {
		t.Error("expected error for non-repo")
	}
}
