// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

// Package manifest reads a repo's workspace manifest — the repo manifest,
// ".terraform-workspaces.json" at the repo root — which lists each root
// module tfmux should manage and the workspaces valid for it. Where a repo
// has one, it is authoritative: listed modules get their workspaces from the
// manifest rather than the backend, and modules absent from it are hidden.
//
// This is unrelated to tfmux's own per-module workspace enumeration cache
// (also named "workspaces.json", under the XDG state dir — see
// internal/state) despite the similar name; callers always say "the repo
// manifest" to keep the two apart.
package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// FileName is the repo manifest's name at the repo root.
const FileName = ".terraform-workspaces.json"

// Entry is one root module listed in the manifest.
type Entry struct {
	Comment    string   `json:"_comment,omitempty"`
	RootModule string   `json:"root_module"` // repo-root-relative, slash-separated
	Workspaces []string `json:"workspaces"`
}

// Manifest is a repo's parsed, validated workspace manifest.
type Manifest struct {
	Path    string  // absolute path to the manifest file
	Entries []Entry // file order preserved

	byModule map[string][]string
}

// Load reads and validates repoPath's repo manifest. A missing file returns
// (nil, nil): the repo simply has no manifest. Any other failure — bad JSON,
// an invalid entry — rejects the whole manifest with a descriptive error;
// callers should treat that repo as having no usable manifest rather than
// applying it partially.
func Load(repoPath string) (*Manifest, error) {
	p := filepath.Join(repoPath, FileName)
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	byModule := make(map[string][]string, len(entries))
	for i := range entries {
		e := &entries[i]
		if e.RootModule == "" {
			return nil, fmt.Errorf("%s: entry %d: root_module is empty", FileName, i)
		}
		cleaned := path.Clean(filepath.ToSlash(e.RootModule))
		if path.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
			return nil, fmt.Errorf("%s: entry %d: root_module %q must be a relative path within the repo", FileName, i, e.RootModule)
		}
		e.RootModule = cleaned
		if _, dup := byModule[e.RootModule]; dup {
			return nil, fmt.Errorf("%s: duplicate root_module %q", FileName, e.RootModule)
		}
		if len(e.Workspaces) == 0 {
			return nil, fmt.Errorf("%s: root_module %q: workspaces must not be empty", FileName, e.RootModule)
		}
		for _, w := range e.Workspaces {
			if w == "" {
				return nil, fmt.Errorf("%s: root_module %q: workspace name must not be empty", FileName, e.RootModule)
			}
		}
		byModule[e.RootModule] = e.Workspaces
	}
	return &Manifest{Path: p, Entries: entries, byModule: byModule}, nil
}

// Workspaces returns the workspaces listed for relPath (a repo-root-relative,
// slash-separated root module path), and whether the manifest lists it at
// all.
func (m *Manifest) Workspaces(relPath string) ([]string, bool) {
	ws, ok := m.byModule[relPath]
	return ws, ok
}
