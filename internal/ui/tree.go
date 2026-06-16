// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/espoon-voltti/tfmux/internal/domain"
	"github.com/espoon-voltti/tfmux/internal/runner"
	"github.com/espoon-voltti/tfmux/internal/state"
	"github.com/espoon-voltti/tfmux/internal/tfexec"
)

func lipglossWidth(s string) int { return lipgloss.Width(s) }

type rowKind int

const (
	rowRepo rowKind = iota
	rowModule
	rowWorkspace
)

// row is one visible line of the flattened repo→module→workspace tree.
type row struct {
	kind rowKind
	repo *domain.Repo
	mod  *domain.Module
	ws   *domain.Workspace
}

// nodeKey identifies the row's underlying item for ignore/collapse/mark maps.
func (r row) nodeKey() string {
	switch r.kind {
	case rowRepo:
		return r.repo.Path
	case rowModule:
		return r.mod.Path
	default:
		return r.ws.Key()
	}
}

// flatten produces the visible rows honoring ignore, collapse and filter
// state. Parent rows appear whenever any descendant matches the filter.
func (m *Model) flatten() []row {
	var rows []row
	filter := strings.ToLower(m.filterText)
	for _, repo := range m.repos {
		repoIgnored := m.ignore[repo.Path]
		if repoIgnored && !m.showIgnored {
			continue
		}
		var moduleRows []row
		for _, mod := range repo.Modules {
			modIgnored := repoIgnored || m.ignore[mod.Path]
			if m.ignore[mod.Path] && !m.showIgnored {
				continue
			}
			var wsRows []row
			for _, ws := range mod.Workspaces {
				if m.ignore[ws.Key()] && !m.showIgnored {
					continue
				}
				if filter != "" && !matchesFilter(filter, repo, mod, ws) {
					continue
				}
				wsRows = append(wsRows, row{kind: rowWorkspace, repo: repo, mod: mod, ws: ws})
			}
			modMatches := filter == "" || matchesFilter(filter, repo, mod, nil) || len(wsRows) > 0
			if !modMatches {
				continue
			}
			moduleRows = append(moduleRows, row{kind: rowModule, repo: repo, mod: mod})
			if !m.collapsed[mod.Path] && !modIgnored {
				moduleRows = append(moduleRows, wsRows...)
			}
		}
		repoMatches := filter == "" || strings.Contains(strings.ToLower(repo.Name), filter) || len(moduleRows) > 0
		if !repoMatches {
			continue
		}
		rows = append(rows, row{kind: rowRepo, repo: repo})
		if !m.collapsed[repo.Path] {
			rows = append(rows, moduleRows...)
		}
	}
	return rows
}

func matchesFilter(filter string, repo *domain.Repo, mod *domain.Module, ws *domain.Workspace) bool {
	if strings.Contains(strings.ToLower(repo.Name), filter) {
		return true
	}
	if mod != nil && strings.Contains(strings.ToLower(mod.RelPath), filter) {
		return true
	}
	if ws != nil && strings.Contains(strings.ToLower(ws.Name), filter) {
		return true
	}
	return false
}

// renderRow renders one line of the tree at the given width.
func (m *Model) renderRow(r row, selected bool, width int) string {
	var line string
	if m.ignore[r.nodeKey()] {
		// Ignored items are only visible under Z; render them uniformly muted
		// (single style over plain text) so they read as inactive at a glance.
		line = m.renderIgnoredRow(r)
	} else {
		switch r.kind {
		case rowRepo:
			line = m.renderRepoRow(r.repo)
		case rowModule:
			line = m.renderModuleRow(r.mod)
		case rowWorkspace:
			line = m.renderWorkspaceRow(r.ws)
		}
	}
	if selected {
		// Render the selection as a solid bar: strip the per-segment styling
		// and re-color the whole row in the cursor style. Nested ANSI resets
		// would otherwise punch holes in the background, and a dim ignored
		// foreground would vanish against it.
		plain := ansi.Strip(line)
		if pad := width - lipglossWidth(plain); pad > 0 {
			plain += strings.Repeat(" ", pad)
		}
		return styleCursor.Render(plain)
	}
	return line
}

// renderIgnoredRow renders an explicitly-ignored node as a single muted,
// plain-text line — keeping the tree indentation but dropping the status cell,
// which is irrelevant for items the user has chosen to skip.
func (m *Model) renderIgnoredRow(r row) string {
	var label string
	switch r.kind {
	case rowRepo:
		marker := "▾"
		if m.collapsed[r.repo.Path] {
			marker = "▸"
		}
		label = fmt.Sprintf("%s %s", marker, r.repo.Name)
	case rowModule:
		marker := "▾"
		if m.collapsed[r.mod.Path] {
			marker = "▸"
		}
		label = fmt.Sprintf("  %s %s", marker, r.mod.RelPath)
	case rowWorkspace:
		label = fmt.Sprintf("      %s", r.ws.Name)
	}
	return styleIgnored.Render(label + "  (ignored)")
}

func (m *Model) renderRepoRow(repo *domain.Repo) string {
	marker := "▾"
	if m.collapsed[repo.Path] {
		marker = "▸"
	}
	s := fmt.Sprintf("%s %s  ", marker, styleRepo.Render(repo.Name))
	g := repo.Git
	switch {
	case g.Err != nil:
		s += styleError.Render("git error")
	case g.Detached:
		s += styleDim.Render("detached")
	default:
		s += styleDim.Render(g.Branch)
	}
	if g.Dirty {
		s += styleChanges.Render(" *")
	}
	if g.Ahead > 0 {
		s += styleDim.Render(fmt.Sprintf(" ↑%d", g.Ahead))
	}
	if g.Behind > 0 {
		s += styleDim.Render(fmt.Sprintf(" ↓%d", g.Behind))
	}
	return s
}

func (m *Model) renderModuleRow(mod *domain.Module) string {
	marker := "▾"
	if m.collapsed[mod.Path] {
		marker = "▸"
	}
	s := fmt.Sprintf("  %s %s  ", marker, styleModule.Render(mod.RelPath))
	switch {
	case m.task(runner.KindInit, mod.Path) != nil:
		s += m.taskBadge(m.task(runner.KindInit, mod.Path), "init -upgrade", "init queued")
	case m.task(runner.KindEnumerate, mod.Path) != nil:
		s += m.taskBadge(m.task(runner.KindEnumerate, mod.Path), "listing workspaces", "workspaces queued")
	case mod.WorkspaceState == domain.WorkspacesUnknown:
		s += styleDim.Render("…")
	case mod.WorkspaceState == domain.WorkspacesError:
		s += styleError.Render("✗ " + firstLine(mod.WorkspaceErr))
	}
	return s
}

// taskBadge renders an in-flight task's status cell: an animated spinner with
// runningLabel while executing, or a static dim marker with queuedLabel while
// it waits for a worker slot.
func (m *Model) taskBadge(ts *taskState, runningLabel, queuedLabel string) string {
	if ts.running {
		return styleRunning.Render(m.spinner.View() + " " + runningLabel)
	}
	return styleDim.Render("◌ " + queuedLabel)
}

func (m *Model) renderWorkspaceRow(ws *domain.Workspace) string {
	key := ws.Key()
	mark := " "
	if m.marked[key] {
		mark = styleMarked.Render("●")
	}
	s := fmt.Sprintf("    %s %s  ", mark, styleWS.Render(ws.Name))
	s += m.renderWorkspaceStatus(ws)
	return s
}

// renderWorkspaceStatus is the at-a-glance status cell: running spinner,
// outstanding changes (the must-stand-out case), clean, error, apply state.
func (m *Model) renderWorkspaceStatus(ws *domain.Workspace) string {
	key := ws.Key()
	if ts := m.task(runner.KindPlan, key); ts != nil {
		return m.taskBadge(ts, "planning", "plan queued")
	}
	if ts := m.task(runner.KindApply, key); ts != nil {
		return m.taskBadge(ts, "applying (tmux)", "apply queued")
	}
	rec := m.runs[key]
	if rec == nil {
		return styleDim.Render("never planned")
	}
	var parts []string
	switch rec.PlanExitCode {
	case tfexec.PlanError:
		parts = append(parts, styleError.Render("✗ plan error"))
	case tfexec.PlanClean:
		parts = append(parts, styleGood.Render("✓ clean"))
	case tfexec.PlanChanges:
		if m.planFiles[key] {
			parts = append(parts, styleChanges.Render("● "+rec.Summary.String()))
			if m.isStale(rec) {
				parts = append(parts, styleStale.Render("STALE"))
			}
		} else {
			parts = append(parts, styleDim.Render("changes (plan expired)"))
		}
	}
	parts = append(parts, styleDim.Render(humanDur(rec.PlanFinished)))
	if rec.Apply != nil {
		a := rec.Apply
		switch {
		case a.Aborted:
			parts = append(parts, styleError.Render("apply aborted"))
		case a.ExitCode != nil && *a.ExitCode == 0:
			parts = append(parts, styleGood.Render("applied "+humanDur(*a.Finished)))
		case a.ExitCode != nil:
			parts = append(parts, styleError.Render(fmt.Sprintf("apply failed (%d)", *a.ExitCode)))
		}
	}
	return strings.Join(parts, " ")
}

// isStale reports whether the module's git content changed since the plan.
func (m *Model) isStale(rec *state.RunRecord) bool {
	current, ok := m.fingerprints[rec.ModulePath]
	if !ok {
		return false
	}
	return current != rec.GitHead+"|"+rec.DirtyHash
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
