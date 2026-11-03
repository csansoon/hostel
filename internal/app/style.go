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

// chips draws names in brackets: [ios] [macos].
func chips(names []string, c lipgloss.Color) string {
	var out []string
	for _, n := range names {
		out = append(out, fg(c).Render("["+clean(n)+"]"))
	}
	return strings.Join(out, " ")
}

// gauge draws a bar of width cells for a share (0 to 1).
func gauge(share float64, width int, c lipgloss.Color) string {
	share = min(max(share, 0), 1)
	n := int(share*float64(width) + 0.5)
	return fg(c).Render(strings.Repeat("█", n)) + dimStyle.Render(strings.Repeat("░", width-n))
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

// card draws a box with rounded borders and its title in the top border. A
// focused card (it has the keyboard) has an amber border.
func card(title, body string, width int, focused bool) string {
	border := dimStyle
	if focused {
		border = accentStyle
	}
	inner := max(width-4, 4)
	head := " " + ansi.Truncate(title, max(width-6, 1), "…") + " "
	fill := max(width-3-lipgloss.Width(head), 0)
	var b strings.Builder
	b.WriteString(border.Render("╭─") + head + border.Render(strings.Repeat("─", fill)+"╮") + "\n")
	for _, l := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		l = ansi.Truncate(l, inner, "…")
		b.WriteString(border.Render("│") + " " + l + strings.Repeat(" ", inner-lipgloss.Width(l)) + " " + border.Render("│") + "\n")
	}
	b.WriteString(border.Render("╰" + strings.Repeat("─", max(width-2, 0)) + "╯"))
	return b.String()
}

// side puts two cards side by side when the screen is wide, else one under
// the other. It returns the widths to use for the two cards.
func side(width int) (left, right int, wide bool) {
	if width >= 100 {
		left = width * 2 / 5
		return left, width - left - 1, true
	}
	return width, width, false
}

func join(a, b string, wide bool) string {
	if wide {
		return lipgloss.JoinHorizontal(lipgloss.Top, a, " ", b)
	}
	return a + "\n" + b
}

// field is a "Label   value" line of a card.
func field(label, value string) string {
	return mutedStyle.Render(fmt.Sprintf("%-12s", label)) + " " + value
}

// table draws rows with aligned columns; the selected row has the marker. The
// cells may be styled.
func table(rows [][]string, sel int, header []string) string {
	var widths []int
	measure := func(cells []string) {
		for i, c := range cells {
			for len(widths) <= i {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], lipgloss.Width(c))
		}
	}
	measure(header)
	for _, r := range rows {
		measure(r)
	}
	line := func(cells []string) string {
		var out []string
		for i, c := range cells {
			if i < len(cells)-1 {
				c += strings.Repeat(" ", widths[i]-lipgloss.Width(c))
			}
			out = append(out, c)
		}
		return strings.TrimRight(strings.Join(out, "  "), " ")
	}
	var b strings.Builder
	if header != nil {
		var hs []string
		for _, h := range header {
			hs = append(hs, dimStyle.Render(h))
		}
		b.WriteString("  " + line(hs) + "\n")
	}
	for n, r := range rows {
		mark := "  "
		if n == sel {
			mark = accentStyle.Render(selMark)
		}
		b.WriteString(mark + line(r) + "\n")
	}
	return b.String()
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
// line (with "❯ ") when the body is longer; "…" marks cut lines.
func window(lines []string, room int) []string {
	if len(lines) <= room {
		return lines
	}
	sel := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(ansi.Strip(l), selMark) })
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

// selMark marks the selected row; window() looks for it to scroll.
const selMark = "❯ "

// clean replaces control characters (also newlines and tabs) with spaces:
// names and tasks come from users and must not move the screen. It also
// replaces the selection mark, so that user text cannot look selected.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || (r >= 0x7f && r <= 0x9f):
			return ' '
		case r == '❯':
			return '>'
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
