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

func TestBumpRef(t *testing.T) {
	cases := map[string]string{
		"git::https://github.com/org/shared.git//modules/waf?ref=v16":        "git::https://github.com/org/shared.git//modules/waf?ref=v18",
		"git@github.com:org/shared.git//modules/s3/?depth=1&ref=v6.20.0":     "git@github.com:org/shared.git//modules/s3/?depth=1&ref=v18",
		"git::ssh://git@github.com/org/shared.git//modules/x?ref=v1&depth=1": "git::ssh://git@github.com/org/shared.git//modules/x?ref=v18&depth=1",
		"git::https://github.com/org/shared.git//modules/waf":                "git::https://github.com/org/shared.git//modules/waf",
		"git::https://github.com/org/shared.git//modules/preferred?prefer=1": "git::https://github.com/org/shared.git//modules/preferred?prefer=1",
	}
	for in, want := range cases {
		if got := BumpRef(in, "v18"); got != want {
			t.Errorf("BumpRef(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewerTagsOnlyForPinsWithANewerTag(t *testing.T) {
	waf := domain.GitRef{Raw: "git::https://github.com/org/shared.git//modules/waf?ref=v16", Repo: "github.com/org/shared", Subdir: "modules/waf", Ref: "v16"}
	s3 := domain.GitRef{Raw: "git::https://github.com/org/shared.git//modules/s3?ref=v16", Repo: "github.com/org/shared", Subdir: "modules/s3", Ref: "v16"}
	d := &domain.Deps{Git: []domain.GitRef{waf, s3}}
	behind := map[string]Behind{
		RefKey(waf): {Ref: "v16", NewerTag: "v18"},
		RefKey(s3):  {Ref: "v16", Unreleased: true},
	}
	got := NewerTags(d, behind)
	want := map[string]string{waf.Raw: "git::https://github.com/org/shared.git//modules/waf?ref=v18"}
	if len(got) != 1 || got[waf.Raw] != want[waf.Raw] {
		t.Errorf("NewerTags = %v, want %v", got, want)
	}
	if NewerTags(nil, behind) != nil {
		t.Error("nil deps should yield nil")
	}
}

func TestRewriteSourcesReplacesQuotedSourcesInPlace(t *testing.T) {
	dir := t.TempDir()
	old := "git::https://github.com/org/shared.git//modules/waf?ref=v16"
	main := "module \"waf\" {\n  source = \"" + old + "\"\n}\nmodule \"waf2\" {\n  source = \"" + old + "\"\n}\n"
	other := "module \"s3\" {\n  source = \"git::https://github.com/org/shared.git//modules/s3?ref=v16\"\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(main), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "other.tf"), []byte(other), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	refs, files, err := RewriteSources([]string{dir}, map[string]string{old: BumpRef(old, "v18")})
	if err != nil {
		t.Fatal(err)
	}
	if refs != 2 || files != 1 {
		t.Errorf("refs=%d files=%d, want 2 refs in 1 file", refs, files)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "main.tf"))
	want := "module \"waf\" {\n  source = \"git::https://github.com/org/shared.git//modules/waf?ref=v18\"\n}\nmodule \"waf2\" {\n  source = \"git::https://github.com/org/shared.git//modules/waf?ref=v18\"\n}\n"
	if string(got) != want {
		t.Errorf("main.tf:\n%s", got)
	}
	if info, _ := os.Stat(filepath.Join(dir, "main.tf")); info.Mode().Perm() != 0o640 {
		t.Errorf("permissions changed to %v", info.Mode().Perm())
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "other.tf")); string(got) != other {
		t.Error("other.tf must be untouched")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "notes.md")); string(got) != old {
		t.Error("non-.tf files must be untouched")
	}
}

func TestSourceDirsStaysInsideTheRepo(t *testing.T) {
	mod := &domain.Module{Path: "/iac/repo/envs/prod", Deps: &domain.Deps{Local: []domain.LocalDep{
		{Dir: "/iac/repo/modules/net", InRepo: true},
		{Dir: "/elsewhere/shared", InRepo: false},
	}}}
	got := SourceDirs(mod)
	if len(got) != 2 || got[0] != "/iac/repo/envs/prod" || got[1] != "/iac/repo/modules/net" {
		t.Errorf("SourceDirs = %v", got)
	}
}
