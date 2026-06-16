// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

// tfmux: a TUI for orchestrating terraform plan/apply across many repos,
// root modules and workspaces.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/japsu/tfmux/internal/config"
	"github.com/japsu/tfmux/internal/discovery"
	"github.com/japsu/tfmux/internal/domain"
	"github.com/japsu/tfmux/internal/gitstatus"
	"github.com/japsu/tfmux/internal/paths"
	"github.com/japsu/tfmux/internal/state"
	"github.com/japsu/tfmux/internal/ui"
)

var version = "dev" // overridden via -ldflags "-X main.version=..."

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "tfmux:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "tui"
	if len(args) > 0 {
		cmd = args[0]
		args = args[1:]
	}
	switch cmd {
	case "version", "--version", "-v":
		fmt.Println("tfmux", version)
		return nil
	case "ls":
		return runLs(args)
	case "tui":
		return runTUI()
	case "help", "--help", "-h":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: tfmux [command]

commands:
  tui        launch the interactive TUI (default)
  ls         print discovered repos, modules and git status
  ls --json  same, as JSON
  version    print version
`)
}

// loadConfig loads the user config; a missing file is reported as a hint but
// still returns usable defaults (with no roots, discovery finds nothing).
func loadConfig() (*config.Config, error) {
	cfg, err := config.LoadDefault()
	if errors.Is(err, config.ErrNotFound) {
		fmt.Fprintln(os.Stderr, "tfmux:", err)
		fmt.Fprintln(os.Stderr, "tfmux: create it with at least: roots = [\"~/path/to/iac\"]")
		return cfg, nil
	}
	return cfg, err
}

func discoverWithGit(ctx context.Context, cfg *config.Config) ([]*domain.Repo, error) {
	repos, err := discovery.Discover(cfg.Roots)
	if err != nil {
		return nil, err
	}
	git := gitstatus.CLI{}
	var wg sync.WaitGroup
	for _, repo := range repos {
		wg.Add(1)
		go func(r *domain.Repo) {
			defer wg.Done()
			r.Git = git.Status(ctx, r.Path)
		}(repo)
	}
	wg.Wait()
	return repos, nil
}

func runLs(args []string) error {
	asJSON := len(args) > 0 && args[0] == "--json"
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	repos, err := discoverWithGit(context.Background(), cfg)
	if err != nil {
		return err
	}
	if asJSON {
		return printJSON(repos)
	}
	for _, repo := range repos {
		fmt.Printf("%s  %s\n", repo.Path, gitSummary(repo.Git))
		for _, m := range repo.Modules {
			fmt.Printf("  %s\n", m.RelPath)
		}
	}
	return nil
}

func gitSummary(g domain.GitStatus) string {
	if g.Err != nil {
		return "[git error: " + g.Err.Error() + "]"
	}
	s := g.Branch
	if g.Detached {
		s = "(detached " + short(g.OID) + ")"
	}
	if g.Dirty {
		s += " *dirty"
	}
	if g.Ahead > 0 {
		s += fmt.Sprintf(" ↑%d", g.Ahead)
	}
	if g.Behind > 0 {
		s += fmt.Sprintf(" ↓%d", g.Behind)
	}
	if !g.HasUpstream && !g.Detached {
		s += " (no upstream)"
	}
	return s
}

func short(oid string) string {
	if len(oid) > 8 {
		return oid[:8]
	}
	return oid
}

type lsModule struct {
	Path    string `json:"path"`
	RelPath string `json:"rel_path"`
}

type lsRepo struct {
	Path     string     `json:"path"`
	Branch   string     `json:"branch,omitempty"`
	Detached bool       `json:"detached,omitempty"`
	Dirty    bool       `json:"dirty"`
	Ahead    int        `json:"ahead"`
	Behind   int        `json:"behind"`
	GitError string     `json:"git_error,omitempty"`
	Modules  []lsModule `json:"modules"`
}

func printJSON(repos []*domain.Repo) error {
	out := make([]lsRepo, 0, len(repos))
	for _, r := range repos {
		lr := lsRepo{
			Path:   r.Path,
			Branch: r.Git.Branch, Detached: r.Git.Detached,
			Dirty: r.Git.Dirty, Ahead: r.Git.Ahead, Behind: r.Git.Behind,
			Modules: make([]lsModule, 0, len(r.Modules)),
		}
		if r.Git.Err != nil {
			lr.GitError = r.Git.Err.Error()
		}
		for _, m := range r.Modules {
			lr.Modules = append(lr.Modules, lsModule{Path: m.Path, RelPath: m.RelPath})
		}
		out = append(out, lr)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func runTUI() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	stateDir, err := paths.StateDir()
	if err != nil {
		return err
	}
	model := ui.NewModel(cfg, state.New(stateDir))
	_, err = tea.NewProgram(model, tea.WithAltScreen()).Run()
	return err
}
