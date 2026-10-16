// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/espoon-voltti/tfmux/internal/discovery"
	"github.com/espoon-voltti/tfmux/internal/gitstatus"
	"github.com/espoon-voltti/tfmux/internal/state"
	"github.com/espoon-voltti/tfmux/internal/tfexec"
)

const backendTF = `terraform {
  backend "s3" {
    bucket = "b"
    key    = "k"
    region = "eu-west-1"
  }
}
`

// gitRepoWithModule creates a committed repo under root containing one root
// module at rel and returns the repo path.
func gitRepoWithModule(t *testing.T, root, name, rel string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(repo, rel), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, rel, "main.tf"), []byte(backendTF), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"commit", "-q", "-m", "init"}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return repo
}

func TestPendingReport(t *testing.T) {
	root := t.TempDir()
	repo := gitRepoWithModule(t, root, "repo1", "envs/prod")
	modPath := filepath.Join(repo, "envs/prod")
	store := state.New(t.TempDir())
	if err := store.SaveWorkspaces(modPath, []string{"current", "changed", "ready", "fresh", "hidden"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	fp, err := gitstatus.Fingerprint(context.Background(), repo, []string{"envs/prod"})
	if err != nil {
		t.Fatal(err)
	}
	save := func(ws string, exit int, fingerprint string) {
		t.Helper()
		if err := store.SaveRun(&state.RunRecord{ModulePath: modPath, Workspace: ws, PlanExitCode: exit, Fingerprint: fingerprint, PlanFinished: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	save("current", tfexec.PlanClean, fp)
	save("changed", tfexec.PlanClean, "stale|stale")
	save("ready", tfexec.PlanChanges, fp)
	planFile, _ := store.PlanFilePath(modPath, "ready")
	if err := os.WriteFile(planFile, []byte("plan"), 0o600); err != nil {
		t.Fatal(err)
	}
	repos, err := discovery.Discover([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	ignore := state.Ignore{modPath + "//hidden": true}

	rows, notes := pendingReport(store, ignore, repos, false)
	if len(notes) != 0 {
		t.Errorf("unexpected notes: %v", notes)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Workspace] = string(r.Reasons[0].Code)
	}
	want := map[string]string{"changed": "changed", "ready": "ready", "fresh": "unplanned"}
	if len(got) != len(want) {
		t.Fatalf("rows = %+v, want %v", rows, want)
	}
	for ws, code := range want {
		if got[ws] != code {
			t.Errorf("%s: code %q, want %q", ws, got[ws], code)
		}
	}

	rows, _ = pendingReport(store, ignore, repos, true)
	for _, r := range rows {
		if r.Workspace == "ready" {
			t.Error("--needs-plan must drop a plan that is ready to apply")
		}
	}

	var out strings.Builder
	printPending(&out, rows, time.Now())
	text := out.String()
	if !strings.Contains(text, "repo1") || !strings.Contains(text, "envs/prod") || !strings.Contains(text, "module changed since plan") {
		t.Errorf("text output:\n%s", text)
	}
}

func TestPendingReportNotesUnknownWorkspaces(t *testing.T) {
	root := t.TempDir()
	gitRepoWithModule(t, root, "repo1", "envs/prod")
	repos, err := discovery.Discover([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	rows, notes := pendingReport(state.New(t.TempDir()), state.Ignore{}, repos, false)
	if len(rows) != 0 || len(notes) != 1 || !strings.Contains(notes[0], "workspaces unknown") {
		t.Errorf("rows=%v notes=%v", rows, notes)
	}
}
