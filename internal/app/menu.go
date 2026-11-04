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
// description for the footer, and its page (nil for Update, which asks and
// runs).
type menuEntry struct {
	icon, label, hint string
	count             func(m *model) string
	open              func() page
}

func menuEntries(m *model) []menuEntry {
	entries := []menuEntry{
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
	if tag := m.updateAvailable(); tag != "" {
		entries = append(entries, menuEntry{"⇪", "Update",
			fmt.Sprintf("Update: install Hostel %s on this computer (it has %s); then open hostel again.", tag, m.version),
			func(m *model) string { return fg(colYellow).Render(tag + " available") }, nil})
	}
	return entries
}

func (p *mainMenu) title() string { return "" }

func (p *mainMenu) body(m *model) string {
	if !m.loaded && m.err == "" {
		return "\n  Reading the status …\n"
	}
	var rows [][]string
	for i, e := range menuEntries(m) {
		rows = append(rows, []string{fmt.Sprintf("%d  %s  %s", i+1, accentStyle.Render(e.icon), e.label), e.count(m)})
	}
	return "\n" + table(rows, p.sel, nil)
}

func (p *mainMenu) hint(m *model) string {
	es := menuEntries(m)
	return es[min(p.sel, len(es)-1)].hint
}

func (p *mainMenu) keys(m *model) []binding {
	return []binding{{"↑↓", "move"}, {"enter", "open"}, {"1-3", "go to"}}
}

func (p *mainMenu) key(m *model, k tea.KeyMsg) tea.Cmd {
	entries := menuEntries(m)
	p.sel = min(p.sel, len(entries)-1)
	if k.String() == "enter" {
		if e := entries[p.sel]; e.open != nil {
			return m.open(e.open())
		}
		tag := m.updateAvailable()
		m.ask(fmt.Sprintf("Install Hostel %s on this computer?", tag), func() tea.Cmd { return m.runSelfUpdate(tag) })
		return nil
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
