// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package report

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/espoon-voltti/tfmux/internal/attention"
	"github.com/espoon-voltti/tfmux/internal/deps"
	"github.com/espoon-voltti/tfmux/internal/domain"
	"github.com/espoon-voltti/tfmux/internal/gitstatus"
	"github.com/espoon-voltti/tfmux/internal/state"
)

// PendingRow is one workspace (or, with an empty Workspace, one module) that
// needs attention.
type PendingRow struct {
	Repo         string             `json:"repo"`
	Module       string             `json:"module"`
	Workspace    string             `json:"workspace,omitempty"` // "" for a module-level row
	Reasons      []attention.Reason `json:"reasons"`
	PlanFinished *time.Time         `json:"plan_finished,omitempty"`
}

// Pending assesses every visible workspace of the discovered repos,
// and every visible module's git module pins against the shared repos'
// checkouts. Modules whose workspaces are not known (never enumerated, not
// in a manifest) are listed in notes rather than assessed.
func Pending(store *state.Store, ignore state.Ignore, repos []*domain.Repo, needsPlanOnly bool) (rows []PendingRow, notes []string) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	ctx := context.Background()
	visible := VisibleRepos(ignore, repos)
	// Module rows are collected apart from the workspace goroutines, which
	// append to rows under mu.
	var moduleRows []PendingRow
	var behind map[string]deps.Behind
	if !needsPlanOnly {
		behind = CheckPins(ctx, visible, deps.Build(visible))
	}
	for _, repo := range visible {
		for _, mod := range repo.Modules {
			for _, r := range attention.Module(mod.Deps, behind) {
				moduleRows = append(moduleRows, PendingRow{Repo: repo.Name, Module: mod.RelPath, Reasons: []attention.Reason{r}})
			}
			var workspaces []string
			if mod.ManifestListed {
				workspaces = mod.ManifestWorkspaces
			} else if cache, ok := store.LoadWorkspaces(mod.Path); ok {
				workspaces = cache.Workspaces
			} else {
				notes = append(notes, fmt.Sprintf("%s/%s: workspaces unknown (open tfmux and press w)", repo.Name, mod.RelPath))
				continue
			}
			wg.Add(1)
			go func(repo *domain.Repo, mod *domain.Module, workspaces []string) {
				defer wg.Done()
				fp, _ := gitstatus.Fingerprint(ctx, repo.Path, mod.FingerprintPaths())
				var local []PendingRow
				for _, ws := range workspaces {
					if ignore[mod.Path+"//"+ws] {
						continue
					}
					rec, _ := store.LoadRun(mod.Path, ws)
					rs := attention.Assess(rec, store.HasPlanFile(mod.Path, ws), fp)
					if len(rs) == 0 || (needsPlanOnly && !attention.NeedsPlan(rs)) {
						continue
					}
					row := PendingRow{Repo: repo.Name, Module: mod.RelPath, Workspace: ws, Reasons: rs}
					if rec != nil {
						t := rec.PlanFinished
						row.PlanFinished = &t
					}
					local = append(local, row)
				}
				mu.Lock()
				rows = append(rows, local...)
				mu.Unlock()
			}(repo, mod, workspaces)
		}
	}
	wg.Wait()
	rows = append(rows, moduleRows...)
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		if a.Module != b.Module {
			return a.Module < b.Module
		}
		return a.Workspace < b.Workspace
	})
	sort.Strings(notes)
	return rows, notes
}

func PrintPending(out io.Writer, rows []PendingRow, now time.Time) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		var details []string
		for _, reason := range r.Reasons[1:] {
			details = append(details, string(reason.Code))
		}
		if d := r.Reasons[0].Detail; d != "" {
			details = append([]string{d}, details...)
		}
		if r.PlanFinished != nil {
			details = append(details, "planned "+Ago(now.Sub(*r.PlanFinished)))
		}
		ws := r.Workspace
		if ws == "" {
			ws = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Repo, r.Module, ws, r.Reasons[0].Code, strings.Join(details, ", "))
	}
	tw.Flush()
}

func Ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
