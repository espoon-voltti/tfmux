// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

// Package deps reads a root module's .tf files to find what it depends on:
// local and git module calls (followed through local modules) and the
// remote states it reads. Everything is static — terraform init is never
// needed — so the result reflects what is written in the configuration, not
// what was last initialised.
package deps

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/zclconf/go-cty/cty"

	"github.com/espoon-voltti/tfmux/internal/domain"
)

// maxLocalDepth bounds how far local module calls are followed, as a backstop
// to the visited set for pathological trees.
const maxLocalDepth = 10

// Scan reads the root module at moduleDir, inside the repo at repoPath.
func Scan(moduleDir, repoPath string) *domain.Deps {
	s := &scanner{repoPath: repoPath, deps: &domain.Deps{}, visited: map[string]bool{}}
	s.scanDir(moduleDir, "", 0, true)
	sort.Slice(s.deps.Local, func(i, j int) bool { return s.deps.Local[i].Dir < s.deps.Local[j].Dir })
	sort.Slice(s.deps.Git, func(i, j int) bool { return s.deps.Git[i].Key() < s.deps.Git[j].Key() })
	return s.deps
}

type scanner struct {
	repoPath string
	deps     *domain.Deps
	visited  map[string]bool
}

var dirSchema = &hcl.BodySchema{
	Blocks: []hcl.BlockHeaderSchema{
		{Type: "module", LabelNames: []string{"name"}},
		{Type: "data", LabelNames: []string{"type", "name"}},
		{Type: "terraform"},
	},
}

// scanDir reads one directory's .tf files. Only the root (isRoot) contributes
// remote states and the backend; nested local modules contribute module calls.
func (s *scanner) scanDir(dir, via string, depth int, isRoot bool) {
	if s.visited[dir] || depth > maxLocalDepth {
		return
	}
	s.visited[dir] = true
	entries, err := os.ReadDir(dir)
	if err != nil {
		s.errorf("%s: %v", dir, err)
		return
	}
	parser := hclparse.NewParser()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tf") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		src, err := os.ReadFile(path)
		if err != nil {
			s.errorf("%s: %v", path, err)
			continue
		}
		f, diags := parser.ParseHCL(src, path)
		if diags.HasErrors() {
			s.errorf("%s: %s", path, diags.Error())
		}
		if f == nil || f.Body == nil {
			continue
		}
		content, _, _ := f.Body.PartialContent(dirSchema)
		if content == nil {
			continue
		}
		for _, block := range content.Blocks {
			switch block.Type {
			case "module":
				s.moduleCall(dir, via, depth, block, src)
			case "data":
				if isRoot && block.Labels[0] == "terraform_remote_state" {
					s.remoteState(block, src)
				}
			case "terraform":
				if isRoot {
					s.backend(block)
				}
			}
		}
	}
}

func (s *scanner) errorf(format string, args ...any) {
	s.deps.Errors = append(s.deps.Errors, fmt.Sprintf(format, args...))
}

func (s *scanner) moduleCall(dir, via string, depth int, block *hcl.Block, src []byte) {
	source, ok := literalAttr(block.Body, "source")
	if !ok {
		return
	}
	callPath := block.Labels[0]
	if via != "" {
		callPath = via + "." + callPath
	}
	switch {
	case strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../"):
		target := filepath.Clean(filepath.Join(dir, filepath.FromSlash(source)))
		dep := domain.LocalDep{Dir: target, Via: callPath}
		if rel, err := filepath.Rel(s.repoPath, target); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			dep.InRepo = true
			dep.RelPath = rel
		}
		s.deps.Local = append(s.deps.Local, dep)
		s.scanDir(target, callPath, depth+1, false)
	default:
		if ref, ok := ParseGitSource(source); ok {
			ref.Via = callPath
			s.deps.Git = append(s.deps.Git, ref)
		}
	}
}

// ParseGitSource recognizes the git forms of a module source and splits it
// into repo, subdir and ref. Registry, local and other sources return false.
func ParseGitSource(source string) (domain.GitRef, bool) {
	ref := domain.GitRef{Raw: source}
	s := source
	forced := strings.HasPrefix(s, "git::")
	s = strings.TrimPrefix(s, "git::")
	if !forced && !looksLikeGit(s) {
		return ref, false
	}
	if i := strings.IndexByte(s, '?'); i >= 0 {
		if q, err := url.ParseQuery(s[i+1:]); err == nil {
			ref.Ref = q.Get("ref")
		}
		s = s[:i]
	}
	rest := s
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if i := strings.Index(rest, "//"); i >= 0 {
		ref.Subdir = strings.Trim(rest[i+2:], "/")
		rest = rest[:i]
	}
	rest = strings.TrimPrefix(rest, "git@")
	if i := strings.IndexByte(rest, '@'); i >= 0 && i < strings.IndexByte(rest+"/", '/') {
		rest = rest[i+1:] // user@host
	}
	if slash := strings.IndexByte(rest, '/'); slash > 0 {
		if colon := strings.IndexByte(rest[:slash], ':'); colon >= 0 {
			rest = rest[:colon] + "/" + rest[colon+1:] // scp-style host:path
		}
	}
	rest = strings.TrimSuffix(strings.TrimSuffix(rest, "/"), ".git")
	ref.Repo = rest
	return ref, ref.Repo != ""
}

func looksLikeGit(s string) bool {
	switch {
	case strings.HasPrefix(s, "git@"), strings.HasPrefix(s, "ssh://"):
		return true
	case strings.HasPrefix(s, "github.com/"), strings.HasPrefix(s, "gitlab.com/"), strings.HasPrefix(s, "bitbucket.org/"):
		return true
	case strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://"):
		path := s
		if i := strings.IndexByte(path, '?'); i >= 0 {
			path = path[:i]
		}
		return strings.Contains(path, ".git")
	}
	return false
}

func (s *scanner) backend(block *hcl.Block) {
	content, _, _ := block.Body.PartialContent(&hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{{Type: "backend", LabelNames: []string{"type"}}},
	})
	if content == nil {
		return
	}
	for _, b := range content.Blocks {
		addr := domain.StateAddress{Backend: b.Labels[0]}
		addr.Bucket, _ = literalAttr(b.Body, "bucket")
		addr.Key, _ = literalAttr(b.Body, "key")
		addr.Region, _ = literalAttr(b.Body, "region")
		if addr.Bucket != "" && addr.Key != "" {
			s.deps.Backend = &addr
		}
	}
}

func (s *scanner) remoteState(block *hcl.Block, src []byte) {
	ref := domain.RemoteStateRef{Name: block.Labels[1], WorkspaceKind: domain.WorkspaceUnknown}
	content, _, _ := block.Body.PartialContent(&hcl.BodySchema{
		Attributes: []hcl.AttributeSchema{{Name: "backend"}, {Name: "config"}, {Name: "workspace"}},
	})
	if content == nil {
		return
	}
	if a, ok := content.Attributes["backend"]; ok {
		ref.Backend, _ = literalString(a.Expr)
	}
	if a, ok := content.Attributes["config"]; ok {
		if pairs, diags := hcl.ExprMap(a.Expr); !diags.HasErrors() {
			for _, p := range pairs {
				key := hcl.ExprAsKeyword(p.Key)
				if key == "" {
					key, _ = literalString(p.Key)
				}
				val, _ := literalString(p.Value)
				switch key {
				case "bucket":
					ref.Bucket = val
				case "key":
					ref.Key = val
				case "region":
					ref.Region = val
				}
			}
		}
	}
	if a, ok := content.Attributes["workspace"]; ok {
		ref.WorkspaceKind, ref.WorkspaceExpr = classifyWorkspace(a.Expr, src)
	} else {
		ref.WorkspaceKind, ref.WorkspaceExpr = domain.WorkspaceLiteral, "default"
	}
	s.deps.RemoteStates = append(s.deps.RemoteStates, ref)
}

// classifyWorkspace evaluates expr with only terraform.workspace bound, twice
// with different probe values: an error means it depends on something else;
// equal results mean a constant; otherwise it is a function of the workspace.
func classifyWorkspace(expr hcl.Expression, src []byte) (domain.WorkspaceExprKind, string) {
	const probeA, probeB = "\x00probe-a", "\x00probe-b"
	a, errA := evalWithWorkspace(expr, probeA)
	b, errB := evalWithWorkspace(expr, probeB)
	switch {
	case errA || errB:
		return domain.WorkspaceUnknown, exprSource(expr, src)
	case a == probeA && b == probeB:
		return domain.WorkspaceSame, ""
	case a == b:
		return domain.WorkspaceLiteral, a
	default:
		return domain.WorkspaceTemplate, exprSource(expr, src)
	}
}

func evalWithWorkspace(expr hcl.Expression, workspace string) (string, bool) {
	ctx := &hcl.EvalContext{Variables: map[string]cty.Value{
		"terraform": cty.ObjectVal(map[string]cty.Value{"workspace": cty.StringVal(workspace)}),
	}}
	v, diags := expr.Value(ctx)
	if diags.HasErrors() || !v.IsKnown() || v.IsNull() || v.Type() != cty.String {
		return "", true
	}
	return v.AsString(), false
}

// EvalWorkspace resolves a Template expression for one downstream workspace
// name, re-parsing the source text recorded by Scan.
func EvalWorkspace(exprSrc, workspace string) (string, bool) {
	expr, diags := hclparse.NewParser().ParseHCL([]byte("w = "+exprSrc), "workspace.hcl")
	if diags.HasErrors() || expr == nil {
		return "", false
	}
	attrs, _ := expr.Body.JustAttributes()
	a, ok := attrs["w"]
	if !ok {
		return "", false
	}
	v, failed := evalWithWorkspace(a.Expr, workspace)
	return v, !failed
}

func exprSource(expr hcl.Expression, src []byte) string {
	r := expr.Range()
	if r.Start.Byte < 0 || r.End.Byte > len(src) || r.Start.Byte > r.End.Byte {
		return ""
	}
	return string(src[r.Start.Byte:r.End.Byte])
}

func literalAttr(body hcl.Body, name string) (string, bool) {
	content, _, _ := body.PartialContent(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: name}}})
	if content == nil {
		return "", false
	}
	a, ok := content.Attributes[name]
	if !ok {
		return "", false
	}
	return literalString(a.Expr)
}

func literalString(expr hcl.Expression) (string, bool) {
	v, diags := expr.Value(nil)
	if diags.HasErrors() || !v.IsKnown() || v.IsNull() || v.Type() != cty.String {
		return "", false
	}
	return v.AsString(), true
}
