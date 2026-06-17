// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/lipgloss"
)

// newHelp builds a help model with our brighter palette.
func newHelp() help.Model {
	h := help.New()
	h.Styles = helpStyles()
	return h
}

// helpStyles overrides the bubbles help component's default palette, whose
// stock grays render too dark on dark terminals.
func helpStyles() help.Styles {
	return help.Styles{
		Ellipsis:       styleHelpSep,
		ShortKey:       styleHelpKey,
		ShortDesc:      styleHelpDesc,
		ShortSeparator: styleHelpSep,
		FullKey:        styleHelpKey,
		FullDesc:       styleHelpDesc,
		FullSeparator:  styleHelpSep,
	}
}

var (
	styleTitle       = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("62")).Padding(0, 1)
	styleCursor      = lipgloss.NewStyle().Background(lipgloss.Color("240")).Foreground(lipgloss.Color("231"))
	styleRepo        = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("75"))
	styleModule      = lipgloss.NewStyle().Foreground(lipgloss.Color("253"))
	styleWS          = lipgloss.NewStyle().Foreground(lipgloss.Color("251"))
	styleDim         = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	styleGood        = lipgloss.NewStyle().Foreground(lipgloss.Color("78"))
	styleChanges     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	styleError       = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	styleRunning     = lipgloss.NewStyle().Foreground(lipgloss.Color("81"))
	styleStale       = lipgloss.NewStyle().Foreground(lipgloss.Color("213"))
	styleMarked      = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	styleIgnored     = lipgloss.NewStyle().Foreground(lipgloss.Color("248")).Italic(true)
	styleStatusBar   = lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Background(lipgloss.Color("238")).Padding(0, 1)
	styleHelpLine    = lipgloss.NewStyle().Foreground(lipgloss.Color("249"))
	styleHelpKey     = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	styleHelpDesc    = lipgloss.NewStyle().Foreground(lipgloss.Color("246"))
	styleHelpSep     = lipgloss.NewStyle().Foreground(lipgloss.Color("242"))
	styleDetailTitle = lipgloss.NewStyle().Bold(true).Underline(true)
	styleHeaderCtx   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("252"))

	stylePlanAdd     = lipgloss.NewStyle().Foreground(lipgloss.Color("78"))
	stylePlanDestroy = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	stylePlanChange  = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	stylePlanHead    = lipgloss.NewStyle().Bold(true)
)

// humanDur renders "3m", "2h", "5d" style relative times. Sub-minute ages
// read as "0m" rather than ticking seconds, to keep fresh rows from flickering.
func humanDur(since time.Time) string {
	d := time.Since(since)
	switch {
	case d < time.Minute:
		return "0m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// colorizePlanLog re-applies terraform-style colors to captured -no-color
// output, line by line.
func colorizePlanLog(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		switch {
		case strings.HasPrefix(trimmed, "-/+ "), strings.HasPrefix(trimmed, "+/- "):
			lines[i] = stylePlanChange.Render(line)
		case strings.HasPrefix(trimmed, "+ "):
			lines[i] = stylePlanAdd.Render(line)
		case strings.HasPrefix(trimmed, "- "):
			lines[i] = stylePlanDestroy.Render(line)
		case strings.HasPrefix(trimmed, "~ "):
			lines[i] = stylePlanChange.Render(line)
		case strings.HasPrefix(line, "Plan:"), strings.HasPrefix(line, "No changes"),
			strings.HasPrefix(line, "Changes to Outputs"):
			lines[i] = stylePlanHead.Render(line)
		case strings.HasPrefix(line, "Error:"), strings.HasPrefix(trimmed, "Error:"):
			lines[i] = styleError.Render(line)
		case strings.HasPrefix(line, "Warning:"), strings.HasPrefix(trimmed, "Warning:"):
			lines[i] = stylePlanChange.Render(line)
		}
	}
	return strings.Join(lines, "\n")
}
