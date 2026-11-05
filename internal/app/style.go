package app

import (
	"fmt"
	"slices"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// The palette: ANSI 256 colors, readable on dark and light terminals. Each
// color has one meaning (owner decision, 2026-11-03). An adaptive color would
// ask the terminal for its background and wait for the answer, so the app
// does not use one. Lip Gloss turns colors off with NO_COLOR.
var (
	colAccent = lipgloss.Color("214") // Hostel, the selection, keys
	colGreen  = lipgloss.Color("78")  // vacant, free
	colBlue   = lipgloss.Color("75")  // occupied, in use
	colYellow = lipgloss.Color("221") // waiting, getting ready
	colRed    = lipgloss.Color("203") // broken, errors
	colMuted  = lipgloss.Color("245") // secondary text
	colDim    = lipgloss.Color("240") // borders, lines
	colChip   = lipgloss.Color("238") // the background of a key
	colInk    = lipgloss.Color("16")  // text on a colored background
)

var (
	boldStyle   = lipgloss.NewStyle().Bold(true)
	mutedStyle  = lipgloss.NewStyle().Foreground(colMuted)
	dimStyle    = lipgloss.NewStyle().Foreground(colDim)
	accentStyle = lipgloss.NewStyle().Foreground(colAccent)
	errStyle    = lipgloss.NewStyle().Foreground(colRed)
	promptStyle = lipgloss.NewStyle().Foreground(colYellow).Bold(true)
	signStyle   = lipgloss.NewStyle().Background(colAccent).Foreground(colInk).Bold(true).Padding(0, 1)
	keyStyle    = lipgloss.NewStyle().Background(colChip).Foreground(lipgloss.Color("252")).Padding(0, 1)
)

func fg(c lipgloss.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(c) }

// pill is a short state on a colored background: " Occupied ".
func pill(text string, c lipgloss.Color) string {
	return lipgloss.NewStyle().Background(c).Foreground(colInk).Padding(0, 1).Render(text)
}

// gauge draws a bar of width cells for a share (0 to 1).
func gauge(share float64, width int, c lipgloss.Color) string {
	share = min(max(share, 0), 1)
	n := int(share*float64(width) + 0.5)
	return fg(c).Render(strings.Repeat("━", n)) + fg(colChip).Render(strings.Repeat("━", width-n))
}

// shareColor is green, then yellow, then red as a share grows.
func shareColor(share float64) lipgloss.Color {
	switch {
	case share >= 0.9:
		return colRed
	case share >= 0.7:
		return colYellow
	}
	return colGreen
}

// field is a "Label   value" line of a card.
func field(label, value string) string {
	return mutedStyle.Render(fmt.Sprintf("%-12s", label)) + " " + value
}

// fitScreen cuts the lines to the width and the body to the height, so that
// the footer stays at the bottom of the screen.
func fitScreen(body, foot string, width, height int) string {
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	footLines := strings.Split(foot, "\n")
	if height > 0 {
		room := max(height-len(footLines), 1)
		lines = window(lines, room)
		for len(lines) < room {
			lines = append(lines, "")
		}
	}
	all := append(lines, footLines...)
	if width > 0 {
		for i, l := range all {
			all[i] = ansi.Truncate(l, width, "…")
		}
	}
	return strings.Join(all, "\n")
}

// window keeps at most room lines: the first lines, and around the selected
// item (with the bar) when the body is longer; "…" marks cut lines.
func window(lines []string, room int) []string {
	if len(lines) <= room {
		return lines
	}
	sel := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(ansi.Strip(l), barMark) })
	start := 0
	if sel >= 0 {
		start = min(max(sel-room/2, 0), len(lines)-room)
	}
	out := slices.Clone(lines[start : start+room])
	more := mutedStyle.Render("…")
	if start > 0 && sel != start {
		out[0] = more
	}
	if start+room < len(lines) && sel != start+room-1 {
		out[room-1] = more
	}
	return out
}

// barMark marks the selected list item; window() looks for it to scroll.
const barMark = "▌"

// clean replaces control characters (also newlines and tabs) with spaces:
// names and tasks come from users and must not move the screen. It also
// replaces the selection mark, so that user text cannot look selected.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || (r >= 0x7f && r <= 0x9f):
			return ' '
		case r == '▌':
			return '|'
		}
		return r
	}, s)
}

// move changes a selection by one step inside n items.
func move(sel, n int, k string) int {
	switch k {
	case "up", "k":
		return max(sel-1, 0)
	case "down", "j":
		return min(sel+1, max(n-1, 0))
	}
	return sel
}

// margin is the left margin of the body.
const margin = "  "

// contentWidth is the width of the body between the margins.
func contentWidth(m *model) int { return min(m.screenWidth()-2*len(margin), 80) }

// spread puts left at the start and right at the end of width cells; a long
// left part gives way to the right one.
func spread(left, right string, width int) string {
	if right == "" {
		return ansi.Truncate(left, width, "…")
	}
	left = ansi.Truncate(left, max(width-lipgloss.Width(right)-2, 1), "…")
	return left + strings.Repeat(" ", max(width-lipgloss.Width(left)-lipgloss.Width(right), 2)) + right
}

// item is a list item: a title line and a dim second line, each with a part
// on the right. The selected item has an amber bar on the left.
func item(width int, sel bool, title, sub, right, rightSub string) string {
	bar, ttl := "   ", boldStyle.Render(title)
	if sel {
		bar, ttl = accentStyle.Render(" "+barMark+" "), accentStyle.Bold(true).Render(title)
	}
	out := margin + bar + spread(ttl, right, width-3)
	if sub != "" || rightSub != "" {
		out += "\n" + margin + bar + spread(mutedStyle.Render(sub), rightSub, width-3)
	}
	return out
}

// row is a one-line list item, with the bar when it is selected.
func row(width int, sel bool, text string) string {
	bar := "   "
	if sel {
		bar = accentStyle.Render(" " + barMark + " ")
	}
	return margin + bar + ansi.Truncate(text, width-3, "…")
}

// heading is the title of a section: small capitals and a thin line.
func heading(title string, width int) string {
	t := mutedStyle.Bold(true).Render(strings.ToUpper(title))
	return margin + t + " " + dimStyle.Render(strings.Repeat("─", max(width-lipgloss.Width(t)-1, 0)))
}

// cell pads a styled text to width cells.
func cell(s string, width int) string {
	return s + strings.Repeat(" ", max(width-lipgloss.Width(s), 0))
}

// head is the top of a detail page: the name, what it is, and a summary
// line.
func head(name, sub, summary string) string {
	out := "\n" + margin + "   " + accentStyle.Bold(true).Render(name) + "\n"
	if sub != "" {
		out += margin + "   " + mutedStyle.Render(sub) + "\n"
	}
	if summary != "" {
		out += "\n" + margin + "   " + summary + "\n"
	}
	return out + "\n\n"
}

// line is a body line at the indent of the list items.
func line(s string) string { return margin + "   " + s + "\n" }

// note is a dim line at the indent of the list items.
func note(s string) string { return line(mutedStyle.Render(s)) }

// section is a heading with an empty line after it; more puts two empty
// lines before it.
func section(title string, width int, more bool) string {
	pre := ""
	if more {
		pre = "\n\n"
	}
	return pre + heading(title, width) + "\n\n"
}
