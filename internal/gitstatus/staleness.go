// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package gitstatus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
)

// Head returns the repo's HEAD commit, or "" for an unborn branch.
func Head(ctx context.Context, repoPath string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "rev-parse", "HEAD").Output()
	if err != nil {
		// Unborn branch (fresh repo): treat as empty head, not an error.
		if ee, ok := err.(*exec.ExitError); ok && strings.Contains(string(ee.Stderr), "unknown revision") {
			return "", nil
		}
		return "", fmt.Errorf("git rev-parse HEAD in %s: %w", repoPath, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// DirtyHash fingerprints the uncommitted state of one module directory:
// sha256 over `git status --porcelain` and `git diff HEAD` scoped to the
// module's path. A saved plan whose recorded hash no longer matches is
// flagged STALE in the UI.
//
// Known limitation (accepted): edits *inside* an already-untracked file keep
// the same porcelain line, so they evade this hash. Terraform's state-serial
// pinning inside the plan file is the real safety net; this is a UX hint.
func DirtyHash(ctx context.Context, repoPath, moduleRel string) (string, error) {
	h := sha256.New()
	for _, args := range [][]string{
		{"-C", repoPath, "status", "--porcelain", "--", moduleRel},
		{"-C", repoPath, "diff", "HEAD", "--", moduleRel},
	} {
		out, err := exec.CommandContext(ctx, "git", args...).Output()
		if err != nil {
			// `diff HEAD` fails on unborn branches; hash what we have.
			if _, ok := err.(*exec.ExitError); ok {
				continue
			}
			return "", fmt.Errorf("git %s in %s: %w", strings.Join(args, " "), repoPath, err)
		}
		h.Write(out)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
