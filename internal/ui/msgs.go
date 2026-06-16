// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package ui

import (
	"context"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/japsu/tfmux/internal/discovery"
	"github.com/japsu/tfmux/internal/domain"
	"github.com/japsu/tfmux/internal/gitstatus"
	"github.com/japsu/tfmux/internal/runner"
	"github.com/japsu/tfmux/internal/state"
)

type discoveryMsg struct {
	repos []*domain.Repo
	force bool // re-enumerate workspaces even if a cache exists
	err   error
}

type gitStatusMsg struct {
	repoPath string
	status   domain.GitStatus
}

// runnerEventMsg bridges runner.Event into the tea loop.
type runnerEventMsg struct{ ev runner.Event }

// runsLoadedMsg delivers persisted run records for one module's workspaces.
type runsLoadedMsg struct {
	modulePath string
	runs       map[string]*state.RunRecord // workspace key -> record
	planFiles  map[string]bool             // workspace key -> plan file exists
}

// fingerprintMsg carries the module's current git fingerprint for staleness.
type fingerprintMsg struct {
	modulePath  string
	fingerprint string
}

// logMsg delivers a task log's contents. id is the task id (kind:key) and path
// is its log file, both carried so a follow tick can re-read without
// re-resolving.
type logMsg struct {
	id      string
	path    string
	content string
	err     error
}

// logFollowMsg ticks the live tail of an in-flight task's log.
type logFollowMsg struct {
	id   string
	path string
}

func logFollowTick(id, path string) tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return logFollowMsg{id: id, path: path} })
}

func loadLogCmd(id, path string) tea.Cmd {
	return func() tea.Msg {
		data, err := os.ReadFile(path)
		if err != nil {
			return logMsg{id: id, path: path, err: err}
		}
		return logMsg{id: id, path: path, content: string(data)}
	}
}

type expiredPlansMsg struct{ n int }

type savedMsg struct{ err error }

type statusMsg struct{ text string }

func discoverCmd(roots []string, force bool) tea.Cmd {
	return func() tea.Msg {
		repos, err := discovery.Discover(roots)
		return discoveryMsg{repos: repos, force: force, err: err}
	}
}

func gitStatusCmd(client gitstatus.Client, repoPath string) tea.Cmd {
	return func() tea.Msg {
		return gitStatusMsg{repoPath: repoPath, status: client.Status(context.Background(), repoPath)}
	}
}

// waitForEvent blocks on the runner channel; the model re-issues it after
// every received event to keep the pump alive.
func waitForEvent(ch chan runner.Event) tea.Cmd {
	return func() tea.Msg {
		return runnerEventMsg{ev: <-ch}
	}
}

func loadRunsCmd(store *state.Store, m *domain.Module, workspaces []string) tea.Cmd {
	return func() tea.Msg {
		msg := runsLoadedMsg{
			modulePath: m.Path,
			runs:       map[string]*state.RunRecord{},
			planFiles:  map[string]bool{},
		}
		for _, ws := range workspaces {
			key := m.Path + "//" + ws
			if rec, err := store.LoadRun(m.Path, ws); err == nil && rec != nil {
				msg.runs[key] = rec
			}
			msg.planFiles[key] = store.HasPlanFile(m.Path, ws)
		}
		return msg
	}
}

func fingerprintCmd(m *domain.Module) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		head, _ := gitstatus.Head(ctx, m.Repo.Path)
		dirty, _ := gitstatus.DirtyHash(ctx, m.Repo.Path, m.RelPath)
		return fingerprintMsg{modulePath: m.Path, fingerprint: head + "|" + dirty}
	}
}

func saveRunCmd(store *state.Store, rec *state.RunRecord) tea.Cmd {
	r := *rec // copy: the model may mutate its record after this cmd is built
	return func() tea.Msg {
		return savedMsg{err: store.SaveRun(&r)}
	}
}

func discardPlanCmd(store *state.Store, modulePath, workspace string) tea.Cmd {
	return func() tea.Msg {
		if err := store.DiscardPlan(modulePath, workspace); err != nil {
			return savedMsg{err: err}
		}
		return savedMsg{}
	}
}

func expirePlansCmd(store *state.Store, ttl time.Duration) tea.Cmd {
	return func() tea.Msg {
		n, _ := store.ExpirePlans(ttl)
		_, _ = store.GC()
		return expiredPlansMsg{n: n}
	}
}
