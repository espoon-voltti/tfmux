package ui

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/japsu/tfmux/internal/config"
	"github.com/japsu/tfmux/internal/state"
	"github.com/japsu/tfmux/internal/tftest"
)

// TestEndToEnd drives the real program loop: discovery of a fixture git repo,
// workspace enumeration through the runner with the fake terraform binary,
// planning a workspace, and quitting.
func TestEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "demo-repo")
	mod := filepath.Join(repo, "envs", "prod")
	if err := os.MkdirAll(mod, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", "-b", "main", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	tf := `terraform {
  backend "s3" {}
}
`
	if err := os.WriteFile(filepath.Join(mod, "main.tf"), []byte(tf), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := tftest.Write(t, t.TempDir())
	t.Setenv("TFMUX_FAKE_PLAN_EXIT", "2")
	showJSON := filepath.Join(t.TempDir(), "show.json")
	if err := os.WriteFile(showJSON,
		[]byte(`{"format_version":"1.2","resource_changes":[{"address":"a","change":{"actions":["create"]}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TFMUX_FAKE_SHOW_JSON", showJSON)

	cfg := config.Default()
	cfg.Roots = []string{root}
	cfg.TerraformBin = bin
	model := NewModel(cfg, state.New(t.TempDir()))

	tm := teatest.NewTestModel(t, model, teatest.WithInitialTermSize(110, 30))

	// discovery + enumeration: repo, module and workspaces appear
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte("demo-repo")) && bytes.Contains(b, []byte("staging"))
	}, teatest.WithDuration(15*time.Second))

	// move to the first workspace row (repo, module, ws...) and plan it
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})

	// outstanding changes badge appears once the plan lands
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte("+1 ~0 -0"))
	}, teatest.WithDuration(15*time.Second))

	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	tm.WaitFinished(t, teatest.WithFinalTimeout(10*time.Second))
}
