// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package main

import (
	"context"
	"sync"

	"github.com/espoon-voltti/tfmux/internal/deps"
	"github.com/espoon-voltti/tfmux/internal/domain"
	"github.com/espoon-voltti/tfmux/internal/state"
)

// visibleRepos returns copies of the repos holding only the modules the user
// manages: not ignored, not manifest-hidden. Repos without modules are kept,
// since shared-module repos have none.
func visibleRepos(ignore state.Ignore, repos []*domain.Repo) []*domain.Repo {
	var out []*domain.Repo
	for _, repo := range repos {
		r := *repo
		r.Modules = nil
		for _, mod := range repo.Modules {
			if !ignore.HidesModule(mod) {
				r.Modules = append(r.Modules, mod)
			}
		}
		out = append(out, &r)
	}
	return out
}

// checkPins compares every pinned git module against the checkout of the
// discovered repo whose origin remote it names, one repo at a time in
// parallel. Pins of repos not checked out under the roots are absent from
// the result.
func checkPins(ctx context.Context, repos []*domain.Repo, ix *deps.Index) map[string]deps.Behind {
	var mu sync.Mutex
	var wg sync.WaitGroup
	out := map[string]deps.Behind{}
	for _, repo := range repos {
		if repo.Git.RemoteURL == "" {
			continue
		}
		refs := ix.RefsTo(deps.NormalizeRepoURL(repo.Git.RemoteURL))
		if len(refs) == 0 {
			continue
		}
		wg.Add(1)
		go func(path string, refs []domain.GitRef) {
			defer wg.Done()
			res := deps.CheckRefs(ctx, path, refs)
			mu.Lock()
			for k, v := range res {
				out[k] = v
			}
			mu.Unlock()
		}(repo.Path, refs)
	}
	wg.Wait()
	return out
}
