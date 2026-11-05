package app

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// A popup is a box over the page for each choice and each question before a
// change (owner decision, 2026-11-05): ↑↓ and Enter choose, Esc cancels, and
// the letter of an option chooses it at once. The page behind it turns gray.
type popup struct {
	title, text string
	options     []option
	sel         int
	// stale, when set, reports that the data that the popup shows has
	// changed; the popup then closes.
	stale func() bool
}

// option is a choice of a popup; run nil only closes the popup.
type option struct {
	label, sub, key string
	danger          bool
	run             func() tea.Cmd
}

// menu opens a popup with options; the first one is selected.
func (m *model) menu(title, text string, opts []option) {
	m.pop, m.msg = &popup{title: title, text: text, options: opts}, ""
}

// ask asks before a change: Cancel (selected, n) or the change (title, y).
func (m *model) ask(title, text string, run func() tea.Cmd) {
	m.menu(title, text, []option{{label: "Cancel", key: "n"}, {label: title, key: "y", run: run}})
}

// warn asks before a change that cannot be undone: the change is red.
func (m *model) warn(title, text string, run func() tea.Cmd) {
	m.menu(title, text, []option{{label: "Cancel", key: "n"}, {label: title, key: "y", danger: true, run: run}})
}

// popupKey handles a key while a popup is open.
func (m *model) popupKey(k tea.KeyMsg) tea.Cmd {
	p := m.pop
	choose := func(o option) tea.Cmd {
		m.pop = nil
		if o.run == nil {
			m.msg = "Cancelled."
			return nil
		}
		m.msg = "Working …"
		return o.run()
	}
	switch k.String() {
	case "up", "k":
		p.sel = max(p.sel-1, 0)
	case "down", "j":
		p.sel = min(p.sel+1, len(p.options)-1)
	case "esc":
		m.pop, m.msg = nil, "Cancelled."
	case "enter":
		return choose(p.options[p.sel])
	default:
		for _, o := range p.options {
			if o.key != "" && k.String() == o.key {
				return choose(o)
			}
		}
	}
	return nil
}

// box draws the popup.
func (p *popup) box(maxWidth int) []string {
	iw := 30
	for _, o := range p.options {
		iw = max(iw, lipgloss.Width(o.label)+8, lipgloss.Width(o.sub)+6)
	}
	iw = min(max(iw, lipgloss.Width(p.text)+4), maxWidth-2)
	var body []string
	add := func(s string) { body = append(body, s) }
	add("")
	if p.text != "" {
		for _, l := range strings.Split(lipgloss.NewStyle().Width(iw-4).Render(clean(p.text)), "\n") {
			add("  " + strings.TrimRight(l, " "))
		}
		add("")
	}
	for i, o := range p.options {
		bar, label := "   ", boldStyle.Render(o.label)
		if o.danger {
			label = errStyle.Bold(true).Render(o.label)
		}
		if i == p.sel {
			bar = accentStyle.Render(" " + barMark + " ")
			if !o.danger {
				label = accentStyle.Bold(true).Render(o.label)
			}
		}
		key := ""
		if o.key != "" {
			key = mutedStyle.Render(o.key)
		}
		add(bar + spread(label, key, iw-5))
		if o.sub != "" {
			add(bar + mutedStyle.Render(ansi.Truncate(o.sub, iw-5, "…")))
		}
		if o.sub != "" && i < len(p.options)-1 {
			add("")
		}
	}
	add("")
	border := dimStyle.Render
	title := " " + accentStyle.Bold(true).Render(clean(p.title)) + " "
	hint := " " + mutedStyle.Render("↑↓ enter esc") + " "
	out := []string{border("╭─") + title + border(strings.Repeat("─", max(iw-1-lipgloss.Width(title), 0))+"╮")}
	for _, l := range body {
		l = ansi.Truncate(l, iw, "…")
		out = append(out, border("│")+l+strings.Repeat(" ", max(iw-lipgloss.Width(l), 0))+border("│"))
	}
	out = append(out, border("╰"+strings.Repeat("─", max(iw-1-lipgloss.Width(hint), 0)))+hint+border("─╯"))
	return out
}

// overlay draws the popup over the body lines, which turn gray.
func (p *popup) overlay(lines []string, w int) []string {
	box := p.box(min(w-4, 76))
	bw := lipgloss.Width(box[0])
	x := max((w-bw)/2, 0)
	y := max((len(lines)-len(box))/3, 1)
	out := make([]string, len(lines))
	for i, l := range lines {
		gray := dimStyle.Render(cell(ansi.Strip(l), w))
		if j := i - y; j >= 0 && j < len(box) {
			gray = cell(ansi.Cut(gray, 0, x), x) + "\x1b[0m" + box[j] + dimStyle.Render(ansi.Cut(cell(ansi.Strip(l), w), x+bw, w))
		}
		out[i] = gray
	}
	return out
}
