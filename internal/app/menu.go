package app

import (
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/csansoon/hostel/internal/protocol"
)

// mainMenu is the first page when the computer is connected.
type mainMenu struct{ sel int }

func newMainMenu() *mainMenu { return &mainMenu{} }

// menuEntry is a section of the main menu: its icon, its live numbers, the
// description for the footer, and its page (nil for Update, which asks and
// runs).
type menuEntry struct {
	icon, label, sub, hint string
	count                  func(m *model) string
	open                   func() page
}

func menuEntries(m *model) []menuEntry {
	entries := []menuEntry{
		{"⌂", "Lobby", "Platforms, and who waits for each one", "Lobby: the platforms, what each can run, which machines are free, and who waits.",
			func(m *model) string {
				if n := waiting(m.st); n > 0 {
					return fg(colYellow).Render(spin("◴", "◷", "◶", "◵") + fmt.Sprintf(" %d waiting", n))
				}
				return mutedStyle.Render("nobody waiting")
			}, func() page { return &lobby{} }},
		{"▦", "Rooms", "Machines, and who uses each one now", "Rooms: the machines where agents run, and who uses each one now.",
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
				return fg(colGreen).Render(fmt.Sprintf("● %d vacant", vacant)) + "   " +
					fg(colBlue).Render(fmt.Sprintf("◉ %d occupied", used))
			}, func() page { return &rooms{} }},
		{"⚿", "Front desk", "This server, people, invites, new machines", "Front desk: this server, the people on it, invites, new machines, and check out.",
			func(m *model) string { return "" }, func() page { return newFrontDesk() }},
	}
	if tag := m.updateAvailable(); tag != "" {
		entries = append(entries, menuEntry{"⇪", "Update", "Install " + tag + " on this computer",
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
	w := contentWidth(m)
	var b strings.Builder
	// The building needs about 15 lines; a short screen shows the entries
	// only.
	if m.height == 0 || m.height >= 34 {
		b.WriteString("\n" + building(m) + "\n\n")
	}
	for i, e := range menuEntries(m) {
		b.WriteString(item(w, i == p.sel, e.icon+"  "+e.label, "   "+e.sub, e.count(m), "") + "\n\n")
	}
	return b.String()
}

// The building has a fixed set of windows (owner decision, 2026-11-05): the
// share of lit windows follows the share of occupied machines, so it works
// for one machine and for fifty. Which windows light is a fixed order for
// each server, so that a change lights or darkens one window at a time.
const (
	floorWindows = 7
	upperFloors  = 2
	// windowCount: the upper floors, and three windows on each side of the
	// door.
	windowCount = floorWindows*upperFloors + 6
)

// windowState is what a window shows.
type windowState int

const (
	windowDark   windowState = iota // a vacant machine
	windowLit                       // an occupied machine
	windowReady                     // a machine that gets ready (it flickers)
	windowClosed                    // an offline or broken machine
)

// windows returns the state of each window for the machines' states.
func windows(serverID string, words []string) []windowState {
	out := make([]windowState, windowCount)
	if len(words) == 0 {
		return out
	}
	n := map[windowState]int{}
	for _, w := range words {
		switch w {
		case "Occupied":
			n[windowLit]++
		case "Getting ready":
			n[windowReady]++
		case "Vacant":
		default:
			n[windowClosed]++
		}
	}
	// share is the number of windows for count machines: at least one when
	// count > 0, and all of them when every machine has that state.
	share := func(count int) int {
		if count == 0 {
			return 0
		}
		return max(1, (count*windowCount+len(words)/2)/len(words))
	}
	h := fnv.New64a()
	h.Write([]byte(serverID))
	order := rand.New(rand.NewPCG(h.Sum64(), 1)).Perm(windowCount)
	// Lit and flickering windows from the start of the order, closed ones
	// from the end: a change of one kind does not move the others.
	i := 0
	for _, k := range []windowState{windowLit, windowReady} {
		for c := share(n[k]); c > 0 && i < windowCount; c-- {
			out[order[i]] = k
			i++
		}
	}
	for c, j := share(n[windowClosed]), windowCount-1; c > 0 && j >= i; c, j = c-1, j-1 {
		out[order[j]] = windowClosed
	}
	return out
}

func (w windowState) draw() string {
	switch w {
	case windowLit:
		return fg(colBlue).Render("██")
	case windowReady:
		return fg(colYellow).Render(spin("██", "▓▓", "▒▒", "▓▓"))
	case windowClosed:
		return dimStyle.Render("▒▒")
	}
	return fg(colChip).Render("██")
}

// building draws the hostel with its windows, and the server and the user
// beside it.
func building(m *model) string {
	now := time.Now()
	var words []string
	count := map[string]int{}
	for _, ms := range m.st.Machines {
		w := machineState(m.st, ms, now).word
		words = append(words, w)
		switch w {
		case "Occupied", "Vacant", "Getting ready":
			count[w]++
		default:
			count["Offline"]++
		}
	}
	ws := windows(m.st.ServerID, words)
	win := func(i int) string { return ws[i].draw() }
	line := dimStyle.Render
	wall := line("│")
	sign := accentStyle.Render

	const inner = floorWindows*4 + 2 // "  ██" for each window, and 2 spaces
	art := []string{
		sign("╭" + strings.Repeat("─", inner+4) + "╮"),
		sign("│") + signStyle.Width(inner+4).Align(lipgloss.Center).Render("H  O  S  T  E  L") + sign("│"),
		sign("╰─┬" + strings.Repeat("─", inner) + "┬─╯"),
	}
	k := 0
	for f := 0; f < upperFloors; f++ {
		fl := ""
		for j := 0; j < floorWindows; j++ {
			fl += "  " + win(k)
			k++
		}
		art = append(art, "  "+wall+fl+"  "+wall, "  "+wall+strings.Repeat(" ", inner)+wall)
	}
	side := func() string {
		s := ""
		for j := 0; j < 3; j++ {
			s += "  " + win(k)
			k++
		}
		return s
	}
	left := side()
	right := side()
	art = append(art,
		"  "+wall+left+line(" ╭──╮")+right[1:]+"  "+wall,
		"  "+wall+strings.Repeat(" ", 12)+line(" │  │ ")+strings.Repeat(" ", 12)+wall,
		line("▁▁┷"+strings.Repeat("━", 13)+"┷━━┷"+strings.Repeat("━", 13)+"┷▁▁"))

	role := clean(m.st.Role)
	if m.admin() {
		role = fg(colBlue).Render(role)
	}
	var legend []string
	for _, k := range []struct {
		word string
		w    windowState
	}{{"Occupied", windowLit}, {"Vacant", windowDark}, {"Getting ready", windowReady}, {"Offline", windowClosed}} {
		if n := count[k.word]; n > 0 {
			legend = append(legend, k.w.draw()+" "+mutedStyle.Render(fmt.Sprintf("%d %s", n, strings.ToLower(k.word))))
		}
	}
	info := []string{
		boldStyle.Render(clean(m.guest.URL())),
		mutedStyle.Render("Hostel " + clean(m.info.Version)),
		"",
		mutedStyle.Render("You are ") + clean(m.st.User) + mutedStyle.Render(" · ") + role,
		"",
	}
	info = append(info, legend...)
	top := max((len(art)-len(info))/2, 0)
	var b strings.Builder
	for i, l := range art {
		b.WriteString(margin + " " + l)
		if j := i - top; j >= 0 && j < len(info) {
			b.WriteString(strings.Repeat(" ", inner+12-lipgloss.Width(l)) + info[j])
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
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
