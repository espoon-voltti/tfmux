// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package report

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/espoon-voltti/tfmux/internal/discovery"
	"github.com/espoon-voltti/tfmux/internal/gittest"
	"github.com/espoon-voltti/tfmux/internal/state"
)

func TestDepsReportAcrossRepos(t *testing.T) {
	root := t.TempDir()
	base := gitRepoWithModule(t, root, "base-infra", "terraform/base")
	gittest.WriteFile(t, filepath.Join(base, "terraform/base/modules.tf"), `
module "waf" {
  source = "git::https://github.com/org/shared.git//modules/waf?ref=v16"
}
`)
	app := gitRepoWithModule(t, root, "app-infra", "terraform/app")
	gittest.WriteFile(t, filepath.Join(app, "terraform/app/main.tf"), `
terraform {
  backend "s3" {
    bucket = "b"
    key    = "app.tfstate"
  }
}
data "terraform_remote_state" "base" {
  backend   = "s3"
  workspace = terraform.workspace
  config = {
    bucket = "b"
    key    = "k"
  }
}
module "waf" {
  source = "git::https://github.com/org/shared.git//modules/waf?ref=v18"
}
`)
	repos, err := discovery.Discover([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	mods, ix, behind := Deps(state.Ignore{}, repos)
	if len(mods) != 2 {
		t.Fatalf("modules = %+v", mods)
	}

	var fwd strings.Builder
	PrintDepsForward(&fwd, mods, behind, "app-infra")
	if !strings.Contains(fwd.String(), "state   base-infra/terraform/base  (base, workspace: same)") {
		t.Errorf("forward output should resolve the upstream root module:\n%s", fwd.String())
	}
	if strings.Contains(fwd.String(), "base-infra/terraform/base\n") {
		t.Errorf("filter should hide the other module:\n%s", fwd.String())
	}

	var rev strings.Builder
	PrintDepsReverse(&rev, ix, behind, "shared//modules/waf")
	got := rev.String()
	if !strings.Contains(got, "github.com/org/shared//modules/waf\n") ||
		!strings.Contains(got, "app-infra/terraform/app @v18") ||
		!strings.Contains(got, "base-infra/terraform/base @v16") {
		t.Errorf("reverse output:\n%s", got)
	}
	if strings.Contains(got, "s3://") {
		t.Errorf("filter should hide state keys:\n%s", got)
	}

	rev.Reset()
	PrintDepsReverse(&rev, ix, behind, "s3://b/k")
	if !strings.Contains(rev.String(), "app-infra/terraform/app  (base, workspace: same)") {
		t.Errorf("reverse state output:\n%s", rev.String())
	}
}
