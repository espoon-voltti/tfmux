// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

// Package domain holds the shared model: repos, root modules, workspaces and
// their statuses. It has no dependencies on other tfmux packages so every
// layer can import it.
package domain

// GitStatus is a snapshot of a repo's working tree, from
// `git status --porcelain=v2 --branch`.
type GitStatus struct {
	Branch      string // empty when detached
	OID         string // HEAD commit, "(initial)" before first commit
	Detached    bool
	Dirty       bool // any staged, unstaged or untracked entries
	Ahead       int
	Behind      int
	HasUpstream bool
	Err         error // git invocation/parse failure; other fields zero
}

// Repo is a git repository discovered under one of the configured roots.
type Repo struct {
	Path    string // absolute
	Name    string // base name for display
	Git     GitStatus
	Modules []*Module
}

// WorkspaceState is a module's last enumeration outcome. In-progress
// enumeration (queued/running) is tracked as a task, not here.
type WorkspaceState int

const (
	WorkspacesUnknown WorkspaceState = iota // not yet enumerated
	WorkspacesReady
	WorkspacesError
)

// Module is a Terraform root module: a directory with .tf files declaring a
// backend (or cloud block, or providers as a local-state fallback).
type Module struct {
	Repo    *Repo
	Path    string // absolute
	RelPath string // relative to repo root, "." for repo-root modules

	TFBin string // resolved terraform binary for this module

	WorkspaceState WorkspaceState
	WorkspaceErr   string // populated when WorkspacesError
	Workspaces     []*Workspace
}

// Workspace is one Terraform workspace of a root module.
type Workspace struct {
	Module *Module
	Name   string
}

// Key returns a stable identifier for a module's workspace, used for run
// state lookups and UI bookkeeping.
func (w *Workspace) Key() string { return w.Module.Path + "//" + w.Name }
