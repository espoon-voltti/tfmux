// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package tfexec

import (
	"os"
	"path/filepath"
	"testing"
)

// isolateEnv clears every env var PluginCacheDirConfigured consults and
// points HOME/APPDATA at an empty temp dir, so a test starts from "nothing
// configured" regardless of what the machine running it actually has.
func isolateEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", home)
	for _, k := range []string{"TF_PLUGIN_CACHE_DIR", "TF_CLI_CONFIG_FILE", "OPENTOFU_CLI_CONFIG_FILE"} {
		t.Setenv(k, "")
	}
	return home
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPluginCacheDirConfiguredNothingSet(t *testing.T) {
	isolateEnv(t)
	if PluginCacheDirConfigured() {
		t.Error("expected false with no env vars or CLI config files present")
	}
}

func TestPluginCacheDirConfiguredEnvVar(t *testing.T) {
	isolateEnv(t)
	t.Setenv("TF_PLUGIN_CACHE_DIR", "/some/cache")
	if !PluginCacheDirConfigured() {
		t.Error("expected true when TF_PLUGIN_CACHE_DIR is set")
	}
}

func TestPluginCacheDirConfiguredInCLIConfigFile(t *testing.T) {
	home := isolateEnv(t)
	writeFile(t, filepath.Join(home, ".terraformrc"), `plugin_cache_dir = "/some/cache"`)
	if !PluginCacheDirConfigured() {
		t.Error("expected true when ~/.terraformrc sets plugin_cache_dir")
	}
}

func TestPluginCacheDirNotConfiguredInCLIConfigFile(t *testing.T) {
	home := isolateEnv(t)
	writeFile(t, filepath.Join(home, ".terraformrc"), `disable_checkpoint = true`)
	if PluginCacheDirConfigured() {
		t.Error("expected false when the CLI config file exists but doesn't set plugin_cache_dir")
	}
}

func TestPluginCacheDirConfiguredViaTFCLIConfigFileEnv(t *testing.T) {
	home := isolateEnv(t)
	path := filepath.Join(home, "custom.tfrc")
	writeFile(t, path, `plugin_cache_dir = "/some/cache"`)
	t.Setenv("TF_CLI_CONFIG_FILE", path)
	if !PluginCacheDirConfigured() {
		t.Error("expected true when TF_CLI_CONFIG_FILE points at a file setting plugin_cache_dir")
	}
}

func TestPluginCacheDirConfiguredInOpenTofuConfigFile(t *testing.T) {
	home := isolateEnv(t)
	writeFile(t, filepath.Join(home, ".tofurc"), `plugin_cache_dir = "/some/cache"`)
	if !PluginCacheDirConfigured() {
		t.Error("expected true when ~/.tofurc sets plugin_cache_dir")
	}
}

func TestPluginCacheDirFailsClosedOnUnparseableFile(t *testing.T) {
	home := isolateEnv(t)
	// Terraform also accepts a JSON-format CLI config file, which the
	// HCL-only parser here rejects — must fail closed, not silently report
	// "not configured".
	writeFile(t, filepath.Join(home, ".terraformrc"), `{"plugin_cache_dir": "/some/cache"}`)
	if !PluginCacheDirConfigured() {
		t.Error("expected true (fail closed) when the CLI config file exists but can't be parsed")
	}
}

func TestPluginCacheDirMissingFileIsUnambiguouslyFalse(t *testing.T) {
	isolateEnv(t)
	// No file at all is the common case and must never default to
	// throttling everyone.
	if PluginCacheDirConfigured() {
		t.Error("expected false when no CLI config file exists")
	}
}
