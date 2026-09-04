// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAbsentFile(t *testing.T) {
	m, err := Load(t.TempDir())
	if err != nil || m != nil {
		t.Fatalf("Load() = %v, %v; want nil, nil", m, err)
	}
}

func TestLoadValid(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `[
	  {"_comment": "sweep order matters", "root_module": "terraform/base", "workspaces": ["staging", "prod"]},
	  {"root_module": "terraform/shared", "workspaces": ["default"], "unknown_field": "tolerated"}
	]`)
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(m.Entries))
	}
	if ws, ok := m.Workspaces("terraform/base"); !ok || len(ws) != 2 {
		t.Errorf("Workspaces(base) = %v, %v", ws, ok)
	}
	if ws, ok := m.Workspaces("terraform/shared"); !ok || ws[0] != "default" {
		t.Errorf("Workspaces(shared) = %v, %v", ws, ok)
	}
	if _, ok := m.Workspaces("terraform/nope"); ok {
		t.Error("unlisted module reported as listed")
	}
}

func TestLoadErrors(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"bad JSON", `not json`},
		{"non-array", `{"root_module": "x", "workspaces": ["a"]}`},
		{"empty root_module", `[{"root_module": "", "workspaces": ["a"]}]`},
		{"duplicate root_module", `[{"root_module": "terraform/base", "workspaces": ["a"]}, {"root_module": "terraform/base", "workspaces": ["b"]}]`},
		{"absolute path", `[{"root_module": "/etc/passwd", "workspaces": ["a"]}]`},
		{"parent traversal", `[{"root_module": "../outside", "workspaces": ["a"]}]`},
		{"empty workspaces", `[{"root_module": "terraform/base", "workspaces": []}]`},
		{"empty workspace name", `[{"root_module": "terraform/base", "workspaces": [""]}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, tc.content)
			if _, err := Load(dir); err == nil {
				t.Error("Load() = nil error, want failure")
			}
		})
	}
}
