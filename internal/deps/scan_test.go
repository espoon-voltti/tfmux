// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package deps

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/espoon-voltti/tfmux/internal/domain"
)

func TestParseGitSource(t *testing.T) {
	cases := []struct {
		src               string
		repo, subdir, ref string
	}{
		{"git@github.com:espoon-voltti/voltti-infra.git//terraform/modules/s3/?depth=1&ref=v6.20.0", "github.com/espoon-voltti/voltti-infra", "terraform/modules/s3", "v6.20.0"},
		{"git::https://github.com/espoon-voltti/evaka-infra-shared.git//modules/waf?depth=1&ref=v16", "github.com/espoon-voltti/evaka-infra-shared", "modules/waf", "v16"},
		{"git::ssh://git@github.com/espoon-voltti/voltti-infra.git//terraform/modules/watchers?depth=1&ref=v6.20.0", "github.com/espoon-voltti/voltti-infra", "terraform/modules/watchers", "v6.20.0"},
		{"github.com/org/repo//sub?ref=main", "github.com/org/repo", "sub", "main"},
		{"git::https://example.com/org/repo.git", "example.com/org/repo", "", ""},
	}
	for _, c := range cases {
		got, ok := ParseGitSource(c.src)
		if !ok {
			t.Errorf("%s: not recognized as git", c.src)
			continue
		}
		if got.Repo != c.repo || got.Subdir != c.subdir || got.Ref != c.ref {
			t.Errorf("%s: got %+v", c.src, got)
		}
	}
	for _, src := range []string{"./modules/x", "../x", "hashicorp/consul/aws", "terraform-aws-modules/vpc/aws", "s3::https://s3.amazonaws.com/b/k.zip"} {
		if _, ok := ParseGitSource(src); ok {
			t.Errorf("%s: should not be git", src)
		}
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanRootModule(t *testing.T) {
	repo := t.TempDir()
	root := filepath.Join(repo, "terraform/service")
	write(t, filepath.Join(root, "main.tf"), `
terraform {
  backend "s3" {
    bucket = "my-terraform"
    key    = "service.tfstate"
    region = "eu-north-1"
  }
}
module "s3" {
  source = "git@github.com:espoon-voltti/voltti-infra.git//terraform/modules/s3/?depth=1&ref=v6.20.0"
}
module "account" {
  source = "../modules/account"
}
module "outside" {
  source = "../../../elsewhere/mod"
}
module "registry" {
  source = "hashicorp/consul/aws"
}
`)
	write(t, filepath.Join(root, "data.tf"), `
data "terraform_remote_state" "base" {
  backend   = "s3"
  workspace = terraform.workspace
  config = {
    bucket = "my-terraform"
    key    = "base.tfstate"
    region = "eu-north-1"
  }
}
data "terraform_remote_state" "common" {
  backend   = "s3"
  workspace = "common-${terraform.workspace}"
  config = {
    bucket = "voltti-common-terraform"
    key    = "voltti/common.tfstate"
  }
}
data "terraform_remote_state" "backup" {
  backend   = "s3"
  workspace = "backup"
  config = {
    bucket = "voltti-common-terraform"
    key    = "voltti/accounts/backup.tfstate"
  }
}
data "terraform_remote_state" "mapped" {
  backend   = "s3"
  workspace = local.variable.account_workspace[terraform.workspace]
  config = {
    bucket = "my-terraform"
    key    = "account.tfstate"
  }
}
data "terraform_remote_state" "implicit" {
  backend = "s3"
  config = {
    bucket = "my-terraform"
    key    = "x.tfstate"
  }
}
`)
	// A local module that itself calls a git module and another local one,
	// which calls back into the first: the cycle must terminate.
	write(t, filepath.Join(repo, "terraform/modules/account/main.tf"), `
module "iam" {
  source = "git::https://github.com/espoon-voltti/voltti-infra.git//terraform/modules/voltti-iam-policies?ref=v6.21.0"
}
module "bucket" {
  source = "../bucket"
}
`)
	write(t, filepath.Join(repo, "terraform/modules/bucket/main.tf"), `
module "account" {
  source = "../account"
}
`)
	write(t, filepath.Join(repo, "../elsewhere/mod/main.tf"), ``)

	d := Scan(root, repo)
	if len(d.Errors) != 0 {
		t.Errorf("errors: %v", d.Errors)
	}
	if d.Backend == nil || d.Backend.Bucket != "my-terraform" || d.Backend.Key != "service.tfstate" || d.Backend.Region != "eu-north-1" {
		t.Errorf("backend = %+v", d.Backend)
	}

	var gitKeys []string
	for _, g := range d.Git {
		gitKeys = append(gitKeys, g.Key()+"@"+g.Ref+" via "+g.Via)
	}
	wantGit := []string{
		"github.com/espoon-voltti/voltti-infra//terraform/modules/s3@v6.20.0 via s3",
		"github.com/espoon-voltti/voltti-infra//terraform/modules/voltti-iam-policies@v6.21.0 via account.iam",
	}
	if len(gitKeys) != len(wantGit) {
		t.Fatalf("git = %v, want %v", gitKeys, wantGit)
	}
	for i := range wantGit {
		if gitKeys[i] != wantGit[i] {
			t.Errorf("git[%d] = %q, want %q", i, gitKeys[i], wantGit[i])
		}
	}

	local := map[string]domain.LocalDep{}
	for _, l := range d.Local {
		local[l.Via] = l
	}
	if l := local["account"]; !l.InRepo || filepath.ToSlash(l.RelPath) != "terraform/modules/account" {
		t.Errorf("account = %+v", l)
	}
	if l := local["account.bucket"]; !l.InRepo || filepath.ToSlash(l.RelPath) != "terraform/modules/bucket" {
		t.Errorf("account.bucket = %+v", l)
	}
	if l := local["account.bucket.account"]; l.Via == "" {
		t.Error("the cyclic call should still be listed once")
	}
	if l := local["outside"]; l.InRepo {
		t.Errorf("outside = %+v should not be in repo", l)
	}

	rs := map[string]domain.RemoteStateRef{}
	for _, r := range d.RemoteStates {
		rs[r.Name] = r
	}
	if r := rs["base"]; r.WorkspaceKind != domain.WorkspaceSame || r.Bucket != "my-terraform" || r.Key != "base.tfstate" || r.Region != "eu-north-1" || r.Backend != "s3" {
		t.Errorf("base = %+v", r)
	}
	if r := rs["common"]; r.WorkspaceKind != domain.WorkspaceTemplate || r.WorkspaceExpr != `"common-${terraform.workspace}"` {
		t.Errorf("common = %+v", r)
	}
	if r := rs["backup"]; r.WorkspaceKind != domain.WorkspaceLiteral || r.WorkspaceExpr != "backup" {
		t.Errorf("backup = %+v", r)
	}
	if r := rs["mapped"]; r.WorkspaceKind != domain.WorkspaceUnknown || r.WorkspaceExpr == "" {
		t.Errorf("mapped = %+v", r)
	}
	if r := rs["implicit"]; r.WorkspaceKind != domain.WorkspaceLiteral || r.WorkspaceExpr != "default" {
		t.Errorf("implicit = %+v", r)
	}

	paths := (&domain.Module{RelPath: "terraform/service", Deps: d}).FingerprintPaths()
	want := []string{"terraform/modules/account", "terraform/modules/bucket", "terraform/service"}
	if len(paths) != len(want) {
		t.Fatalf("FingerprintPaths = %v", paths)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("FingerprintPaths = %v, want %v", paths, want)
		}
	}
}

func TestScanPartialBackendAndParseError(t *testing.T) {
	repo := t.TempDir()
	root := filepath.Join(repo, "app")
	write(t, filepath.Join(root, "main.tf"), `
terraform {
  backend "s3" {}
}
`)
	write(t, filepath.Join(root, "broken.tf"), `module "x" { source = `)
	d := Scan(root, repo)
	if d.Backend != nil {
		t.Errorf("partial backend should yield nil, got %+v", d.Backend)
	}
	if len(d.Errors) == 0 {
		t.Error("parse error should be reported")
	}
}

func TestEvalWorkspace(t *testing.T) {
	if got, ok := EvalWorkspace(`"common-${terraform.workspace}"`, "prod"); !ok || got != "common-prod" {
		t.Errorf("got %q, %v", got, ok)
	}
	if _, ok := EvalWorkspace(`local.x`, "prod"); ok {
		t.Error("unresolvable expression should fail")
	}
}

func TestIndexResolvesStateCycles(t *testing.T) {
	repo := t.TempDir()
	a := filepath.Join(repo, "a")
	b := filepath.Join(repo, "b")
	write(t, filepath.Join(a, "main.tf"), `
terraform {
  backend "s3" {
    bucket = "tf"
    key    = "a.tfstate"
  }
}
data "terraform_remote_state" "b" {
  backend = "s3"
  config = {
    bucket = "tf"
    key    = "b.tfstate"
  }
}
module "waf" {
  source = "git::https://github.com/org/shared.git//modules/waf?ref=v1"
}
`)
	write(t, filepath.Join(b, "main.tf"), `
terraform {
  backend "s3" {
    bucket = "tf"
    key    = "b.tfstate"
    region = "eu-west-1"
  }
}
data "terraform_remote_state" "a" {
  backend = "s3"
  config = {
    bucket = "tf"
    key    = "a.tfstate"
  }
}
module "waf" {
  source = "git::https://github.com/org/shared.git//modules/waf?ref=v2"
}
`)
	r := &domain.Repo{Path: repo}
	modA := &domain.Module{Repo: r, Path: a, RelPath: "a", Deps: Scan(a, repo)}
	modB := &domain.Module{Repo: r, Path: b, RelPath: "b", Deps: Scan(b, repo)}
	r.Modules = []*domain.Module{modA, modB}
	ix := Build([]*domain.Repo{r})

	if up := ix.Upstream(modA.Deps.RemoteStates[0]); up != modB {
		t.Errorf("a's upstream = %v, want b", up)
	}
	if up := ix.Upstream(modB.Deps.RemoteStates[0]); up != modA {
		t.Errorf("b's upstream = %v, want a", up)
	}
	if deps := ix.DependentsOfState(modA); len(deps) != 1 || deps[0] != modB {
		t.Errorf("dependents of a = %v", deps)
	}
	if deps := ix.DependentsOfGit("github.com/org/shared//modules/waf"); len(deps) != 2 {
		t.Errorf("waf dependents = %v", deps)
	}
	if keys := ix.GitKeys(); len(keys) != 1 {
		t.Errorf("git keys = %v", keys)
	}
	if addrs := ix.StateKeys(); len(addrs) != 2 {
		t.Errorf("state keys = %v", addrs)
	}
}
