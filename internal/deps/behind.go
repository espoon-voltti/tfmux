// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package deps

import (
	"context"
	"strings"

	"github.com/espoon-voltti/tfmux/internal/domain"
	"github.com/espoon-voltti/tfmux/internal/gitstatus"
)

// Behind describes how a pinned git module ref relates to the shared repo's
// local checkout. The zero value means the pin is up to date with everything
// the checkout knows; tfmux never fetches, so that is only as fresh as the
// checkout.
type Behind struct {
	Ref        string `json:"ref"`
	RefMissing bool   `json:"ref_missing,omitempty"` // ref not present locally: not fetched, or a typo
	NewerTag   string `json:"newer_tag,omitempty"`   // newest tag whose subdir tree differs from the ref's and which the ref does not already contain
	Unreleased bool   `json:"unreleased,omitempty"`  // HEAD's subdir tree differs from the newest of the ref and the newest tag
	Dirty      bool   `json:"dirty,omitempty"`       // uncommitted changes under subdir
}

// Any reports whether the pin is behind in some way.
func (b Behind) Any() bool { return b.RefMissing || b.NewerTag != "" || b.Unreleased || b.Dirty }

func (b Behind) String() string {
	var parts []string
	if b.RefMissing {
		parts = append(parts, "ref not found locally")
	}
	if b.NewerTag != "" {
		parts = append(parts, b.Ref+"→"+b.NewerTag)
	}
	if b.Unreleased {
		parts = append(parts, "unreleased")
	}
	if b.Dirty {
		parts = append(parts, "uncommitted")
	}
	return strings.Join(parts, ", ")
}

// RefKey identifies one pin of a git module: its Key plus the ref.
func RefKey(g domain.GitRef) string { return g.Key() + "@" + g.Ref }

// CheckRefs compares every pinned ref against the checkout at repoPath,
// which must be the repository the refs name. Unpinned refs are skipped. The
// result is keyed by RefKey.
//
// A ref that already contains the newest tag (a branch, or a commit after
// the tag) is never reported as behind that tag: the pin floats on purpose
// and bumping it to the tag would be a downgrade.
func CheckRefs(ctx context.Context, repoPath string, refs []domain.GitRef) map[string]Behind {
	out := map[string]Behind{}
	newest, _ := gitstatus.NewestTag(ctx, repoPath)
	for _, g := range refs {
		key := RefKey(g)
		if g.Ref == "" {
			continue
		}
		if _, done := out[key]; done {
			continue
		}
		b := Behind{Ref: g.Ref}
		refTree, found, err := gitstatus.TreeID(ctx, repoPath, g.Ref, g.Subdir)
		if err != nil {
			continue
		}
		if !found {
			b.RefMissing = true
		}
		latest := refTree
		if found && newest != "" && newest != g.Ref {
			tagTree, ok, _ := gitstatus.TreeID(ctx, repoPath, newest, g.Subdir)
			contains, _ := gitstatus.IsAncestor(ctx, repoPath, newest, g.Ref)
			if ok && tagTree != refTree && !contains {
				b.NewerTag = newest
				latest = tagTree
			}
		}
		if headTree, _, _ := gitstatus.TreeID(ctx, repoPath, "HEAD", g.Subdir); found && headTree != latest {
			b.Unreleased = true
		}
		if dirty, _ := gitstatus.HasChanges(ctx, repoPath, []string{g.Subdir}); dirty {
			b.Dirty = true
		}
		out[key] = b
	}
	return out
}
