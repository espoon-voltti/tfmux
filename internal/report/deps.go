// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package report

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/espoon-voltti/tfmux/internal/deps"
	"github.com/espoon-voltti/tfmux/internal/domain"
	"github.com/espoon-voltti/tfmux/internal/state"
)

// DepsModule is one root module's dependencies as `tfmux deps --json`
// prints them: the scanned Deps plus each remote state resolved to the root
// module holding it, when one is discovered.
type DepsModule struct {
	Repo      string                 `json:"repo"`
	Module    string                 `json:"module"`
	Path      string                 `json:"path"`
	Deps      *domain.Deps           `json:"deps"`
	Upstreams []DepsUpstream         `json:"upstreams,omitempty"`
	Behind    map[string]deps.Behind `json:"behind,omitempty"` // deps.RefKey -> comparison, only for pins that are behind
}

type DepsUpstream struct {
	RemoteState string `json:"remote_state"`   // the data block's name
	Repo        string `json:"repo,omitempty"` // resolved root module, when discovered
	Module      string `json:"module,omitempty"`
}

// Deps lists the visible root modules with their dependencies, and the
// index for reverse lookups over the same set. Pins of git modules whose
// repo is checked out under the roots are compared against that checkout.
func Deps(ignore state.Ignore, repos []*domain.Repo) ([]DepsModule, *deps.Index, map[string]deps.Behind) {
	visible := VisibleRepos(ignore, repos)
	ix := deps.Build(visible)
	behind := CheckPins(context.Background(), visible, ix)
	var out []DepsModule
	for _, repo := range visible {
		for _, mod := range repo.Modules {
			if mod.Deps == nil {
				continue
			}
			dm := DepsModule{Repo: repo.Name, Module: mod.RelPath, Path: mod.Path, Deps: mod.Deps}
			for _, g := range mod.Deps.Git {
				if b, ok := behind[deps.RefKey(g)]; ok && b.Any() {
					if dm.Behind == nil {
						dm.Behind = map[string]deps.Behind{}
					}
					dm.Behind[deps.RefKey(g)] = b
				}
			}
			for _, rs := range mod.Deps.RemoteStates {
				up := DepsUpstream{RemoteState: rs.Name}
				if upstream := ix.Upstream(rs); upstream != nil {
					up.Repo, up.Module = upstream.Repo.Name, upstream.RelPath
				}
				dm.Upstreams = append(dm.Upstreams, up)
			}
			out = append(out, dm)
		}
	}
	return out, ix, behind
}

// behindNote renders the comparison for one pin as a line suffix, or "".
func behindNote(behind map[string]deps.Behind, g domain.GitRef) string {
	if b, ok := behind[deps.RefKey(g)]; ok && b.Any() {
		return "  behind: " + b.String()
	}
	return ""
}

func ModuleLabel(mod *domain.Module) string { return mod.Repo.Name + "/" + mod.RelPath }

func PrintDepsForward(out io.Writer, mods []DepsModule, behind map[string]deps.Behind, filter string) {
	for _, dm := range mods {
		label := dm.Repo + "/" + dm.Module
		if filter != "" && !strings.Contains(label, filter) {
			continue
		}
		fmt.Fprintln(out, label)
		for _, g := range dm.Deps.Git {
			ref := g.Ref
			if ref == "" {
				ref = "(unpinned)"
			}
			fmt.Fprintf(out, "  git     %s @%s  (via %s)%s\n", g.Key(), ref, g.Via, behindNote(behind, g))
		}
		for _, l := range dm.Deps.Local {
			where := l.RelPath
			if !l.InRepo {
				where = l.Dir + "  (outside the repo: not fingerprinted)"
			}
			fmt.Fprintf(out, "  local   %s  (via %s)\n", where, l.Via)
		}
		for i, rs := range dm.Deps.RemoteStates {
			target := rs.String() + "  (no root module found)"
			if up := dm.Upstreams[i]; up.Module != "" {
				target = up.Repo + "/" + up.Module
			}
			fmt.Fprintf(out, "  state   %s  (%s, workspace: %s)\n", target, rs.Name, describeWorkspace(rs))
		}
	}
}

func describeWorkspace(rs domain.RemoteStateRef) string {
	switch rs.WorkspaceKind {
	case domain.WorkspaceSame:
		return "same"
	case domain.WorkspaceLiteral:
		return rs.WorkspaceExpr
	case domain.WorkspaceTemplate:
		return rs.WorkspaceExpr
	default:
		return rs.WorkspaceExpr + " — unresolved"
	}
}

// PrintDepsReverse lists, for every git module and remote state in use, the
// root modules depending on it. filter substring-matches the key.
func PrintDepsReverse(out io.Writer, ix *deps.Index, behind map[string]deps.Behind, filter string) {
	for _, key := range ix.GitKeys() {
		if filter != "" && !strings.Contains(key, filter) {
			continue
		}
		fmt.Fprintln(out, key)
		for _, mod := range ix.DependentsOfGit(key) {
			for _, g := range mod.Deps.Git {
				if g.Key() == key {
					fmt.Fprintf(out, "  %s @%s  (via %s)%s\n", ModuleLabel(mod), g.Ref, g.Via, behindNote(behind, g))
				}
			}
		}
	}
	for _, addr := range ix.StateKeys() {
		key := addr.String()
		if filter != "" && !strings.Contains(key, filter) {
			continue
		}
		fmt.Fprintln(out, key)
		for _, mod := range ix.DependentsOfAddress(addr) {
			for _, rs := range mod.Deps.RemoteStates {
				if addr.SameState(rs.StateAddress) {
					fmt.Fprintf(out, "  %s  (%s, workspace: %s)\n", ModuleLabel(mod), rs.Name, describeWorkspace(rs))
				}
			}
		}
	}
}
