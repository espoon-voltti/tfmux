// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package ui

import (
	"fmt"
	"testing"

	"github.com/espoon-voltti/tfmux/internal/domain"
)

// repoRow returns the (settled) row the cursor is on, requiring it be a repo.
func cursorRepo(t *testing.T, m *Model) *domain.Repo {
	t.Helper()
	r, ok := m.currentRow()
	if !ok || r.kind != rowRepo {
		t.Fatalf("cursor not on a repo row: %+v (ok=%v)", r, ok)
	}
	return r.repo
}

// twoRepos extends the fixture with a second repo so the jump has somewhere to
// land that isn't already under the cursor.
func twoRepos(t *testing.T) (*Model, *domain.Repo, *domain.Repo) {
	t.Helper()
	m, mod := fixtureModel(t)
	repo1 := mod.Repo
	repo2 := &domain.Repo{Path: "/iac/repo2", Name: "repo2"}
	mod2 := &domain.Module{Repo: repo2, Path: "/iac/repo2/envs/dev", RelPath: "envs/dev"}
	repo2.Modules = []*domain.Module{mod2}
	m.repos = append(m.repos, repo2)
	m.reflow()
	m.cursor = 0 // start on repo1
	return m, repo1, repo2
}

// A repo far down the list lands at the top of the viewport (so its modules
// and workspaces are visible), not merely on the bottom row.
func TestJumpToStartRepoPositionsAtTop(t *testing.T) {
	m, _ := fixtureModel(t)
	var repos []*domain.Repo
	for i := 0; i < 100; i++ {
		repos = append(repos, &domain.Repo{Path: fmt.Sprintf("/iac/r%03d", i), Name: fmt.Sprintf("r%03d", i)})
	}
	m.repos = repos
	m.reflow()
	m.cursor, m.top = 0, 0 // start at the very top

	m.startDir = repos[40].Path
	m.jumpToStartRepo()

	if m.cursor != 40 {
		t.Fatalf("cursor = %d, want 40", m.cursor)
	}
	if m.top != m.cursor {
		t.Fatalf("repo should sit at the top of the viewport: top=%d cursor=%d", m.top, m.cursor)
	}
}

func TestJumpToStartRepoInModule(t *testing.T) {
	m, _, repo2 := twoRepos(t)
	m.startDir = "/iac/repo2/envs/dev" // launched from a module dir inside repo2
	m.jumpToStartRepo()
	if got := cursorRepo(t, m); got != repo2 {
		t.Fatalf("expected cursor on repo2, got %q", got.Path)
	}
}

func TestJumpToStartRepoAtRoot(t *testing.T) {
	m, _, repo2 := twoRepos(t)
	m.startDir = "/iac/repo2" // launched from the repo root itself
	m.jumpToStartRepo()
	if got := cursorRepo(t, m); got != repo2 {
		t.Fatalf("expected cursor on repo2, got %q", got.Path)
	}
}

func TestJumpToStartRepoOutside(t *testing.T) {
	m, repo1, _ := twoRepos(t)
	m.startDir = "/somewhere/else" // not under any managed repo
	m.jumpToStartRepo()
	if got := cursorRepo(t, m); got != repo1 {
		t.Fatalf("cursor should stay put (repo1), got %q", got.Path)
	}
}

// A sibling whose path is a string prefix of the launch dir but not a path
// prefix ("/iac/repo2-staging" vs "/iac/repo2") must not match.
func TestJumpToStartRepoNoSubstringMatch(t *testing.T) {
	m, repo1, _ := twoRepos(t)
	m.startDir = "/iac/repo2-staging"
	m.jumpToStartRepo()
	if got := cursorRepo(t, m); got != repo1 {
		t.Fatalf("substring sibling should not match; expected repo1, got %q", got.Path)
	}
}

// On nested repos, the most specific (longest matching) repo wins.
func TestJumpToStartRepoNestedPrefersLongest(t *testing.T) {
	m, _, _ := twoRepos(t)
	outer := &domain.Repo{Path: "/iac/outer", Name: "outer"}
	inner := &domain.Repo{Path: "/iac/outer/vendor/inner", Name: "inner"}
	m.repos = append(m.repos, outer, inner)
	m.reflow()
	m.startDir = "/iac/outer/vendor/inner/mod"
	m.jumpToStartRepo()
	if got := cursorRepo(t, m); got != inner {
		t.Fatalf("expected the most specific repo (inner), got %q", got.Path)
	}
}
