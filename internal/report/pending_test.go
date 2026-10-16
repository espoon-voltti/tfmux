// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package report

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/espoon-voltti/tfmux/internal/attention"
	"github.com/espoon-voltti/tfmux/internal/config"
	"github.com/espoon-voltti/tfmux/internal/discovery"
	"github.com/espoon-voltti/tfmux/internal/gitstatus"
	"github.com/espoon-voltti/tfmux/internal/gittest"
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
	repo := filepath.Join(root, name)
	gittest.WriteFile(t, filepath.Join(repo, rel, "main.tf"), backendTF)
	gittest.Run(t, repo, "init", "-q", "-b", "main")
	gittest.CommitAll(t, repo, "init")
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

	rows, notes := Pending(store, ignore, repos, false)
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

	rows, _ = Pending(store, ignore, repos, true)
	for _, r := range rows {
		if r.Workspace == "ready" {
			t.Error("--needs-plan must drop a plan that is ready to apply")
		}
	}

	var out strings.Builder
	PrintPending(&out, rows, time.Now())
	text := out.String()
	if !strings.Contains(text, "repo1") || !strings.Contains(text, "envs/prod") || !strings.Contains(text, "module changed since plan") || !strings.Contains(text, "planned just now") {
		t.Errorf("text output:\n%s", text)
	}
}

func TestAgo(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Second: "just now",
		5 * time.Minute:  "5m ago",
		3 * time.Hour:    "3h ago",
		50 * time.Hour:   "2d ago",
	} {
		if got := Ago(d); got != want {
			t.Errorf("Ago(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestPendingReportNotesUnknownWorkspaces(t *testing.T) {
	root := t.TempDir()
	gitRepoWithModule(t, root, "repo1", "envs/prod")
	repos, err := discovery.Discover([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	rows, notes := Pending(state.New(t.TempDir()), state.Ignore{}, repos, false)
	if len(rows) != 0 || len(notes) != 1 || !strings.Contains(notes[0], "workspaces unknown") {
		t.Errorf("rows=%v notes=%v", rows, notes)
	}
}

func TestPendingReportsModulesBehind(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	gittest.WriteFile(t, filepath.Join(shared, "modules/waf/main.tf"), "# v1\n")
	gittest.Run(t, shared, "init", "-q", "-b", "main")
	gittest.Run(t, shared, "remote", "add", "origin", "https://github.com/org/shared")
	gittest.CommitAll(t, shared, "v1")
	gittest.Run(t, shared, "tag", "v1")
	gittest.WriteFile(t, filepath.Join(shared, "modules/waf/main.tf"), "# v2\n")
	gittest.Run(t, shared, "commit", "-q", "-am", "v2")
	gittest.Run(t, shared, "tag", "v2")

	app := gitRepoWithModule(t, root, "app-infra", "terraform/app")
	gittest.WriteFile(t, filepath.Join(app, "terraform/app/waf.tf"), `
module "waf" {
  source = "git::https://github.com/org/shared.git//modules/waf?ref=v1"
}
`)
	gittest.CommitAll(t, app, "pin")
	store := state.New(t.TempDir())
	modPath := filepath.Join(app, "terraform/app")
	if err := store.SaveWorkspaces(modPath, []string{"prod"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	fp, _ := gitstatus.Fingerprint(context.Background(), app, []string{"terraform/app"})
	if err := store.SaveRun(&state.RunRecord{ModulePath: modPath, Workspace: "prod", PlanExitCode: tfexec.PlanClean, Fingerprint: fp, PlanFinished: time.Now()}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Roots = []string{root}
	repos, err := DiscoverWithGit(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	rows, _ := Pending(store, state.Ignore{}, repos, false)
	if len(rows) != 1 || rows[0].Workspace != "" || rows[0].Reasons[0].Code != attention.ModuleBehind || rows[0].Reasons[0].Detail != "waf v1→v2 (via waf)" {
		t.Fatalf("rows = %+v", rows)
	}
	var out strings.Builder
	PrintPending(&out, rows, time.Now())
	if !strings.Contains(out.String(), "terraform/app  -  module-behind") {
		t.Errorf("text:\n%s", out.String())
	}
	if rows, _ := Pending(store, state.Ignore{}, repos, true); len(rows) != 0 {
		t.Errorf("--needs-plan should drop module rows, got %+v", rows)
	}

	mods, _, behind := Deps(state.Ignore{}, repos)
	out.Reset()
	PrintDepsForward(&out, mods, behind, "app-infra")
	if !strings.Contains(out.String(), "@v1  (via waf)  behind: v1→v2") {
		t.Errorf("deps forward:\n%s", out.String())
	}
}
