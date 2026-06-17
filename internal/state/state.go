// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

// Package state persists tfmux's machine-owned state under the XDG state
// dir: run records, saved plan files, and the ignore list.
//
// Layout:
//
//	$STATE/tfmux/
//	├── ignore.json
//	└── modules/<sha256(absModulePath)[:16]>/
//	    ├── module.json              back-reference for debugging/GC
//	    ├── lock                     flock'd to serialize work across instances
//	    ├── workspaces.json          cached workspace enumeration
//	    └── ws/<workspaceName>/
//	        ├── run.json             RunRecord
//	        ├── plan.tfplan          0600 — plan files embed secrets
//	        ├── plan.log             captured plan output
//	        └── apply.exit           written atomically by the tmux wrapper
//
// All directories are 0700 and files 0600: plan files contain resolved
// secret values and provider credentials.
package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ChangeSummary counts planned resource changes, as in
// "Plan: 2 to add, 1 to change, 0 to destroy."
type ChangeSummary struct {
	Add     int `json:"add"`
	Change  int `json:"change"`
	Destroy int `json:"destroy"`
}

func (s ChangeSummary) Any() bool { return s.Add+s.Change+s.Destroy > 0 }

func (s ChangeSummary) String() string {
	return fmt.Sprintf("+%d ~%d -%d", s.Add, s.Change, s.Destroy)
}

// ApplyRecord tracks one apply launched in tmux.
type ApplyRecord struct {
	Started  time.Time  `json:"started"`
	Finished *time.Time `json:"finished,omitempty"`
	ExitCode *int       `json:"exit_code,omitempty"` // nil while running/unknown
	WindowID string     `json:"window_id,omitempty"` // tmux @window_id
	Aborted  bool       `json:"aborted,omitempty"`   // window vanished without exit file
}

// RunRecord is the persisted outcome of the latest plan (and its apply) for
// one workspace.
type RunRecord struct {
	ModulePath string `json:"module_path"`
	Workspace  string `json:"workspace"`

	PlanStarted  time.Time `json:"plan_started"`
	PlanFinished time.Time `json:"plan_finished"`
	PlanExitCode int       `json:"plan_exit_code"` // 0 clean, 1 error, 2 changes

	Summary ChangeSummary `json:"summary"`

	GitHead      string `json:"git_head,omitempty"`
	DirtyHash    string `json:"dirty_hash,omitempty"`
	TFBinVersion string `json:"tf_bin_version,omitempty"`

	Apply *ApplyRecord `json:"apply,omitempty"`
}

// Store reads and writes tfmux state rooted at Dir.
type Store struct {
	Dir string
}

func New(dir string) *Store { return &Store{Dir: dir} }

func moduleHash(modulePath string) string {
	sum := sha256.Sum256([]byte(modulePath))
	return hex.EncodeToString(sum[:])[:16]
}

// ModuleDir returns (and creates) the state dir for a module, maintaining
// the module.json back-reference.
func (s *Store) ModuleDir(modulePath string) (string, error) {
	dir := filepath.Join(s.Dir, "modules", moduleHash(modulePath))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	ref := filepath.Join(dir, "module.json")
	if _, err := os.Stat(ref); errors.Is(err, os.ErrNotExist) {
		data, _ := json.Marshal(map[string]string{"path": modulePath})
		if err := os.WriteFile(ref, data, 0o600); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// WorkspaceDir returns (and creates) the state dir for one workspace.
func (s *Store) WorkspaceDir(modulePath, workspace string) (string, error) {
	mdir, err := s.ModuleDir(modulePath)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(mdir, "ws", workspace)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func (s *Store) PlanFilePath(modulePath, workspace string) (string, error) {
	dir, err := s.WorkspaceDir(modulePath, workspace)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "plan.tfplan"), nil
}

// ModuleLogPath returns the path for a module-level task log (e.g. "init",
// "enumerate"), creating the module dir.
func (s *Store) ModuleLogPath(modulePath, name string) (string, error) {
	dir, err := s.ModuleDir(modulePath)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".log"), nil
}

// ModuleLockPath returns the path of a module's cross-process lock file,
// creating the module dir. tfmux instances flock this to serialize terraform
// work in the module dir (any task can lazily `init` and mutate .terraform/),
// extending the runner's in-process per-module serialization across instances.
func (s *Store) ModuleLockPath(modulePath string) (string, error) {
	dir, err := s.ModuleDir(modulePath)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "lock"), nil
}

func (s *Store) PlanLogPath(modulePath, workspace string) (string, error) {
	dir, err := s.WorkspaceDir(modulePath, workspace)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "plan.log"), nil
}

func (s *Store) ApplyExitPath(modulePath, workspace string) (string, error) {
	dir, err := s.WorkspaceDir(modulePath, workspace)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "apply.exit"), nil
}

// SaveRun persists the record as run.json (0600, atomic rename).
func (s *Store) SaveRun(r *RunRecord) error {
	dir, err := s.WorkspaceDir(r.ModulePath, r.Workspace)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "run.json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "run.json"))
}

// LoadRun returns the stored record, or (nil, nil) when none exists.
func (s *Store) LoadRun(modulePath, workspace string) (*RunRecord, error) {
	dir := filepath.Join(s.Dir, "modules", moduleHash(modulePath), "ws", workspace)
	data, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r RunRecord
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("corrupt run.json for %s//%s: %w", modulePath, workspace, err)
	}
	return &r, nil
}

// WorkspaceCache is the persisted result of a workspace enumeration. Listing
// workspaces hits the backend (e.g. S3/DynamoDB) and is slow and rate-limited,
// so we cache it and re-enumerate only on explicit user refresh.
type WorkspaceCache struct {
	Workspaces   []string  `json:"workspaces"`
	EnumeratedAt time.Time `json:"enumerated_at"`
}

func (s *Store) workspacesPath(modulePath string) string {
	return filepath.Join(s.Dir, "modules", moduleHash(modulePath), "workspaces.json")
}

// SaveWorkspaces caches a module's enumerated workspace list (0600, atomic).
func (s *Store) SaveWorkspaces(modulePath string, names []string, at time.Time) error {
	dir, err := s.ModuleDir(modulePath)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(WorkspaceCache{Workspaces: names, EnumeratedAt: at}, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "workspaces.json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "workspaces.json"))
}

// LoadWorkspaces returns the cached workspace list, or (nil, false) when no
// cache exists.
func (s *Store) LoadWorkspaces(modulePath string) (*WorkspaceCache, bool) {
	data, err := os.ReadFile(s.workspacesPath(modulePath))
	if err != nil {
		return nil, false
	}
	var c WorkspaceCache
	if json.Unmarshal(data, &c) != nil {
		return nil, false
	}
	return &c, true
}

// HasPlanFile reports whether a saved (non-expired, non-discarded) plan file
// exists for the workspace.
func (s *Store) HasPlanFile(modulePath, workspace string) bool {
	path := filepath.Join(s.Dir, "modules", moduleHash(modulePath), "ws", workspace, "plan.tfplan")
	_, err := os.Stat(path)
	return err == nil
}

// DiscardPlan removes the saved plan file (manual discard, post-apply
// cleanup, TTL expiry). Missing file is not an error.
func (s *Store) DiscardPlan(modulePath, workspace string) error {
	path := filepath.Join(s.Dir, "modules", moduleHash(modulePath), "ws", workspace, "plan.tfplan")
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// ExpirePlans deletes every plan.tfplan older than ttl and returns how many
// were removed. Plan files embed secrets; bounded residency is the point.
func (s *Store) ExpirePlans(ttl time.Duration) (int, error) {
	pattern := filepath.Join(s.Dir, "modules", "*", "ws", "*", "plan.tfplan")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-ttl)
	removed := 0
	for _, path := range matches {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if os.Remove(path) == nil {
				removed++
			}
		}
	}
	return removed, nil
}

// GC removes state for modules whose directory no longer exists on disk
// (deleted repos, moved modules). Returns the number of pruned module dirs.
func (s *Store) GC() (int, error) {
	matches, err := filepath.Glob(filepath.Join(s.Dir, "modules", "*", "module.json"))
	if err != nil {
		return 0, err
	}
	pruned := 0
	for _, ref := range matches {
		data, err := os.ReadFile(ref)
		if err != nil {
			continue
		}
		var meta struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(data, &meta) != nil || meta.Path == "" {
			continue
		}
		if _, err := os.Stat(meta.Path); errors.Is(err, os.ErrNotExist) {
			if os.RemoveAll(filepath.Dir(ref)) == nil {
				pruned++
			}
		}
	}
	return pruned, nil
}

// --- ignore list ---

// Ignore is a persisted set of ignored item keys: repo paths, module paths,
// or workspace keys (modulePath + "//" + workspace).
type Ignore map[string]bool

func (s *Store) ignorePath() string { return filepath.Join(s.Dir, "ignore.json") }

func (s *Store) LoadIgnore() (Ignore, error) {
	data, err := os.ReadFile(s.ignorePath())
	if errors.Is(err, os.ErrNotExist) {
		return Ignore{}, nil
	}
	if err != nil {
		return nil, err
	}
	var keys []string
	if err := json.Unmarshal(data, &keys); err != nil {
		return nil, fmt.Errorf("corrupt ignore.json: %w", err)
	}
	ig := make(Ignore, len(keys))
	for _, k := range keys {
		ig[k] = true
	}
	return ig, nil
}

func (s *Store) SaveIgnore(ig Ignore) error {
	keys := make([]string, 0, len(ig))
	for k, v := range ig {
		if v {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	data, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	tmp := s.ignorePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.ignorePath())
}
