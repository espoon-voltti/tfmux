// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package gitstatus

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// TreeID returns the id of subdir's tree at rev ("" subdir means the repo
// root). found is false when rev or the path does not exist in it.
func TreeID(ctx context.Context, repoPath, rev, subdir string) (id string, found bool, err error) {
	spec := rev + ":"
	if subdir != "." && subdir != "" {
		spec += strings.TrimSuffix(subdir, "/")
	}
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "rev-parse", "--verify", "--quiet", spec).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("git rev-parse %s in %s: %w", spec, repoPath, err)
	}
	return strings.TrimSpace(string(out)), true, nil
}

// NewestTag returns the highest tag by version sort, or "" when the repo has
// no tags.
func NewestTag(ctx context.Context, repoPath string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "tag", "--sort=-v:refname").Output()
	if err != nil {
		return "", fmt.Errorf("git tag in %s: %w", repoPath, err)
	}
	first, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(first), nil
}

// HasChanges reports whether any of the repo-relative paths has uncommitted
// or untracked changes.
func HasChanges(ctx context.Context, repoPath string, relPaths []string) (bool, error) {
	args := append([]string{"-C", repoPath, "status", "--porcelain", "--"}, relPaths...)
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		return false, fmt.Errorf("git status in %s: %w", repoPath, err)
	}
	return len(strings.TrimSpace(string(out))) > 0, nil
}

// RemoteURL returns the URL of the origin remote, or "" when there is none.
func RemoteURL(ctx context.Context, repoPath string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
