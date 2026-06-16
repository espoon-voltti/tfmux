// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

// Package paths resolves tfmux's config and state directories following the
// XDG base directory spec, with ~/.config and ~/.local/state fallbacks on
// every platform (including darwin, where Application Support is wrong for a
// CLI tool).
package paths

import (
	"os"
	"path/filepath"
)

const appDir = "tfmux"

// ConfigDir returns the directory holding config.toml.
func ConfigDir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, appDir), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", appDir), nil
}

// ConfigFile returns the path to config.toml.
func ConfigFile() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.toml"), nil
}

// StateDir returns the directory holding run records, plan files and the
// ignore list. It is created with 0700 if missing.
func StateDir() (string, error) {
	var dir string
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		dir = filepath.Join(xdg, appDir)
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".local", "state", appDir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// ExpandHome expands a leading ~ or ~/ in p to the user's home directory.
func ExpandHome(p string) (string, error) {
	if p == "~" {
		return os.UserHomeDir()
	}
	if len(p) >= 2 && p[0] == '~' && p[1] == filepath.Separator || len(p) >= 2 && p[:2] == "~/" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, p[2:]), nil
	}
	return p, nil
}
