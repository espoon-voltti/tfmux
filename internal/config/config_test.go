// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadFull(t *testing.T) {
	t.Setenv("HOME", "/home/test")
	path := writeConfig(t, `
roots = ["~/work/iac", "/abs/other"]
parallelism = 8
terraform_bin = "tofu"
tmux_session = "infra"
plan_ttl = "2h30m"
init_parallelism = 2

[mcp]
tools = "all"
listen = "127.0.0.1:9000"

[repos."~/work/iac/legacy"]
terraform_bin = "terraform-0.13"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Roots[0], "/home/test/work/iac"; got != want {
		t.Errorf("root[0] = %q, want %q", got, want)
	}
	if cfg.Roots[1] != "/abs/other" {
		t.Errorf("root[1] = %q", cfg.Roots[1])
	}
	if cfg.Parallelism != 8 || cfg.TerraformBin != "tofu" || cfg.TmuxSession != "infra" {
		t.Errorf("unexpected scalars: %+v", cfg)
	}
	if cfg.PlanTTLDuration() != 2*time.Hour+30*time.Minute {
		t.Errorf("plan_ttl = %v", cfg.PlanTTLDuration())
	}
	if cfg.InitParallelism != 2 {
		t.Errorf("init_parallelism = %d", cfg.InitParallelism)
	}
	if !cfg.MCP.AllTools() || cfg.MCP.Listen != "127.0.0.1:9000" {
		t.Errorf("mcp = %+v", cfg.MCP)
	}
	if got := cfg.BinFor("/home/test/work/iac/legacy"); got != "terraform-0.13" {
		t.Errorf("BinFor(legacy) = %q", got)
	}
	if got := cfg.BinFor("/home/test/work/iac/modern"); got != "tofu" {
		t.Errorf("BinFor(modern) = %q", got)
	}
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if cfg == nil || cfg.Parallelism != 4 || cfg.TerraformBin != "terraform" {
		t.Errorf("defaults not returned: %+v", cfg)
	}
	if cfg.PlanTTLDuration() != 24*time.Hour {
		t.Errorf("default plan_ttl = %v", cfg.PlanTTLDuration())
	}
	if cfg.InitParallelism != 0 || cfg.MCP.Listen != "127.0.0.1:7411" || cfg.MCP.AllTools() {
		t.Errorf("defaults: init_parallelism=%d mcp=%+v", cfg.InitParallelism, cfg.MCP)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	path := writeConfig(t, `paralellism = 3`) // typo must be caught
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	for name, content := range map[string]string{
		"zero parallelism":          `parallelism = 0`,
		"empty bin":                 `terraform_bin = ""`,
		"bad ttl":                   `plan_ttl = "soon"`,
		"negative init_parallelism": `init_parallelism = -1`,
		"unknown mcp.tools":         "[mcp]\ntools = 'some'",
		"empty mcp.listen":          "[mcp]\nlisten = ''",
	} {
		if _, err := Load(writeConfig(t, content)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

// Detection itself is tfexec's; this covers only how the override and the
// detected value combine.
func TestEffectiveInitParallelism(t *testing.T) {
	t.Setenv("TF_PLUGIN_CACHE_DIR", "/tmp/cache")
	cfg := Default()
	if got := cfg.EffectiveInitParallelism(); got != 1 {
		t.Errorf("a plugin cache should cap inits at 1, got %d", got)
	}
	cfg.InitParallelism = 3
	if got := cfg.EffectiveInitParallelism(); got != 3 {
		t.Errorf("an explicit value should win, got %d", got)
	}
}

func TestCreateWritesLoadableConfig(t *testing.T) {
	t.Setenv("HOME", "/home/test")
	// The config directory doesn't exist yet; Create makes it.
	path := filepath.Join(t.TempDir(), "tfmux", "config.toml")
	if err := Create(path, []string{"~/infra", `/odd "quoted" path`}); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/home/test/infra", `/odd "quoted" path`}
	if len(cfg.Roots) != len(want) || cfg.Roots[0] != want[0] || cfg.Roots[1] != want[1] {
		t.Errorf("Roots = %q, want %q", cfg.Roots, want)
	}
	if def := Default(); cfg.Parallelism != def.Parallelism || cfg.TerraformBin != def.TerraformBin {
		t.Errorf("non-root settings should keep their defaults, got %+v", cfg)
	}
}

func TestCreateRefusesExistingFile(t *testing.T) {
	path := writeConfig(t, "parallelism = 2\n")
	err := Create(path, []string{"/somewhere"})
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("err = %v, want os.ErrExist", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "parallelism = 2\n" {
		t.Errorf("existing file was modified: %q", got)
	}
}
