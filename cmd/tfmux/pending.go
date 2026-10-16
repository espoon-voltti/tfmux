// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/espoon-voltti/tfmux/internal/attention"
	"github.com/espoon-voltti/tfmux/internal/domain"
	"github.com/espoon-voltti/tfmux/internal/gitstatus"
	"github.com/espoon-voltti/tfmux/internal/paths"
	"github.com/espoon-voltti/tfmux/internal/state"
)

// pendingRow is one workspace that needs attention.
type pendingRow struct {
	Repo         string             `json:"repo"`
	Module       string             `json:"module"`
	Workspace    string             `json:"workspace"`
	Reasons      []attention.Reason `json:"reasons"`
	PlanFinished *time.Time         `json:"plan_finished,omitempty"`
}

// pendingReport assesses every visible workspace of the discovered repos.
// Modules whose workspaces are not known (never enumerated, not in a
// manifest) are listed in notes rather than assessed.
func pendingReport(store *state.Store, ignore state.Ignore, repos []*domain.Repo, needsPlanOnly bool) (rows []pendingRow, notes []string) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	ctx := context.Background()
	for _, repo := range repos {
		for _, mod := range repo.Modules {
			if ignore.HidesModule(mod) {
				continue
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
				var local []pendingRow
				for _, ws := range workspaces {
					if ignore[mod.Path+"//"+ws] {
						continue
					}
					rec, _ := store.LoadRun(mod.Path, ws)
					rs := attention.Assess(rec, store.HasPlanFile(mod.Path, ws), fp)
					if len(rs) == 0 || (needsPlanOnly && !attention.NeedsPlan(rs)) {
						continue
					}
					row := pendingRow{Repo: repo.Name, Module: mod.RelPath, Workspace: ws, Reasons: rs}
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

func printPending(out io.Writer, rows []pendingRow, now time.Time) {
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
			details = append(details, "planned "+ago(now.Sub(*r.PlanFinished)))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Repo, r.Module, r.Workspace, r.Reasons[0].Code, strings.Join(details, ", "))
	}
	tw.Flush()
}

func ago(d time.Duration) string {
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

func runPending(args []string) error {
	asJSON, needsPlanOnly := false, false
	for _, a := range args {
		switch a {
		case "--json":
			asJSON = true
		case "--needs-plan":
			needsPlanOnly = true
		default:
			return fmt.Errorf("pending: unknown flag %q", a)
		}
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	store, ignore, err := openStore()
	if err != nil {
		return err
	}
	repos, err := discoverWithGit(context.Background(), cfg)
	if err != nil {
		return err
	}
	rows, notes := pendingReport(store, ignore, repos, needsPlanOnly)
	for _, n := range notes {
		fmt.Fprintln(os.Stderr, "tfmux:", n)
	}
	if asJSON {
		if rows == nil {
			rows = []pendingRow{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	printPending(os.Stdout, rows, time.Now())
	return nil
}

// openStore opens the state store and its ignore list.
func openStore() (*state.Store, state.Ignore, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return nil, nil, err
	}
	store := state.New(dir)
	ignore, err := store.LoadIgnore()
	if err != nil {
		return nil, nil, err
	}
	return store, ignore, nil
}
