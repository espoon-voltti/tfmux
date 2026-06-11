package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/japsu/tfmux/internal/domain"
	"github.com/japsu/tfmux/internal/state"
	"github.com/japsu/tfmux/internal/tfexec"
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
	switch r.kind {
	case rowRepo:
		line = m.renderRepoRow(r.repo)
	case rowModule:
		line = m.renderModuleRow(r.mod)
	case rowWorkspace:
		line = m.renderWorkspaceRow(r.ws)
	}
	if m.ignore[r.nodeKey()] {
		line += styleDim.Render("  (ignored)")
	}
	if selected {
		pad := width - lipglossWidth(line)
		if pad > 0 {
			line += strings.Repeat(" ", pad)
		}
		return styleCursor.Render(line)
	}
	return line
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
	switch mod.WorkspaceState {
	case domain.WorkspacesUnknown:
		s += styleDim.Render("…")
	case domain.WorkspacesLoading:
		s += styleRunning.Render(m.spinner.View() + " loading workspaces")
	case domain.WorkspacesError:
		s += styleError.Render("✗ " + firstLine(mod.WorkspaceErr))
	}
	return s
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
	if m.planning[key] {
		return styleRunning.Render(m.spinner.View() + " planning")
	}
	rec := m.runs[key]
	if rec == nil {
		return styleDim.Render("never planned")
	}
	if rec.Apply != nil && rec.Apply.ExitCode == nil && !rec.Apply.Aborted {
		return styleRunning.Render(m.spinner.View() + " applying (tmux)")
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
