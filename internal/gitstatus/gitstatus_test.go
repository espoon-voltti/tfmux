// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package gitstatus

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/espoon-voltti/tfmux/internal/domain"
	"github.com/espoon-voltti/tfmux/internal/gittest"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want func(t *testing.T, st domain.GitStatus)
	}{
		{"clean with upstream", "# branch.oid 1234567890abcdef\n# branch.head main\n# branch.upstream origin/main\n# branch.ab +2 -1\n",
			func(t *testing.T, st domain.GitStatus) {
				if st.Branch != "main" || st.Dirty || st.Ahead != 2 || st.Behind != 1 || !st.HasUpstream {
					t.Errorf("unexpected: %+v", st)
				}
			}},
		{"dirty detached", "# branch.oid deadbeef\n# branch.head (detached)\n1 .M N... 100644 100644 100644 abc def main.tf\n? untracked.tf\n",
			func(t *testing.T, st domain.GitStatus) {
				if !st.Detached || st.Branch != "" || !st.Dirty {
					t.Errorf("unexpected: %+v", st)
				}
			}},
		{"initial, no upstream", "# branch.oid (initial)\n# branch.head main\n",
			func(t *testing.T, st domain.GitStatus) {
				if st.Branch != "main" || st.HasUpstream || st.Dirty || st.OID != "(initial)" {
					t.Errorf("unexpected: %+v", st)
				}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.want(t, Parse(c.out)) })
	}
}

func TestCLIAgainstRealRepo(t *testing.T) {
	dir := gittest.Init(t)
	gittest.WriteFile(t, filepath.Join(dir, "a.txt"), "x")
	gittest.CommitAll(t, dir, "init")

	st := CLI{}.Status(context.Background(), dir)
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	if st.Branch != "main" || st.Dirty {
		t.Errorf("clean repo: %+v", st)
	}

	gittest.WriteFile(t, filepath.Join(dir, "b.txt"), "y")
	st = CLI{}.Status(context.Background(), dir)
	if !st.Dirty {
		t.Errorf("expected dirty after untracked file: %+v", st)
	}
}

func TestCLINonRepo(t *testing.T) {
	st := CLI{}.Status(context.Background(), t.TempDir())
	if st.Err == nil {
		t.Error("expected error for non-repo")
	}
}
