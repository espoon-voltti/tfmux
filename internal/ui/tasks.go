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
// scheduling priority, then oldest first, then by task id as a final,
// deterministic tiebreaker. That last clause matters: m.tasks is a map, so
// its iteration order is randomized per call — without a total order, two
// separate calls to sortedTasks (e.g. one to render the pane, another
// moments later to act on "the row under the cursor") could legally disagree
// on the order of two tied tasks, making the highlighted row and the
// acted-on task different rows within the same keypress.
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
		if !a.started.Equal(b.started) {
			return a.started.Before(b.started)
		}
		return runner.TaskID(a.kind, a.key) < runner.TaskID(b.kind, b.key)
	})
	return out
}

// taskCursorIndex resolves the pane's selection (tracked by stable task id,
// not position) against a freshly-sorted list. Task order shifts on its own
// — a queued task flipping to running jumps to the top — so a bare index
// would end up pointing at a different task than the one last highlighted;
// see reflow for the tree's version of the same problem.
func (m *Model) taskCursorIndex(tasks []*taskState) (int, bool) {
	for i, ts := range tasks {
		if runner.TaskID(ts.kind, ts.key) == m.taskCursorID {
			return i, true
		}
	}
	return 0, false
}

// clampTaskCursor resets the selection to the first task when it no longer
// resolves (its task finished, or there is no selection yet).
func (m *Model) clampTaskCursor() {
	tasks := m.sortedTasks()
	if len(tasks) == 0 {
		m.taskCursorID = ""
		return
	}
	if _, ok := m.taskCursorIndex(tasks); !ok {
		m.taskCursorID = runner.TaskID(tasks[0].kind, tasks[0].key)
	}
}

// updateTaskKey handles input while the task pane is focused.
func (m *Model) updateTaskKey(msg tea.KeyMsg) tea.Cmd {
	switch {
	case key.Matches(msg, keys.Esc), key.Matches(msg, keys.Tasks), key.Matches(msg, keys.Quit):
		m.focus = focusTree
	case key.Matches(msg, keys.Up):
		tasks := m.sortedTasks()
		if i, ok := m.taskCursorIndex(tasks); ok && i > 0 {
			m.taskCursorID = runner.TaskID(tasks[i-1].kind, tasks[i-1].key)
		}
	case key.Matches(msg, keys.Down):
		tasks := m.sortedTasks()
		if i, ok := m.taskCursorIndex(tasks); ok {
			if i+1 < len(tasks) {
				m.taskCursorID = runner.TaskID(tasks[i+1].kind, tasks[i+1].key)
			}
		} else if len(tasks) > 0 {
			m.taskCursorID = runner.TaskID(tasks[0].kind, tasks[0].key)
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
	i, ok := m.taskCursorIndex(tasks)
	if !ok {
		return nil
	}
	ts := tasks[i]
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
	i, ok := m.taskCursorIndex(tasks)
	if !ok {
		return
	}
	ts := tasks[i]
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
	windowID, ok := m.tmux.WindowFor(ts.token)
	if !ok {
		m.status = "apply window is already gone"
		return
	}
	if err := m.tmux.KillWindow(windowID); err != nil {
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
	if len(tasks) == 0 {
		return styleDim.Render("  no active tasks")
	}

	cursor, _ := m.taskCursorIndex(tasks)

	listH := height
	if listH < 1 {
		listH = 1
	}
	start := 0
	if cursor >= listH {
		start = cursor - listH + 1
	}
	end := start + listH
	if end > len(tasks) {
		end = len(tasks)
	}
	for i := start; i < end; i++ {
		b.WriteString(m.renderTaskLine(tasks[i], i == cursor, m.width))
		b.WriteByte('\n')
	}
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
