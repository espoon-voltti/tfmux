// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

// Package discovery walks configured root directories to find git repos and,
// within them, Terraform root modules.
//
// A directory containing .git is a repo. Inside a repo, a directory is a root
// module when any of its *.tf files declares a terraform{} block with a
// backend or cloud sub-block; as a fallback (local-state modules), a
// terraform{} block plus at least one provider block in the directory counts.
package discovery

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"

	"github.com/espoon-voltti/tfmux/internal/domain"
	"github.com/espoon-voltti/tfmux/internal/manifest"
)

// skippedDirs are never descended into while scanning inside a repo.
var skippedDirs = map[string]bool{
	".git":         true,
	".terraform":   true,
	"node_modules": true,
}

// Discover walks each root and returns the repos found, sorted by path.
// Roots that do not exist are silently skipped (the config may list dirs
// that only exist on some machines).
func Discover(roots []string) ([]*domain.Repo, error) {
	var repos []*domain.Repo
	seen := map[string]bool{}
	for _, root := range roots {
		abs, err := filepath.Abs(root)
		if err != nil {
			return nil, err
		}
		if info, err := os.Stat(abs); err != nil || !info.IsDir() {
			continue
		}
		err = filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable subtree: skip, don't abort the scan
			}
			if !d.IsDir() {
				return nil
			}
			if name := d.Name(); path != abs && strings.HasPrefix(name, ".") || skippedDirs[d.Name()] {
				return fs.SkipDir
			}
			if isGitRepo(path) && !seen[path] {
				seen[path] = true
				repo := scanRepo(path)
				repos = append(repos, repo)
				return fs.SkipDir // modules were scanned inside scanRepo
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].Path < repos[j].Path })
	return repos, nil
}

func isGitRepo(dir string) bool {
	// .git may be a directory (normal) or a file (worktrees/submodules).
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// scanRepo walks inside one repo collecting root modules, sorted by RelPath.
func scanRepo(repoPath string) *domain.Repo {
	repo := &domain.Repo{
		Path: repoPath,
		Name: filepath.Base(repoPath),
	}
	_ = filepath.WalkDir(repoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		name := d.Name()
		if path != repoPath && (strings.HasPrefix(name, ".") || skippedDirs[name]) {
			return fs.SkipDir
		}
		if IsRootModule(path) {
			rel, relErr := filepath.Rel(repoPath, path)
			if relErr != nil {
				rel = path
			}
			repo.Modules = append(repo.Modules, &domain.Module{
				Repo:    repo,
				Path:    path,
				RelPath: rel,
			})
		}
		return nil
	})
	applyManifest(repo)
	sort.Slice(repo.Modules, func(i, j int) bool { return repo.Modules[i].RelPath < repo.Modules[j].RelPath })
	return repo
}

// applyManifest loads repo's repo manifest, if any, and annotates its
// modules: a listed module gets its manifest workspaces; an entry with no
// discovered module becomes a module in its own right when the directory
// exists (the manifest is authoritative even where IsRootModule missed it),
// otherwise it's recorded as missing.
func applyManifest(repo *domain.Repo) {
	mf, err := manifest.Load(repo.Path)
	if err != nil {
		repo.ManifestErr = err.Error()
		return
	}
	if mf == nil {
		return
	}
	repo.ManifestPath = mf.Path

	byRelPath := make(map[string]*domain.Module, len(repo.Modules))
	for _, mod := range repo.Modules {
		byRelPath[filepath.ToSlash(mod.RelPath)] = mod
	}
	for _, entry := range mf.Entries {
		if mod, ok := byRelPath[entry.RootModule]; ok {
			mod.ManifestListed = true
			mod.ManifestWorkspaces = entry.Workspaces
			continue
		}
		dir := filepath.Join(repo.Path, filepath.FromSlash(entry.RootModule))
		if info, statErr := os.Stat(dir); statErr == nil && info.IsDir() {
			repo.Modules = append(repo.Modules, &domain.Module{
				Repo:               repo,
				Path:               dir,
				RelPath:            filepath.FromSlash(entry.RootModule),
				ManifestListed:     true,
				ManifestWorkspaces: entry.Workspaces,
			})
			continue
		}
		repo.ManifestMissing = append(repo.ManifestMissing, entry.RootModule)
	}
}

// IsRootModule reports whether dir's *.tf files mark it as a Terraform root
// module. HCL parse errors are tolerated: we look for evidence in whatever
// parses, never reject a module because one file has a syntax error.
func IsRootModule(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	hasTerraformBlock := false
	hasBackendOrCloud := false
	hasProvider := false
	parser := hclparse.NewParser()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tf") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		f, _ := parser.ParseHCL(src, filepath.Join(dir, e.Name()))
		if f == nil || f.Body == nil {
			continue
		}
		content, _, _ := f.Body.PartialContent(&hcl.BodySchema{
			Blocks: []hcl.BlockHeaderSchema{
				{Type: "terraform"},
				{Type: "provider", LabelNames: []string{"name"}},
			},
		})
		if content == nil {
			continue
		}
		for _, block := range content.Blocks {
			switch block.Type {
			case "provider":
				hasProvider = true
			case "terraform":
				hasTerraformBlock = true
				inner, _, _ := block.Body.PartialContent(&hcl.BodySchema{
					Blocks: []hcl.BlockHeaderSchema{
						{Type: "backend", LabelNames: []string{"type"}},
						{Type: "cloud"},
					},
				})
				if inner != nil && len(inner.Blocks) > 0 {
					hasBackendOrCloud = true
				}
			}
		}
	}
	return hasBackendOrCloud || (hasTerraformBlock && hasProvider)
}
