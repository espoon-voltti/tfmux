package ui

import "github.com/charmbracelet/bubbles/key"

type keyMap struct {
	Up, Down, Left, Right key.Binding
	Mark                  key.Binding
	Plan, PlanAll         key.Binding
	Apply                 key.Binding
	Attach                key.Binding
	View                  key.Binding
	Discard               key.Binding
	Cancel                key.Binding
	Ignore, ShowIgnored   key.Binding
	InitUpgrade           key.Binding
	Refresh, Rediscover   key.Binding
	Filter                key.Binding
	Help                  key.Binding
	Quit                  key.Binding
	Esc                   key.Binding
}

var keys = keyMap{
	Up:          key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
	Down:        key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
	Left:        key.NewBinding(key.WithKeys("left", "h"), key.WithHelp("←/h", "collapse")),
	Right:       key.NewBinding(key.WithKeys("right", "l"), key.WithHelp("→/l", "expand")),
	Mark:        key.NewBinding(key.WithKeys(" "), key.WithHelp("space", "mark")),
	Plan:        key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "plan marked/cursor")),
	PlanAll:     key.NewBinding(key.WithKeys("P"), key.WithHelp("P", "plan all")),
	Apply:       key.NewBinding(key.WithKeys("A"), key.WithHelp("A", "apply (tmux)")),
	Attach:      key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "attach tmux")),
	View:        key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "view plan log")),
	Discard:     key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "discard plan")),
	Cancel:      key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "cancel job")),
	Ignore:      key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "toggle ignore")),
	ShowIgnored: key.NewBinding(key.WithKeys("Z"), key.WithHelp("Z", "show ignored")),
	InitUpgrade: key.NewBinding(key.WithKeys("I"), key.WithHelp("I", "init -upgrade")),
	Refresh:     key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh status")),
	Rediscover:  key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "re-discover")),
	Filter:      key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
	Help:        key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
	Quit:        key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	Esc:         key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "close/back")),
}

// ShortHelp is the always-visible hint line.
func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Plan, k.Apply, k.View, k.Ignore, k.Filter, k.Help, k.Quit}
}

// FullHelp feeds the expanded help overlay.
func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Up, k.Down, k.Left, k.Right, k.Mark, k.Filter},
		{k.Plan, k.PlanAll, k.Cancel, k.View, k.Discard},
		{k.Apply, k.Attach, k.InitUpgrade},
		{k.Ignore, k.ShowIgnored, k.Refresh, k.Rediscover},
		{k.Help, k.Esc, k.Quit},
	}
}
