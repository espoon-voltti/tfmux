// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

// Package config loads and validates tfmux's TOML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/espoon-voltti/tfmux/internal/paths"
	"github.com/espoon-voltti/tfmux/internal/tfexec"
)

// RepoConfig holds per-repo overrides, keyed by repo path in the TOML.
type RepoConfig struct {
	TerraformBin string `toml:"terraform_bin"`
}

// Config is the user-facing configuration. The ignore list deliberately does
// not live here: config.toml is human-owned and never rewritten by tfmux.
type Config struct {
	Roots        []string              `toml:"roots"`
	Parallelism  int                   `toml:"parallelism"`
	TerraformBin string                `toml:"terraform_bin"`
	TmuxSession  string                `toml:"tmux_session"`
	PlanTTL      duration              `toml:"plan_ttl"`
	Repos        map[string]RepoConfig `toml:"repos"`

	// InitParallelism caps concurrent `terraform init` invocations across all
	// modules. 0 (the default) auto-detects: capped to 1 when a Terraform or
	// OpenTofu plugin cache is configured (see tfexec.PluginCacheDirConfigured),
	// unlimited otherwise. A positive value overrides detection.
	InitParallelism int `toml:"init_parallelism"`
}

// duration wraps time.Duration so TOML strings like "24h" round-trip.
type duration time.Duration

func (d *duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = duration(v)
	return nil
}

func (d duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

// PlanTTLDuration returns the plan TTL as a time.Duration.
func (c *Config) PlanTTLDuration() time.Duration { return time.Duration(c.PlanTTL) }

// EffectiveInitParallelism resolves the concurrent-init cap: an explicit
// InitParallelism always wins, otherwise it's auto-detected from the
// terraform/tofu CLI config. 0 means unlimited.
func (c *Config) EffectiveInitParallelism() int {
	if c.InitParallelism > 0 {
		return c.InitParallelism
	}
	if tfexec.PluginCacheDirConfigured() {
		return 1
	}
	return 0
}

// Default returns the configuration used when no config file exists.
func Default() *Config {
	return &Config{
		Parallelism:  4,
		TerraformBin: "terraform",
		TmuxSession:  "tfmux",
		PlanTTL:      duration(24 * time.Hour),
	}
}

// Load reads the config file at path. A missing file yields Default() with
// ErrNotFound so callers can distinguish "no config yet" from a broken one.
var ErrNotFound = errors.New("config file not found")

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), fmt.Errorf("%w: %s", ErrNotFound, path)
	}
	if err != nil {
		return nil, err
	}
	cfg := Default()
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.normalize(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// LoadDefault loads from the XDG config location.
func LoadDefault() (*Config, error) {
	path, err := paths.ConfigFile()
	if err != nil {
		return nil, err
	}
	return Load(path)
}

// normalize expands ~ in paths and validates values.
func (c *Config) normalize() error {
	if c.Parallelism < 1 {
		return fmt.Errorf("parallelism must be >= 1, got %d", c.Parallelism)
	}
	if c.TerraformBin == "" {
		return errors.New("terraform_bin must not be empty")
	}
	if c.PlanTTLDuration() <= 0 {
		return errors.New("plan_ttl must be positive")
	}
	if c.InitParallelism < 0 {
		return fmt.Errorf("init_parallelism must be >= 0, got %d", c.InitParallelism)
	}
	for i, r := range c.Roots {
		expanded, err := paths.ExpandHome(r)
		if err != nil {
			return err
		}
		c.Roots[i] = filepath.Clean(expanded)
	}
	repos := make(map[string]RepoConfig, len(c.Repos))
	for k, v := range c.Repos {
		expanded, err := paths.ExpandHome(k)
		if err != nil {
			return err
		}
		repos[filepath.Clean(expanded)] = v
	}
	c.Repos = repos
	return nil
}

// BinFor resolves the terraform binary for a repo path: per-repo override,
// then the global default.
func (c *Config) BinFor(repoPath string) string {
	if rc, ok := c.Repos[filepath.Clean(repoPath)]; ok && rc.TerraformBin != "" {
		return rc.TerraformBin
	}
	return c.TerraformBin
}
