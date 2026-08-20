// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package tfexec

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/zclconf/go-cty/cty"
)

// PluginCacheDirConfigured reports whether terraform or tofu's CLI config
// enables a provider plugin cache (plugin_cache_dir), which serializes
// terraform init: concurrent inits against the same cache directory can
// corrupt it.
//
// A CLI config file that exists but can't be read or parsed (e.g. one
// written in JSON, which this HCL-only check can't handle) is treated as
// configured — failing closed is safer than silently allowing concurrent
// inits against a cache we couldn't confirm the absence of. A missing file
// is unambiguous and reports false.
func PluginCacheDirConfigured() bool {
	if os.Getenv("TF_PLUGIN_CACHE_DIR") != "" {
		return true
	}
	for _, path := range cliConfigCandidates() {
		if configFileHasPluginCache(path) {
			return true
		}
	}
	return false
}

// cliConfigCandidates returns the CLI config file paths terraform and tofu
// each resolve, honoring their env var overrides and falling back to their
// per-tool default location otherwise.
func cliConfigCandidates() []string {
	var candidates []string
	if p := os.Getenv("TF_CLI_CONFIG_FILE"); p != "" {
		candidates = append(candidates, p)
	} else if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, defaultTerraformRC(home))
	}
	if p := os.Getenv("OPENTOFU_CLI_CONFIG_FILE"); p != "" {
		candidates = append(candidates, p)
	} else if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".tofurc"))
	}
	return candidates
}

func defaultTerraformRC(home string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("APPDATA"), "terraform.rc")
	}
	return filepath.Join(home, ".terraformrc")
}

// configFileHasPluginCache reports whether path's CLI config sets a
// non-empty plugin_cache_dir. See PluginCacheDirConfigured for the fail-open
// (missing file) vs fail-closed (unreadable/unparseable file) rationale.
func configFileHasPluginCache(path string) bool {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		return true
	}
	f, diags := hclparse.NewParser().ParseHCL(data, path)
	if diags.HasErrors() || f == nil || f.Body == nil {
		return true
	}
	content, _, diags := f.Body.PartialContent(&hcl.BodySchema{
		Attributes: []hcl.AttributeSchema{{Name: "plugin_cache_dir"}},
	})
	if diags.HasErrors() || content == nil {
		return true
	}
	attr, ok := content.Attributes["plugin_cache_dir"]
	if !ok {
		return false
	}
	val, diags := attr.Expr.Value(nil)
	if diags.HasErrors() || val.IsNull() || val.Type() != cty.String {
		return true
	}
	return val.AsString() != ""
}
