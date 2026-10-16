// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package report

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/espoon-voltti/tfmux/internal/attention"
	"github.com/espoon-voltti/tfmux/internal/deps"
	"github.com/espoon-voltti/tfmux/internal/domain"
	"github.com/espoon-voltti/tfmux/internal/gitstatus"
	"github.com/espoon-voltti/tfmux/internal/state"
)

// EstateRepo is one repo with its visible modules and their workspaces'
// latest outcomes: the whole picture in one structure.
type EstateRepo struct {
	Name     string         `json:"repo"`
	Path     string         `json:"path"`
	Branch   string         `json:"branch,omitempty"`
	Dirty    bool           `json:"dirty"`
	GitError string         `json:"git_error,omitempty"`
	Modules  []EstateModule `json:"modules"`
}

type EstateModule struct {
	Module          string             `json:"module"` // repo-relative path; "repo/module" is the label tools take
	Path            string             `json:"path"`
	Behind          []attention.Reason `json:"behind,omitempty"`
	WorkspacesKnown bool               `json:"workspaces_known"` // false: never enumerated and not in a manifest
	Workspaces      []EstateWorkspace  `json:"workspaces"`
}

type EstateWorkspace struct {
	Name         string             `json:"workspace"`
	PlanExitCode *int               `json:"plan_exit_code,omitempty"` // 0 clean, 1 error, 2 changes; absent when never planned
	Summary      string             `json:"summary,omitempty"`
	PlanFinished *time.Time         `json:"plan_finished,omitempty"`
	PlanFile     bool               `json:"plan_file"`
	Apply        *state.ApplyRecord `json:"apply,omitempty"`
	Reasons      []attention.Reason `json:"reasons,omitempty"`
}

// Workspaces returns a module's known workspace names: the manifest's, else
// the enumeration cache's, else (nil, false).
func Workspaces(store *state.Store, mod *domain.Module) ([]string, bool) {
	if mod.ManifestListed {
		return mod.ManifestWorkspaces, true
	}
	if cache, ok := store.LoadWorkspaces(mod.Path); ok {
		return cache.Workspaces, true
	}
	return nil, false
}

// Estate assesses every visible workspace and pin of the discovered repos.
func Estate(ctx context.Context, store *state.Store, ignore state.Ignore, repos []*domain.Repo) []EstateRepo {
	visible := VisibleRepos(ignore, repos)
	behind := CheckPins(ctx, visible, deps.Build(visible))
	var out []EstateRepo
	var wg sync.WaitGroup
	for _, repo := range visible {
		er := EstateRepo{Name: repo.Name, Path: repo.Path, Branch: repo.Git.Branch, Dirty: repo.Git.Dirty}
		if repo.Git.Err != nil {
			er.GitError = repo.Git.Err.Error()
		}
		er.Modules = make([]EstateModule, len(repo.Modules))
		for i, mod := range repo.Modules {
			wg.Add(1)
			go func(i int, mod *domain.Module) {
				defer wg.Done()
				er.Modules[i] = estateModule(ctx, store, ignore, repo, mod, behind)
			}(i, mod)
		}
		out = append(out, er)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func estateModule(ctx context.Context, store *state.Store, ignore state.Ignore, repo *domain.Repo, mod *domain.Module, behind map[string]deps.Behind) EstateModule {
	em := EstateModule{Module: mod.RelPath, Path: mod.Path, Behind: attention.Module(mod.Deps, behind), Workspaces: []EstateWorkspace{}}
	names, known := Workspaces(store, mod)
	em.WorkspacesKnown = known
	if !known {
		return em
	}
	fp, _ := gitstatus.Fingerprint(ctx, repo.Path, mod.FingerprintPaths())
	for _, ws := range names {
		if ignore[mod.Path+"//"+ws] {
			continue
		}
		em.Workspaces = append(em.Workspaces, AssessWorkspace(store, mod, ws, fp))
	}
	return em
}

// AssessWorkspace assesses one workspace from its persisted record.
func AssessWorkspace(store *state.Store, mod *domain.Module, ws, fingerprint string) EstateWorkspace {
	rec, _ := store.LoadRun(mod.Path, ws)
	planFile := store.HasPlanFile(mod.Path, ws)
	ew := EstateWorkspace{Name: ws, PlanFile: planFile, Reasons: attention.Assess(rec, planFile, fingerprint)}
	if rec != nil {
		code := rec.PlanExitCode
		ew.PlanExitCode = &code
		t := rec.PlanFinished
		ew.PlanFinished = &t
		ew.Apply = rec.Apply
		if rec.Summary.Any() {
			ew.Summary = rec.Summary.String()
		}
	}
	return ew
}
