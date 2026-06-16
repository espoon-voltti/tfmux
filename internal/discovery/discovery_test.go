// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package discovery

import (
	"os"
	"path/filepath"
	"testing"
)

// mkdir creates dir (and parents) under root and returns its path.
func mkdir(t *testing.T, root string, parts ...string) string {
	t.Helper()
	dir := filepath.Join(append([]string{root}, parts...)...)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const backendModule = `
terraform {
  required_version = ">= 1.5"
  backend "s3" {
    bucket = "state"
  }
}
`

const cloudModule = `
terraform {
  cloud {
    organization = "acme"
  }
}
`

const localStateModule = `
terraform {
  required_version = ">= 1.5"
}
provider "aws" {
  region = "eu-north-1"
}
`

const childModule = `
variable "name" { type = string }
resource "null_resource" "x" {}
`

func TestIsRootModule(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{"backend block", map[string]string{"main.tf": backendModule}, true},
		{"cloud block", map[string]string{"main.tf": cloudModule}, true},
		{"terraform + provider across files", map[string]string{"versions.tf": "terraform {}", "providers.tf": `provider "aws" {}`}, true},
		{"local-state module", map[string]string{"main.tf": localStateModule}, true},
		{"child module", map[string]string{"main.tf": childModule}, false},
		{"no tf files", map[string]string{"README.md": "hi"}, false},
		{"terraform block only", map[string]string{"versions.tf": "terraform {}"}, false},
		{"syntax error tolerated", map[string]string{"broken.tf": "terraform {", "main.tf": backendModule}, true},
		{"backend in comment ignored", map[string]string{"main.tf": "# terraform { backend \"s3\" {} }\nresource \"null_resource\" \"x\" {}"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range tc.files {
				write(t, dir, name, content)
			}
			if got := IsRootModule(dir); got != tc.want {
				t.Errorf("IsRootModule = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDiscover(t *testing.T) {
	root := t.TempDir()

	// repo1: module at repo root + nested module + a child module dir
	repo1 := mkdir(t, root, "repo1")
	mkdir(t, repo1, ".git")
	write(t, repo1, "main.tf", backendModule)
	nested := mkdir(t, repo1, "envs", "prod")
	write(t, nested, "main.tf", backendModule)
	child := mkdir(t, repo1, "modules", "vpc")
	write(t, child, "main.tf", childModule)
	// .terraform dirs must be skipped even if they contain tf files
	tfdir := mkdir(t, repo1, "envs", "prod", ".terraform", "modules", "x")
	write(t, tfdir, "main.tf", backendModule)

	// repo2: no modules at all
	repo2 := mkdir(t, root, "repo2")
	mkdir(t, repo2, ".git")
	write(t, repo2, "README.md", "docs only")

	// not a repo: has a module but no .git — must not be reported
	stray := mkdir(t, root, "stray")
	write(t, stray, "main.tf", backendModule)

	repos, err := Discover([]string{root, filepath.Join(root, "does-not-exist")})
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 {
		t.Fatalf("got %d repos, want 2: %+v", len(repos), repos)
	}
	r1 := repos[0]
	if r1.Path != repo1 {
		t.Errorf("repos[0] = %s", r1.Path)
	}
	if len(r1.Modules) != 2 {
		t.Fatalf("repo1 modules = %d, want 2", len(r1.Modules))
	}
	if r1.Modules[0].RelPath != "." || r1.Modules[1].RelPath != filepath.Join("envs", "prod") {
		t.Errorf("module relpaths = %q, %q", r1.Modules[0].RelPath, r1.Modules[1].RelPath)
	}
	if len(repos[1].Modules) != 0 {
		t.Errorf("repo2 should have no modules")
	}
}

func TestDiscoverRootIsRepo(t *testing.T) {
	root := t.TempDir()
	mkdir(t, root, ".git")
	write(t, root, "main.tf", backendModule)
	repos, err := Discover([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || len(repos[0].Modules) != 1 {
		t.Fatalf("repos = %+v", repos)
	}
}
