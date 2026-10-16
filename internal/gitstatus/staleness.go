// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package gitstatus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Fingerprint identifies the content of the given repo-relative paths,
// committed and uncommitted: the tree id of each path at HEAD, then a hash
// over `git status --porcelain` and `git diff HEAD` scoped to all of them.
// Any change to any of the paths changes the result; commits elsewhere in
// the repo do not. A path with no tree at HEAD (untracked, or an unborn
// branch) contributes an empty id rather than an error.
//
// Known limitation (accepted): edits *inside* an already-untracked file keep
// the same porcelain line, so they evade the hash. Terraform's state-serial
// pinning inside the plan file is the real safety net; this is a UX hint.
func Fingerprint(ctx context.Context, repoPath string, relPaths []string) (string, error) {
	var parts []string
	for _, rel := range relPaths {
		id, err := treeID(ctx, repoPath, rel)
		if err != nil {
			return "", err
		}
		parts = append(parts, id)
	}
	dirty, err := dirtyHash(ctx, repoPath, relPaths)
	if err != nil {
		return "", err
	}
	return strings.Join(parts, ",") + "|" + dirty, nil
}

// treeID returns the id of rel's tree at HEAD, or "" when HEAD has no such
// path.
func treeID(ctx context.Context, repoPath, rel string) (string, error) {
	id, _, err := TreeID(ctx, repoPath, "HEAD", rel)
	return id, err
}

func dirtyHash(ctx context.Context, repoPath string, relPaths []string) (string, error) {
	h := sha256.New()
	for _, base := range [][]string{
		{"-C", repoPath, "status", "--porcelain", "--"},
		{"-C", repoPath, "diff", "HEAD", "--"},
	} {
		args := append(base, relPaths...)
		out, err := exec.CommandContext(ctx, "git", args...).Output()
		if err != nil {
			// `diff HEAD` fails on unborn branches; hash what we have.
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				continue
			}
			return "", fmt.Errorf("git %s in %s: %w", strings.Join(args, " "), repoPath, err)
		}
		h.Write(out)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
