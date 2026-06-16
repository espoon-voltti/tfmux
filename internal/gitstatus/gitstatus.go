// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

// Package gitstatus reads a repo's branch/dirty/ahead-behind snapshot by
// shelling out to the git CLI. `git status --porcelain=v2 --branch` is a
// stable machine-readable format and yields everything in one subprocess.
package gitstatus

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/japsu/tfmux/internal/domain"
)

// Client fetches git status for a repo. Interface so the UI can be tested
// with a fake, and so a GitHub PR-count enricher can sit beside it later.
type Client interface {
	Status(ctx context.Context, repoPath string) domain.GitStatus
}

// CLI is the production Client backed by the git binary.
type CLI struct {
	GitBin string // defaults to "git" when empty
}

func (c CLI) Status(ctx context.Context, repoPath string) domain.GitStatus {
	bin := c.GitBin
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(ctx, bin, "-C", repoPath, "status", "--porcelain=v2", "--branch")
	out, err := cmd.Output()
	if err != nil {
		var detail string
		if ee, ok := err.(*exec.ExitError); ok {
			detail = strings.TrimSpace(string(ee.Stderr))
		}
		return domain.GitStatus{Err: fmt.Errorf("git status in %s: %w (%s)", repoPath, err, detail)}
	}
	return Parse(string(out))
}

// Parse decodes porcelain=v2 --branch output.
//
// Header lines:
//
//	# branch.oid <oid> | (initial)
//	# branch.head <name> | (detached)
//	# branch.upstream <remote>/<branch>     (only when set)
//	# branch.ab +<ahead> -<behind>          (only when upstream resolves)
//
// Any non-# line is a changed/untracked/unmerged entry => dirty.
func Parse(out string) domain.GitStatus {
	var st domain.GitStatus
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "# ") {
			st.Dirty = true
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		switch fields[1] {
		case "branch.oid":
			st.OID = fields[2]
		case "branch.head":
			if fields[2] == "(detached)" {
				st.Detached = true
			} else {
				st.Branch = fields[2]
			}
		case "branch.upstream":
			st.HasUpstream = true
		case "branch.ab":
			if len(fields) >= 4 {
				fmt.Sscanf(fields[2], "+%d", &st.Ahead)
				fmt.Sscanf(fields[3], "-%d", &st.Behind)
			}
		}
	}
	return st
}
