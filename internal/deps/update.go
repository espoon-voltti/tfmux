// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package deps

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/espoon-voltti/tfmux/internal/domain"
)

var refParam = regexp.MustCompile(`([?&])ref=[^&]*`)

// BumpRef returns source with its ?ref= query value replaced by ref. A
// source without a ref is returned unchanged: an unpinned module floats on
// purpose.
func BumpRef(source, ref string) string {
	return refParam.ReplaceAllString(source, "${1}ref="+ref)
}

// NewerTags maps each git module source of d that has a newer tag in its
// shared repo's checkout, per behind (keyed by RefKey), to the same source
// pinned to that tag.
func NewerTags(d *domain.Deps, behind map[string]Behind) map[string]string {
	if d == nil {
		return nil
	}
	out := map[string]string{}
	for _, g := range d.Git {
		if b, ok := behind[RefKey(g)]; ok && b.NewerTag != "" {
			out[g.Raw] = BumpRef(g.Raw, b.NewerTag)
		}
	}
	return out
}

// RewriteSources replaces quoted module source strings in the .tf files
// directly under each dir, per replace (old source → new source), and
// reports how many occurrences and files changed. Files are rewritten in
// place with their original permissions.
func RewriteSources(dirs []string, replace map[string]string) (refs, files int, err error) {
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return refs, files, err
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".tf") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			src, err := os.ReadFile(path)
			if err != nil {
				return refs, files, err
			}
			text := string(src)
			n := 0
			for old, new := range replace {
				quoted := `"` + old + `"`
				c := strings.Count(text, quoted)
				if c == 0 {
					continue
				}
				text = strings.ReplaceAll(text, quoted, `"`+new+`"`)
				n += c
			}
			if n == 0 {
				continue
			}
			info, err := e.Info()
			if err != nil {
				return refs, files, err
			}
			if err := os.WriteFile(path, []byte(text), info.Mode().Perm()); err != nil {
				return refs, files, err
			}
			refs += n
			files++
		}
	}
	return refs, files, nil
}

// SourceDirs lists the directories whose .tf files RewriteSources should
// touch for a root module: its own and every in-repo local module it calls.
// Local modules outside the repo belong to someone else's checkout.
func SourceDirs(mod *domain.Module) []string {
	dirs := []string{mod.Path}
	if mod.Deps != nil {
		for _, d := range mod.Deps.Local {
			if d.InRepo {
				dirs = append(dirs, d.Dir)
			}
		}
	}
	return dirs
}
