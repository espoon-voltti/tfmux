// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package paths

import (
	"path/filepath"
	"testing"
)

func TestConfigDirHonorsXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg/config")
	dir, err := ConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join("/xdg/config", "tfmux") {
		t.Errorf("ConfigDir = %q", dir)
	}
}

func TestConfigDirFallback(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/test")
	dir, err := ConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if dir != "/home/test/.config/tfmux" {
		t.Errorf("ConfigDir = %q", dir)
	}
}

func TestStateDirHonorsXDGAndCreates(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	dir, err := StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(base, "tfmux") {
		t.Errorf("StateDir = %q", dir)
	}
}

func TestExpandHome(t *testing.T) {
	t.Setenv("HOME", "/home/test")
	for in, want := range map[string]string{
		"~":          "/home/test",
		"~/x/y":      "/home/test/x/y",
		"/abs/path":  "/abs/path",
		"rel/path":   "rel/path",
		"~user/path": "~user/path", // ~user expansion unsupported, passed through
	} {
		got, err := ExpandHome(in)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("ExpandHome(%q) = %q, want %q", in, got, want)
		}
	}
}
