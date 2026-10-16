// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

// Package gittest builds throwaway git repositories for tests, with a fixed
// identity and no user or system config so results don't depend on the
// machine running them.
package gittest

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Init creates an empty repository on branch main under a fresh temp dir and
// returns its path. The test is skipped when git is not installed.
func Init(t testing.TB) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	Run(t, dir, "init", "-q", "-b", "main")
	return dir
}

// Run runs git with args in dir, failing the test on a non-zero exit.
func Run(t testing.TB, dir string, args ...string) {
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

// WriteFile writes content to path, creating parent directories.
func WriteFile(t testing.TB, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// CommitAll stages everything and commits it.
func CommitAll(t testing.TB, dir, message string) {
	t.Helper()
	Run(t, dir, "add", ".")
	Run(t, dir, "commit", "-q", "-m", message)
}
