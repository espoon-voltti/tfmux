// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/espoon-voltti/tfmux/internal/config"
)

// initEnv points HOME and XDG_CONFIG_HOME at a fresh temp dir, chdirs into
// home/infra, and returns home and the config file path. Symlinks in the
// temp dir are resolved so the working directory reported by os.Getwd (e.g.
// /private/var on macOS) is comparable with HOME.
func initEnv(t *testing.T) (home, configPath string) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	infra := filepath.Join(home, "infra")
	if err := os.Mkdir(infra, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(infra)
	return home, filepath.Join(home, ".config", "tfmux", "config.toml")
}

func loadRoots(t *testing.T, path string) []string {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Roots
}

func TestInitDefaultsToWorkingDirectory(t *testing.T) {
	home, path := initEnv(t)
	var out strings.Builder
	if err := runInit(strings.NewReader("\n"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "[~/infra]") {
		t.Errorf("prompt should offer the working directory as default, got %q", out.String())
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `'~/infra'`) && !strings.Contains(string(raw), `"~/infra"`) {
		t.Errorf("root under HOME should be written as ~/infra, got:\n%s", raw)
	}
	if roots := loadRoots(t, path); len(roots) != 1 || roots[0] != filepath.Join(home, "infra") {
		t.Errorf("roots = %q", roots)
	}
}

func TestInitDefaultsOnEOFWithoutAnswer(t *testing.T) {
	home, path := initEnv(t)
	if err := runInit(strings.NewReader(""), &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if roots := loadRoots(t, path); len(roots) != 1 || roots[0] != filepath.Join(home, "infra") {
		t.Errorf("roots = %q", roots)
	}
}

func TestInitUsesAnsweredDirectory(t *testing.T) {
	home, path := initEnv(t)
	if err := os.Mkdir(filepath.Join(home, "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A relative answer resolves against the working directory (home/infra).
	if err := runInit(strings.NewReader("  ../other  \n"), &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if roots := loadRoots(t, path); len(roots) != 1 || roots[0] != filepath.Join(home, "other") {
		t.Errorf("roots = %q", roots)
	}
}

func TestInitRejectsMissingDirectory(t *testing.T) {
	_, path := initEnv(t)
	if err := runInit(strings.NewReader("~/nope\n"), &strings.Builder{}); err == nil {
		t.Fatal("expected an error for a nonexistent directory")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("no config file should be written, stat err = %v", err)
	}
}

func TestInitRefusesExistingConfig(t *testing.T) {
	_, path := initEnv(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("parallelism = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The refusal must come before the prompt, so a reader that fails the test
	// if read stands in for stdin.
	err := runInit(failingReader{t}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v, want a refusal naming %s", err, path)
	}
	if got, _ := os.ReadFile(path); string(got) != "parallelism = 2\n" {
		t.Errorf("existing config was modified: %q", got)
	}
}

type failingReader struct{ t *testing.T }

func (r failingReader) Read([]byte) (int, error) {
	r.t.Error("stdin was read despite an existing config")
	return 0, os.ErrClosed
}
