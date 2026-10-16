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

func TestPluginCacheDirConfigured(t *testing.T) {
	const cache = `plugin_cache_dir = "/some/cache"`
	cases := []struct {
		name  string
		setup func(t *testing.T, home string)
		want  bool
	}{
		// No file at all is the common case and must never default to
		// throttling everyone.
		{"nothing set", func(*testing.T, string) {}, false},
		{"TF_PLUGIN_CACHE_DIR", func(t *testing.T, _ string) { t.Setenv("TF_PLUGIN_CACHE_DIR", "/some/cache") }, true},
		{"~/.terraformrc sets it", func(t *testing.T, home string) { writeFile(t, filepath.Join(home, ".terraformrc"), cache) }, true},
		{"~/.terraformrc without it", func(t *testing.T, home string) {
			writeFile(t, filepath.Join(home, ".terraformrc"), `disable_checkpoint = true`)
		}, false},
		{"TF_CLI_CONFIG_FILE", func(t *testing.T, home string) {
			path := filepath.Join(home, "custom.tfrc")
			writeFile(t, path, cache)
			t.Setenv("TF_CLI_CONFIG_FILE", path)
		}, true},
		{"~/.tofurc sets it", func(t *testing.T, home string) { writeFile(t, filepath.Join(home, ".tofurc"), cache) }, true},
		// Terraform also accepts a JSON-format CLI config file, which the
		// HCL-only parser rejects: fail closed rather than report "not
		// configured".
		{"unparseable file fails closed", func(t *testing.T, home string) {
			writeFile(t, filepath.Join(home, ".terraformrc"), `{"plugin_cache_dir": "/some/cache"}`)
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := isolateEnv(t)
			c.setup(t, home)
			if got := PluginCacheDirConfigured(); got != c.want {
				t.Errorf("PluginCacheDirConfigured() = %v, want %v", got, c.want)
			}
		})
	}
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
