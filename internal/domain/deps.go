// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package domain

import (
	"path/filepath"
	"sort"
)

// Deps is what a root module's configuration depends on, found by statically
// reading its .tf files (never by terraform init): the modules it calls and
// the remote states it reads.
type Deps struct {
	Local        []LocalDep       `json:"local,omitempty"`         // ./ and ../ module sources, transitively
	Git          []GitRef         `json:"git,omitempty"`           // git module sources, transitively through local modules
	RemoteStates []RemoteStateRef `json:"remote_states,omitempty"` // data "terraform_remote_state" blocks of the root
	Backend      *StateAddress    `json:"backend,omitempty"`       // the root's own state, when its backend config is literal
	Errors       []string         `json:"errors,omitempty"`        // parse problems; never fatal
}

// LocalDep is a module called by a relative path.
type LocalDep struct {
	Dir     string `json:"dir"`                // absolute
	RelPath string `json:"rel_path,omitempty"` // repo-relative, when InRepo
	InRepo  bool   `json:"in_repo"`
	Via     string `json:"via"` // module call path, e.g. "base.vpc"
}

// GitRef is a module called from a git repository.
type GitRef struct {
	Raw    string `json:"raw"`
	Repo   string `json:"repo"`             // normalized "host/org/name"
	Subdir string `json:"subdir,omitempty"` // path inside the repo, no leading or trailing slash
	Ref    string `json:"ref,omitempty"`    // the ?ref= value; "" when unpinned
	Via    string `json:"via"`              // module call path, e.g. "base.vpc"
}

// Key identifies the module inside its repo, independent of ref and URL
// form: "host/org/name//subdir".
func (g GitRef) Key() string {
	if g.Subdir == "" {
		return g.Repo
	}
	return g.Repo + "//" + g.Subdir
}

// StateAddress locates one terraform state in a backend.
type StateAddress struct {
	Backend string `json:"backend"`
	Bucket  string `json:"bucket,omitempty"`
	Key     string `json:"key,omitempty"`
	Region  string `json:"region,omitempty"`
}

// SameState reports whether two addresses name the same state: same backend,
// bucket and key, and the regions agree when both are known.
func (a StateAddress) SameState(b StateAddress) bool {
	if a.Backend != b.Backend || a.Bucket != b.Bucket || a.Key != b.Key {
		return false
	}
	return a.Region == "" || b.Region == "" || a.Region == b.Region
}

func (a StateAddress) String() string {
	s := a.Backend + "://" + a.Bucket
	if a.Key != "" {
		s += "/" + a.Key
	}
	return s
}

// WorkspaceExprKind classifies the workspace a terraform_remote_state reads.
type WorkspaceExprKind string

const (
	WorkspaceUnknown  WorkspaceExprKind = "unknown"  // depends on something other than terraform.workspace
	WorkspaceLiteral  WorkspaceExprKind = "literal"  // a constant; WorkspaceExpr holds it
	WorkspaceSame     WorkspaceExprKind = "same"     // terraform.workspace
	WorkspaceTemplate WorkspaceExprKind = "template" // a function of terraform.workspace only; WorkspaceExpr holds its source
)

// RemoteStateRef is one data "terraform_remote_state" block.
type RemoteStateRef struct {
	Name string `json:"name"`
	StateAddress
	WorkspaceKind WorkspaceExprKind `json:"workspace_kind"`
	WorkspaceExpr string            `json:"workspace_expr,omitempty"`
}

// FingerprintPaths lists the repo-relative paths whose content a plan of this
// module depends on: its own directory and every in-repo local module it
// calls. gitstatus.Fingerprint hashes them to detect that a saved plan is out
// of date.
func (m *Module) FingerprintPaths() []string {
	paths := []string{m.RelPath}
	if m.Deps != nil {
		for _, d := range m.Deps.Local {
			if d.InRepo {
				paths = append(paths, filepath.ToSlash(d.RelPath))
			}
		}
	}
	sort.Strings(paths)
	out := paths[:0]
	for i, p := range paths {
		if i == 0 || p != paths[i-1] {
			out = append(out, p)
		}
	}
	return out
}
