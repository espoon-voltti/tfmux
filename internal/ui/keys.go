// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package ui

import "github.com/charmbracelet/bubbles/key"

type keyMap struct {
	Up, Down, Left, Right key.Binding
	CollapseOthers        key.Binding
	ExpandAll             key.Binding
	PageUp, PageDown      key.Binding
	Mark                  key.Binding
	Plan, PlanAll         key.Binding
	Apply                 key.Binding
	View                  key.Binding
	Discard               key.Binding
	Cancel, CancelAll     key.Binding
	Tasks                 key.Binding
	Ignore, ShowIgnored   key.Binding
	InitUpgrade           key.Binding
	Refresh, Rediscover   key.Binding
	RefreshWorkspaces     key.Binding
	Filter                key.Binding
	Help                  key.Binding
	Quit                  key.Binding
	Esc                   key.Binding
}

var keys = keyMap{
	Up:                key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
	Down:              key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
	PageUp:            key.NewBinding(key.WithKeys("pgup", "ctrl+u"), key.WithHelp("PgUp", "page up")),
	PageDown:          key.NewBinding(key.WithKeys("pgdown", "ctrl+d"), key.WithHelp("PgDn", "page down")),
	Left:              key.NewBinding(key.WithKeys("left", "h"), key.WithHelp("←/h", "collapse")),
	Right:             key.NewBinding(key.WithKeys("right", "l"), key.WithHelp("→/l", "expand")),
	CollapseOthers:    key.NewBinding(key.WithKeys("H", "shift+left"), key.WithHelp("H/⇧←", "collapse others")),
	ExpandAll:         key.NewBinding(key.WithKeys("L", "shift+right"), key.WithHelp("L/⇧→", "expand all")),
	Mark:              key.NewBinding(key.WithKeys(" "), key.WithHelp("space", "mark")),
	Plan:              key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "plan marked/cursor")),
	PlanAll:           key.NewBinding(key.WithKeys("P"), key.WithHelp("P", "plan all")),
	Apply:             key.NewBinding(key.WithKeys("A"), key.WithHelp("A", "apply (tmux)")),
	View:              key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "view log / attach")),
	Discard:           key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "discard plan")),
	Cancel:            key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "cancel/kill task")),
	CancelAll:         key.NewBinding(key.WithKeys("X"), key.WithHelp("X", "cancel all queued")),
	Tasks:             key.NewBinding(key.WithKeys("T"), key.WithHelp("T", "task pane")),
	Ignore:            key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "toggle ignore")),
	ShowIgnored:       key.NewBinding(key.WithKeys("Z"), key.WithHelp("Z", "show ignored")),
	InitUpgrade:       key.NewBinding(key.WithKeys("I"), key.WithHelp("I", "init -upgrade (module/repo)")),
	Refresh:           key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh status")),
	Rediscover:        key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "re-discover")),
	RefreshWorkspaces: key.NewBinding(key.WithKeys("w"), key.WithHelp("w", "refresh workspaces")),
	Filter:            key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
	Help:              key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
	Quit:              key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	Esc:               key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "close/back")),
}

// ShortHelp is the always-visible hint line.
func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Plan, k.Apply, k.View, k.Ignore, k.Filter, k.Help, k.Quit}
}

// FullHelp feeds the expanded help overlay.
func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Up, k.Down, k.PageUp, k.PageDown, k.Left, k.Right, k.CollapseOthers, k.ExpandAll, k.Mark, k.Filter},
		{k.Plan, k.PlanAll, k.Cancel, k.View, k.Discard},
		{k.Apply, k.InitUpgrade, k.Tasks, k.CancelAll},
		{k.Ignore, k.ShowIgnored, k.Refresh, k.RefreshWorkspaces, k.Rediscover},
		{k.Help, k.Esc, k.Quit},
	}
}
