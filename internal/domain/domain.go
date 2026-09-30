// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

// Package domain holds the shared model: repos, root modules, workspaces and
// their statuses. It has no dependencies on other tfmux packages so every
// layer can import it.
package domain

import "strings"

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

	ManifestPath    string   // absolute path to the repo manifest; "" when the repo has none
	ManifestErr     string   // repo manifest parse failure; modules behave as without a manifest
	ManifestMissing []string // manifest entries whose root_module directory doesn't exist
}

// HasManifest reports whether the repo has a usable repo manifest.
func (r *Repo) HasManifest() bool { return r.ManifestPath != "" }

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

	InitErr string // output of the last terraform init, when it failed

	ManifestListed     bool              // true when the repo manifest lists this module
	ManifestWorkspaces []string          // this module's workspaces per the manifest, when ManifestListed
	Templates          *CommandTemplates // from the repo manifest; nil means the default terraform commands
}

// ManifestHidden reports whether the module should be hidden from the normal
// (non-showIgnored) view because its repo has a manifest that doesn't list
// it. A repo with no manifest hides nothing this way.
func (m *Module) ManifestHidden() bool {
	return m.Repo.HasManifest() && !m.ManifestListed
}

// Templated reports whether the module's commands come from manifest
// templates. For such a module a workspace is a logical name handed to the
// templates as TFMUX_WORKSPACE, not a terraform workspace.
func (m *Module) Templated() bool { return m.Templates != nil }

// WorkspaceDependentInit reports whether the module directory can be
// initialised for only one workspace at a time.
func (m *Module) WorkspaceDependentInit() bool { return m.Templates.InitUsesWorkspace() }

// CommandTemplates are a manifest entry's optional shell snippets for init,
// plan and apply. An empty field means the default command for that verb.
type CommandTemplates struct {
	Init  string
	Plan  string
	Apply string
}

// InitUsesWorkspace reports whether the init template expands
// TFMUX_WORKSPACE, so that running it for one workspace invalidates the
// module directory for every other. Nil-safe.
func (c *CommandTemplates) InitUsesWorkspace() bool {
	return c != nil && (strings.Contains(c.Init, "$TFMUX_WORKSPACE") ||
		strings.Contains(c.Init, "${TFMUX_WORKSPACE"))
}

// Workspace is one Terraform workspace of a root module.
type Workspace struct {
	Module *Module
	Name   string
}

// Key returns a stable identifier for a module's workspace, used for run
// state lookups and UI bookkeeping.
func (w *Workspace) Key() string { return w.Module.Path + "//" + w.Name }
