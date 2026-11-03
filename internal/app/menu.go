package app

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/csansoon/hostel/internal/protocol"
)

// mainMenu is the first page when the computer is connected.
type mainMenu struct{ sel int }

func newMainMenu() *mainMenu { return &mainMenu{} }

// menuEntry is a section of the main menu: its icon, its live numbers, the
// description for the footer, and its page.
type menuEntry struct {
	icon, label, hint string
	count             func(m *model) string
	open              func() page
}

func menuEntries() []menuEntry {
	return []menuEntry{
		{"⌂", "Lobby", "Lobby: the platforms, what each can run, which machines are free, and who waits.",
			func(m *model) string {
				if n := waiting(m.st); n > 0 {
					return fg(colYellow).Render(fmt.Sprintf("%d waiting", n))
				}
				return mutedStyle.Render("nobody waiting")
			}, func() page { return &lobby{} }},
		{"▦", "Rooms", "Rooms: the machines where agents run, and who uses each one now.",
			func(m *model) string {
				vacant, used := 0, 0
				for _, ms := range m.st.Machines {
					switch machineState(m.st, ms, time.Now()).word {
					case "Vacant":
						vacant++
					case "Occupied":
						used++
					}
				}
				return fg(colGreen).Render(fmt.Sprintf("%d vacant", vacant)) + mutedStyle.Render(" · ") +
					fg(colBlue).Render(fmt.Sprintf("%d occupied", used))
			}, func() page { return &rooms{} }},
		{"⚿", "Front desk", "Front desk: this server, the people on it, invites, new machines, and check out.",
			func(m *model) string { return "" }, func() page { return newFrontDesk() }},
	}
}

func (p *mainMenu) title() string { return "" }

func (p *mainMenu) body(m *model) string {
	if !m.loaded && m.err == "" {
		return "\n  Reading the status …\n"
	}
	var rows [][]string
	for i, e := range menuEntries() {
		rows = append(rows, []string{fmt.Sprintf("%d  %s  %s", i+1, accentStyle.Render(e.icon), e.label), e.count(m)})
	}
	return "\n" + table(rows, p.sel, nil)
}

func (p *mainMenu) hint(m *model) string { return menuEntries()[p.sel].hint }

func (p *mainMenu) keys(m *model) []binding {
	return []binding{{"↑↓", "move"}, {"enter", "open"}, {"1-3", "go to"}}
}

func (p *mainMenu) key(m *model, k tea.KeyMsg) tea.Cmd {
	entries := menuEntries()
	if k.String() == "enter" {
		return m.open(entries[p.sel].open())
	}
	p.sel = move(p.sel, len(entries), k.String())
	return nil
}

// waiting counts the queued leases.
func waiting(st protocol.Status) int {
	n := 0
	for _, l := range st.Leases {
		if l.State == protocol.LeaseQueued {
			n++
		}
	}
	return n
}
