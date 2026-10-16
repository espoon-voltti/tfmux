// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package deps

import (
	"sort"

	"github.com/espoon-voltti/tfmux/internal/domain"
)

// Index answers dependency questions across every discovered root module. It
// holds direct edges only; nothing here walks the graph, so root modules
// that read each other's state (which happens) are harmless.
type Index struct {
	modules []*domain.Module
}

// Build indexes the modules of repos that have been scanned (nil Deps are
// skipped).
func Build(repos []*domain.Repo) *Index {
	ix := &Index{}
	for _, repo := range repos {
		for _, mod := range repo.Modules {
			if mod.Deps != nil {
				ix.modules = append(ix.modules, mod)
			}
		}
	}
	return ix
}

// Upstream returns the root module whose backend holds the state ref reads,
// or nil when none is discovered.
func (ix *Index) Upstream(ref domain.RemoteStateRef) *domain.Module {
	for _, mod := range ix.modules {
		if mod.Deps.Backend != nil && mod.Deps.Backend.SameState(ref.StateAddress) {
			return mod
		}
	}
	return nil
}

// DependentsOfGit returns the root modules calling the git module keyed
// "host/org/name//subdir" (see GitRef.Key), at any ref.
func (ix *Index) DependentsOfGit(key string) []*domain.Module {
	var out []*domain.Module
	for _, mod := range ix.modules {
		for _, g := range mod.Deps.Git {
			if g.Key() == key {
				out = append(out, mod)
				break
			}
		}
	}
	return sorted(out)
}

// DependentsOfState returns the root modules reading mod's state.
func (ix *Index) DependentsOfState(mod *domain.Module) []*domain.Module {
	if mod.Deps == nil || mod.Deps.Backend == nil {
		return nil
	}
	var out []*domain.Module
	for _, other := range ix.modules {
		for _, rs := range other.Deps.RemoteStates {
			if mod.Deps.Backend.SameState(rs.StateAddress) {
				out = append(out, other)
				break
			}
		}
	}
	return sorted(out)
}

// GitKeys lists every git module key in use, sorted.
func (ix *Index) GitKeys() []string {
	seen := map[string]bool{}
	var keys []string
	for _, mod := range ix.modules {
		for _, g := range mod.Deps.Git {
			if !seen[g.Key()] {
				seen[g.Key()] = true
				keys = append(keys, g.Key())
			}
		}
	}
	sort.Strings(keys)
	return keys
}

// StateKeys lists every remote state address read by some module, sorted by
// its String form, deduplicated by SameState.
func (ix *Index) StateKeys() []domain.StateAddress {
	var addrs []domain.StateAddress
	for _, mod := range ix.modules {
		for _, rs := range mod.Deps.RemoteStates {
			dup := false
			for _, a := range addrs {
				if a.SameState(rs.StateAddress) {
					dup = true
					break
				}
			}
			if !dup {
				addrs = append(addrs, rs.StateAddress)
			}
		}
	}
	sort.Slice(addrs, func(i, j int) bool { return addrs[i].String() < addrs[j].String() })
	return addrs
}

// DependentsOfAddress returns the root modules reading the state at addr.
func (ix *Index) DependentsOfAddress(addr domain.StateAddress) []*domain.Module {
	var out []*domain.Module
	for _, mod := range ix.modules {
		for _, rs := range mod.Deps.RemoteStates {
			if addr.SameState(rs.StateAddress) {
				out = append(out, mod)
				break
			}
		}
	}
	return sorted(out)
}

func sorted(mods []*domain.Module) []*domain.Module {
	sort.Slice(mods, func(i, j int) bool { return mods[i].Path < mods[j].Path })
	return mods
}
