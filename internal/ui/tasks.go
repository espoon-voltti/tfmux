// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package ui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/espoon-voltti/tfmux/internal/runner"
)

// sortedTasks lists in-flight tasks for the pane: running first, then by
// scheduling priority, then oldest first.
func (m *Model) sortedTasks() []*taskState {
	out := make([]*taskState, 0, len(m.tasks))
	for _, ts := range m.tasks {
		out = append(out, ts)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.running != b.running {
			return a.running
		}
		if pa, pb := a.kind.Priority(), b.kind.Priority(); pa != pb {
			return pa > pb
		}
		return a.started.Before(b.started)
	})
	return out
}

func (m *Model) clampTaskCursor() {
	if n := len(m.tasks); m.taskCursor >= n {
		m.taskCursor = n - 1
	}
	if m.taskCursor < 0 {
		m.taskCursor = 0
	}
}

// updateTaskKey handles input while the task pane is focused.
func (m *Model) updateTaskKey(msg tea.KeyMsg) tea.Cmd {
	switch {
	case key.Matches(msg, keys.Esc), key.Matches(msg, keys.Tasks):
		m.focus = focusTree
	case key.Matches(msg, keys.Up):
		if m.taskCursor > 0 {
			m.taskCursor--
		}
	case key.Matches(msg, keys.Down):
		if m.taskCursor < len(m.tasks)-1 {
			m.taskCursor++
		}
	case key.Matches(msg, keys.View):
		return m.viewSelectedTask()
	case key.Matches(msg, keys.Cancel):
		m.cancelSelectedTask()
	case key.Matches(msg, keys.CancelAll):
		m.cancelQueuedTasks()
	}
	return nil
}

// viewSelectedTask runs the unified view/attach action on the selected task:
// attach for a live apply, otherwise follow the task's log.
func (m *Model) viewSelectedTask() tea.Cmd {
	tasks := m.sortedTasks()
	if m.taskCursor < 0 || m.taskCursor >= len(tasks) {
		return nil
	}
	ts := tasks[m.taskCursor]
	switch ts.kind {
	case runner.KindPlan, runner.KindApply:
		return m.viewOrAttach(ts.key)
	case runner.KindEnumerate, runner.KindInit:
		return m.openLog(ts.kind, ts.key)
	}
	return nil
}

// cancelSelectedTask cancels (or, for a running apply, asks to kill) the task
// under the pane cursor.
func (m *Model) cancelSelectedTask() {
	tasks := m.sortedTasks()
	if m.taskCursor < 0 || m.taskCursor >= len(tasks) {
		return
	}
	ts := tasks[m.taskCursor]
	if ts.kind == runner.KindApply && ts.running {
		// killing a live apply terminates terraform mid-flight — confirm first
		m.confirmKill = runner.TaskID(ts.kind, ts.key)
		return
	}
	m.runner.Cancel(ts.key)
	m.forgetTasks(ts.key)
	m.clampTaskCursor()
}

// killTask closes a running apply's tmux window. The runner's poll then sees
// the window vanish and reports the apply aborted (state unknown).
func (m *Model) killTask(id string) {
	ts := m.tasks[id]
	if ts == nil {
		return
	}
	if err := m.tmux.KillWindow(ts.windowID); err != nil {
		m.status = "kill failed: " + err.Error()
		return
	}
	m.status = "killed apply window — will be marked aborted, re-plan"
	m.clampTaskCursor()
}

// cancelQueuedTasks drops every queued (not-yet-running) task at once.
func (m *Model) cancelQueuedTasks() {
	n := 0
	for id, ts := range m.tasks {
		if ts.running {
			continue
		}
		m.runner.Cancel(ts.key)
		delete(m.tasks, id)
		n++
	}
	if n > 0 {
		m.status = fmt.Sprintf("canceled %d queued task(s)", n)
	}
	m.clampTaskCursor()
}

func (m *Model) taskLabel(ts *taskState) string {
	switch ts.kind {
	case runner.KindPlan, runner.KindApply:
		if i := strings.LastIndex(ts.key, "//"); i >= 0 {
			if mod := m.findModule(ts.key[:i]); mod != nil {
				return mod.Repo.Name + "/" + mod.RelPath + " · " + ts.key[i+2:]
			}
		}
	default:
		if mod := m.findModule(ts.key); mod != nil {
			return mod.Repo.Name + "/" + mod.RelPath
		}
	}
	return ts.key
}

// renderTaskPane is the full-screen list of in-flight tasks (toggled with T).
func (m *Model) renderTaskPane(height int) string {
	tasks := m.sortedTasks()
	m.clampTaskCursor()

	var b strings.Builder
	b.WriteString(styleDetailTitle.Render(fmt.Sprintf("Tasks (%d)", len(tasks))))
	b.WriteString("\n\n")
	if len(tasks) == 0 {
		b.WriteString(styleDim.Render("  no active tasks"))
		b.WriteString("\n\n")
		b.WriteString(styleHelpLine.Render("  esc/T close"))
		return b.String()
	}

	listH := height - 4 // title + blank + blank + footer
	if listH < 1 {
		listH = 1
	}
	start := 0
	if m.taskCursor >= listH {
		start = m.taskCursor - listH + 1
	}
	end := start + listH
	if end > len(tasks) {
		end = len(tasks)
	}
	for i := start; i < end; i++ {
		b.WriteString(m.renderTaskLine(tasks[i], i == m.taskCursor, m.width))
		b.WriteByte('\n')
	}
	b.WriteString("\n")
	b.WriteString(styleHelpLine.Render("  enter view/attach · x cancel/kill · X cancel all queued · esc/T close"))
	return b.String()
}

func (m *Model) renderTaskLine(ts *taskState, selected bool, width int) string {
	var badge string
	if ts.running {
		badge = styleRunning.Render(m.spinner.View() + " running")
	} else {
		badge = styleDim.Render("◌ queued ")
	}
	line := fmt.Sprintf("  %s  %s  %s", badge, styleModule.Render(fmt.Sprintf("%-9s", ts.kind.String())), m.taskLabel(ts))
	line += "  " + styleDim.Render(humanDur(ts.started))
	if ts.kind == runner.KindApply && ts.running {
		line += styleDim.Render("  (enter: attach)")
	}
	if selected {
		plain := ansi.Strip(line)
		if pad := width - lipglossWidth(plain); pad > 0 {
			plain += strings.Repeat(" ", pad)
		}
		return styleCursor.Render(plain)
	}
	return line
}
