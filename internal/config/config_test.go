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
	} {
		if _, err := Load(writeConfig(t, content)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestInitParallelismDefaultsToZero(t *testing.T) {
	cfg, err := Load(writeConfig(t, `parallelism = 4`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InitParallelism != 0 {
		t.Errorf("InitParallelism = %d, want 0", cfg.InitParallelism)
	}
}

func TestLoadInitParallelismOverride(t *testing.T) {
	cfg, err := Load(writeConfig(t, `init_parallelism = 2`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InitParallelism != 2 {
		t.Errorf("InitParallelism = %d, want 2", cfg.InitParallelism)
	}
}

// isolateEffectiveInitParallelismEnv clears every env var
// PluginCacheDirConfigured consults and points HOME/APPDATA at an empty temp
// dir, so the test doesn't pick up whatever the machine running it happens to
// have configured for real.
func isolateEffectiveInitParallelismEnv(t *testing.T) {
	t.Helper()
	empty := t.TempDir()
	t.Setenv("HOME", empty)
	t.Setenv("APPDATA", empty)
	for _, k := range []string{"TF_PLUGIN_CACHE_DIR", "TF_CLI_CONFIG_FILE", "OPENTOFU_CLI_CONFIG_FILE"} {
		t.Setenv(k, "")
	}
}

func TestEffectiveInitParallelism(t *testing.T) {
	t.Run("explicit override wins even with a plugin cache configured", func(t *testing.T) {
		isolateEffectiveInitParallelismEnv(t)
		t.Setenv("TF_PLUGIN_CACHE_DIR", "/tmp/cache")
		cfg := Default()
		cfg.InitParallelism = 3
		if got := cfg.EffectiveInitParallelism(); got != 3 {
			t.Errorf("got %d, want 3", got)
		}
	})
	t.Run("auto-detects a plugin cache from the env var", func(t *testing.T) {
		isolateEffectiveInitParallelismEnv(t)
		t.Setenv("TF_PLUGIN_CACHE_DIR", "/tmp/cache")
		cfg := Default()
		if got := cfg.EffectiveInitParallelism(); got != 1 {
			t.Errorf("got %d, want 1", got)
		}
	})
	t.Run("unlimited when nothing is configured", func(t *testing.T) {
		isolateEffectiveInitParallelismEnv(t)
		cfg := Default()
		if got := cfg.EffectiveInitParallelism(); got != 0 {
			t.Errorf("got %d, want 0", got)
		}
	})
}
