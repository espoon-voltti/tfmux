// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

var (
	styleTitle       = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("62")).Padding(0, 1)
	styleCursor      = lipgloss.NewStyle().Background(lipgloss.Color("240")).Foreground(lipgloss.Color("231"))
	styleRepo        = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("75"))
	styleModule      = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	styleWS          = lipgloss.NewStyle().Foreground(lipgloss.Color("249"))
	styleDim         = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styleGood        = lipgloss.NewStyle().Foreground(lipgloss.Color("70"))
	styleChanges     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	styleError       = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	styleRunning     = lipgloss.NewStyle().Foreground(lipgloss.Color("81"))
	styleStale       = lipgloss.NewStyle().Foreground(lipgloss.Color("213"))
	styleMarked      = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	styleIgnored     = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Italic(true)
	styleStatusBar   = lipgloss.NewStyle().Foreground(lipgloss.Color("248")).Background(lipgloss.Color("236")).Padding(0, 1)
	styleHelpLine    = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	styleDetailTitle = lipgloss.NewStyle().Bold(true).Underline(true)

	stylePlanAdd     = lipgloss.NewStyle().Foreground(lipgloss.Color("70"))
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
