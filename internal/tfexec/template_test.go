// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package tfexec

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/espoon-voltti/tfmux/internal/domain"
)

func TestTemplateCommandStrings(t *testing.T) {
	odd := "/state/it's a dir/plan.tfplan"
	quoted := `'/state/it'\''s a dir/plan.tfplan'`
	cases := []struct{ got, want string }{
		{InitCommand("terraform init", false), "terraform init -input=false -no-color"},
		{InitCommand("terraform init", true), "terraform init -input=false -no-color -upgrade"},
		{PlanCommand("terraform plan", odd), "terraform plan -input=false -no-color -detailed-exitcode -out=" + quoted},
		{ApplyCommand("terraform apply", odd), "terraform apply -input=false " + quoted},
		{TemplateOr("", "plan"), `"$TFMUX_TF_BIN" plan`},
		{TemplateOr("custom plan", "plan"), "custom plan"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got  %s\nwant %s", c.got, c.want)
		}
	}
}

// A template module's workspace reaches the template as TFMUX_WORKSPACE and
// never as TF_WORKSPACE.
func TestPlanTemplateNoTFWorkspace(t *testing.T) {
	tf, logFile := newTF(t)
	tf.Templates = &domain.CommandTemplates{Plan: `"$TFMUX_TF_BIN" plan -var-file=$TFMUX_WORKSPACE.tfvars`}
	outFile := filepath.Join(t.TempDir(), "plan.tfplan")
	res, err := tf.Plan(context.Background(), "prod", outFile)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d:\n%s", res.ExitCode, res.Output)
	}
	cs := calls(t, logFile)
	last := cs[len(cs)-1]
	if !strings.Contains(last, " <none> ") {
		t.Errorf("TF_WORKSPACE leaked into a template run: %q", last)
	}
	for _, arg := range []string{"-var-file=prod.tfvars", "-input=false", "-detailed-exitcode", "-out=" + outFile} {
		if !strings.Contains(last, arg) {
			t.Errorf("missing arg %q in %q", arg, last)
		}
	}
	if _, err := os.Stat(outFile); err != nil {
		t.Errorf("plan file not written: %v", err)
	}
}

func TestInitTemplateExpandsSuffix(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		tf, logFile := newTF(t)
		tf.Templates = &domain.CommandTemplates{Init: `"$TFMUX_TF_BIN" init -backend-config=${TFMUX_WORKSPACE##*-}.hcl`}
		res, err := tf.Init(context.Background(), "myproject-prod", upgrade)
		if err != nil {
			t.Fatal(err)
		}
		if res.ExitCode != 0 {
			t.Fatalf("exit = %d:\n%s", res.ExitCode, res.Output)
		}
		cs := calls(t, logFile)
		last := cs[len(cs)-1]
		for _, arg := range []string{"init -backend-config=prod.hcl", "-input=false", "-no-color"} {
			if !strings.Contains(last, arg) {
				t.Errorf("missing %q in %q", arg, last)
			}
		}
		if strings.Contains(last, "-upgrade") != upgrade {
			t.Errorf("upgrade=%v but call was %q", upgrade, last)
		}
	}
}

// With only an init template, plan and output fall back to the binary — still
// without TF_WORKSPACE, since the workspace is a logical name.
func TestTemplateDefaultsForMissingVerbs(t *testing.T) {
	tf, logFile := newTF(t)
	tf.Templates = &domain.CommandTemplates{Init: `"$TFMUX_TF_BIN" init -backend-config=$TFMUX_WORKSPACE.hcl`}
	if err := os.MkdirAll(filepath.Join(tf.Dir, ".terraform"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := tf.Plan(context.Background(), "prod", filepath.Join(t.TempDir(), "p")); err != nil {
		t.Fatal(err)
	}
	if _, err := tf.Output(context.Background(), "prod"); err != nil {
		t.Fatal(err)
	}
	cs := calls(t, logFile)
	if len(cs) != 2 {
		t.Fatalf("calls = %v", cs)
	}
	for _, c := range cs {
		if !strings.Contains(c, " <none> ") {
			t.Errorf("TF_WORKSPACE set for a template module: %q", c)
		}
	}
	if !strings.Contains(cs[0], " plan ") || !strings.Contains(cs[1], " output ") {
		t.Errorf("calls = %v", cs)
	}
}

func TestTemplateExitCodePropagates(t *testing.T) {
	tf, _ := newTF(t)
	tf.Templates = &domain.CommandTemplates{Plan: "echo boom >&2; exit 3; true"}
	res, err := tf.Plan(context.Background(), "prod", filepath.Join(t.TempDir(), "p"))
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || !strings.Contains(string(res.Output), "boom") {
		t.Errorf("res = %d %q", res.ExitCode, res.Output)
	}
}
