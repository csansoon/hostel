package app

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/csansoon/hostel/internal/protocol"
)

// lobby lists the platforms, each with its free machines and its queue.
type lobby struct{ sel int }

func (p *lobby) title() string { return "Lobby" }

// queued returns the queued leases of a platform in queue order (a lease
// that the scheduler has not placed yet comes last).
func queued(st protocol.Status, platform string) []protocol.LeaseStatus {
	var out []protocol.LeaseStatus
	for _, l := range st.Leases {
		if l.State == protocol.LeaseQueued && l.Platform == platform {
			out = append(out, l)
		}
	}
	slices.SortStableFunc(out, func(a, b protocol.LeaseStatus) int {
		if (a.Position == 0) != (b.Position == 0) {
			if a.Position == 0 {
				return 1
			}
			return -1
		}
		return a.Position - b.Position
	})
	return out
}

// onlineMachines are the machines of a platform that are not offline: the
// Lobby leaves out the offline ones (owner decision, 2026-11-03).
func onlineMachines(m *model, p protocol.Platform) []protocol.MachineStatus {
	var out []protocol.MachineStatus
	for _, key := range p.Machines {
		if i := slices.IndexFunc(m.st.Machines, func(ms protocol.MachineStatus) bool { return ms.Key == key }); i >= 0 && online(m.st, m.st.Machines[i]) {
			out = append(out, m.st.Machines[i])
		}
	}
	return out
}

// freeMachines counts the machines of a platform that can take a lease now.
func freeMachines(m *model, p protocol.Platform) int {
	n := 0
	for _, ms := range onlineMachines(m, p) {
		if machineFree(m.st, ms, p.Name) {
			n++
		}
	}
	return n
}

func waitText(seconds int) string {
	if seconds <= 0 {
		return "now"
	}
	return "~" + ago(time.Duration(seconds)*time.Second)
}

func (p *lobby) body(m *model) string {
	switch {
	case m.platformsErr != "" && !m.platformsRead:
		return "\n  " + errStyle.Render(clean("Cannot read the platforms: "+m.platformsErr)) + "\n"
	case !m.platformsRead:
		return "\n  Reading the platforms …\n"
	case len(m.platforms) == 0:
		return "\n  No platform has a machine yet. An admin adds a machine in Front desk › Rooms.\n"
	}
	stale := ""
	if m.platformsErr != "" {
		stale = "  " + errStyle.Render(clean("! Cannot read the platforms: "+m.platformsErr+" (older data below; trying again)")) + "\n"
	}
	w := contentWidth(m)
	var b strings.Builder
	b.WriteString(stale + "\n")
	for i, pl := range m.platforms {
		total := len(onlineMachines(m, pl))
		sub := clean(pl.Description) + " · " + plural(len(pl.Machines), "machine")
		b.WriteString(item(w, i == p.sel, clean(pl.Name), sub, m.vacancy(pl), waitLine(pl, total)) + "\n\n")
	}
	return b.String()
}

// vacancy is a platform's free machines: ● 2 vacant, ◉ all occupied, or no
// machine online; it glows when it changes.
func (m *model) vacancy(pl protocol.Platform) string {
	free, total := freeMachines(m, pl), len(onlineMachines(m, pl))
	s := state{fmt.Sprintf("%d vacant", free), colGreen, ""}
	glyph := "●"
	switch {
	case total == 0:
		return mutedStyle.Render("○ no machine online")
	case free == 0:
		s, glyph = state{"all occupied", colBlue, ""}, "◉"
	}
	if m.glowing("p:" + pl.Name) {
		return fg(s.color).Render(glyph) + " " + lipgloss.NewStyle().Background(s.color).Foreground(colInk).Render(s.word)
	}
	return fg(s.color).Render(glyph + " " + s.word)
}

// waitLine is a platform's waiting line in a few words.
func waitLine(pl protocol.Platform, online int) string {
	if pl.Queue == 0 {
		return mutedStyle.Render("nobody waiting")
	}
	next := "next room " + waitText(pl.EstimatedWaitSeconds)
	if online == 0 {
		next = "no machine online"
	}
	return fg(colYellow).Render(spin("◴", "◷", "◶", "◵")+fmt.Sprintf(" %d waiting", pl.Queue)) + mutedStyle.Render(" · "+next)
}

func (p *lobby) hint(m *model) string {
	if p.sel >= len(m.platforms) {
		return ""
	}
	pl := m.platforms[p.sel]
	return pl.Name + ": " + pl.Description
}

func (p *lobby) keys(m *model) []binding { return []binding{{"↑↓", "move"}, {"enter", "open"}} }

func (p *lobby) key(m *model, k tea.KeyMsg) tea.Cmd {
	if k.String() == "enter" && p.sel < len(m.platforms) {
		return m.open(&platformPage{name: m.platforms[p.sel].Name, onMachines: true})
	}
	p.sel = move(p.sel, len(m.platforms), k.String())
	return nil
}

// platformPage is one platform: its machines and its full queue, with the
// queue actions.
type platformPage struct {
	name string
	// sel is the selected lease (its ID: the queue order changes).
	sel string
	// onMachines: the Machines card has the keyboard (tab), not the Waiting
	// line; machineSel is the key of its selected machine (the order
	// follows the states).
	onMachines bool
	machineSel string
}

func (p *platformPage) title() string { return p.name }

func (p *platformPage) platform(m *model) (protocol.Platform, bool) {
	i := slices.IndexFunc(m.platforms, func(pl protocol.Platform) bool { return pl.Name == p.name })
	if i < 0 {
		return protocol.Platform{}, false
	}
	return m.platforms[i], true
}

// selected returns the queue and the index of the selected lease (the first
// one when the selected lease left the queue).
func (p *platformPage) selected(m *model) ([]protocol.LeaseStatus, int) {
	q := queued(m.st, p.name)
	i := slices.IndexFunc(q, func(l protocol.LeaseStatus) bool { return l.ID == p.sel })
	if i < 0 && len(q) > 0 {
		i = 0
		p.sel = q[0].ID
	}
	return q, i
}

func (p *platformPage) body(m *model) string {
	pl, ok := p.platform(m)
	if !ok {
		return "\n  This platform is not on the server any more.\n"
	}
	now := time.Now()
	w := contentWidth(m)
	machines := p.machines(m, pl)
	q, qsel := p.selected(m)
	if len(q) == 0 {
		p.onMachines = true
	}
	var b strings.Builder
	if m.platformsErr != "" {
		b.WriteString(margin + errStyle.Render(clean("! Cannot read the platforms: "+m.platformsErr+" (older data below; trying again)")) + "\n")
	}

	typical := "typical stay not known yet"
	if pl.TypicalLeaseSeconds > 0 {
		typical = "typical stay " + ago(time.Duration(pl.TypicalLeaseSeconds)*time.Second)
	}
	b.WriteString(head(clean(pl.Name), clean(pl.Description), m.vacancy(pl)+"     "+waitLine(pl, len(machines))+"     "+mutedStyle.Render(typical)))

	// The machines: one line each.
	b.WriteString(heading("Machines", w) + "\n\n")
	if len(machines) == 0 {
		b.WriteString(margin + "   " + mutedStyle.Render("No machine for "+clean(pl.Name)+" is online.") + "\n")
	}
	_, msel := p.selectedMachine(m, pl)
	nameW := 0
	for _, ms := range machines {
		nameW = max(nameW, lipgloss.Width(clean(ms.Key)))
	}
	for i, ms := range machines {
		s := machineState(m.st, ms, now)
		word := m.word("m:"+ms.Key, s)
		line := s.dot() + " " + cell(boldStyle.Render(clean(ms.Key)), nameW) + "   " + cell(word, len("No new leases")) + "   " + mutedStyle.Render(s.detail)
		b.WriteString(row(w, p.onMachines && i == msel, line) + "\n")
	}

	// The waiting line: two lines each.
	b.WriteString("\n\n" + heading("Waiting line", w) + "\n\n")
	if len(q) == 0 {
		b.WriteString(margin + "   " + mutedStyle.Render("Nobody is waiting.") + "\n\n")
	}
	for i, l := range q {
		pos := "–"
		if l.Position > 0 {
			pos = fmt.Sprint(l.Position)
		}
		prio := l.Priority + " priority"
		switch l.Priority {
		case "high":
			prio = fg(colRed).Render("▲ high priority")
		case "low":
			prio = mutedStyle.Render("▼ low priority")
		}
		title := pos + "  " + clean(l.User) + "  " + fmt.Sprintf("%q", clean(l.Task))
		sub := "   " + prio + mutedStyle.Render(" · waiting "+ago(now.Sub(l.CreatedAt)))
		right := fg(colYellow).Render(spin("◴", "◷", "◶", "◵") + " room in " + waitText(l.EstimatedWaitSeconds))
		b.WriteString(item(w, !p.onMachines && i == qsel, title, sub, right, "") + "\n\n")
	}

	// The devices.
	if len(pl.Devices) > 0 {
		b.WriteString("\n\n" + heading("Devices", w) + "\n\n")
		var ds []string
		for _, d := range pl.Devices {
			ds = append(ds, fg(colBlue).Render(clean(d)))
		}
		b.WriteString(margin + "   " + strings.Join(ds, "    ") + "      " + mutedStyle.Render(fmt.Sprintf("up to %d at the same time", pl.MaxDevices)) + "\n")
	}
	return b.String()
}

// selectedMachine returns the machines of the card and the index of the
// selected one (the first when it is gone).
func (p *platformPage) selectedMachine(m *model, pl protocol.Platform) ([]protocol.MachineStatus, int) {
	ms := p.machines(m, pl)
	i := slices.IndexFunc(ms, func(x protocol.MachineStatus) bool { return x.Key == p.machineSel })
	if i < 0 && len(ms) > 0 {
		i, p.machineSel = 0, ms[0].Key
	}
	return ms, i
}

// machines are the online machines that serve the platform, in state order.
func (p *platformPage) machines(m *model, pl protocol.Platform) []protocol.MachineStatus {
	return byState(m.st, onlineMachines(m, pl), time.Now())
}

func (p *platformPage) hint(m *model) string {
	pl, ok := p.platform(m)
	if !ok {
		return ""
	}
	if p.onMachines {
		if ms, i := p.selectedMachine(m, pl); i >= 0 {
			s := machineState(m.st, ms[i], time.Now())
			return ms[i].Key + ": " + strings.TrimPrefix(s.word+" · "+s.detail, " · ")
		}
		return ""
	}
	q, i := p.selected(m)
	if i < 0 {
		return "The Waiting line: the agents that wait for a room for " + pl.Name + ", first in, first out inside each priority."
	}
	l := q[i]
	h := fmt.Sprintf("%s · %q · from %s", clean(l.User), clean(l.Task), clean(l.Guest))
	if l.Reason != "" {
		h += " · waits because: " + clean(l.Reason)
	}
	return h
}

func (p *platformPage) canEnd(m *model, l protocol.LeaseStatus) bool {
	return m.admin() || (m.st.User != "" && l.User == m.st.User)
}

func (p *platformPage) keys(m *model) []binding {
	ks := []binding{{"↑↓", "move"}, {"tab", "next section"}}
	if p.onMachines {
		pl, _ := p.platform(m)
		if ms, _ := p.selectedMachine(m, pl); len(ms) > 0 {
			ks = append(ks, binding{"enter", "open the machine"})
		}
		return ks
	}
	q, sel := p.selected(m)
	if sel >= 0 && len(p.actions(m, q, sel)) > 0 {
		ks = append(ks, binding{"enter", "actions"})
	}
	return ks
}

// actions are what the user can do with the selected lease of the waiting
// line; each one runs the key of its letter.
func (p *platformPage) actions(m *model, q []protocol.LeaseStatus, i int) []option {
	l := q[i]
	// The options keep the lease and the queue of this moment; the popup
	// closes when the queue changes (cancelOnChange).
	act := func(label, key string, run func()) option {
		return option{label: label, key: key, run: func() tea.Cmd { run(); return nil }}
	}
	var out []option
	if p.canEnd(m, l) {
		out = append(out, act("End the lease", "e", func() { p.endLease(m, l) }))
	}
	if m.admin() {
		if i > 0 {
			out = append(out, act("Move up", "K", func() { p.moveUp(m, q, i) }))
		}
		if i < len(q)-1 {
			out = append(out, act("Move down", "J", func() { p.moveDown(m, q, i) }))
		}
		out = append(out, act("Change the priority", "p", func() { p.priority(m, l) }))
	}
	return out
}

// runeKey is the key message of a letter.
func runeKey(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func (p *platformPage) key(m *model, k tea.KeyMsg) tea.Cmd {
	if k.String() == "tab" || k.String() == "shift+tab" {
		p.onMachines = !p.onMachines
		return nil
	}
	if p.onMachines {
		pl, _ := p.platform(m)
		ms, i := p.selectedMachine(m, pl)
		// Down from the last machine goes on to the waiting line.
		if q, _ := p.selected(m); (k.String() == "down" || k.String() == "j") && i == len(ms)-1 && len(q) > 0 {
			p.onMachines, p.sel = false, q[0].ID
			return nil
		}
		if i < 0 {
			return nil
		}
		if k.String() == "enter" {
			return m.open(&machinePage{machineKey: ms[i].Key})
		}
		p.machineSel = ms[move(i, len(ms), k.String())].Key
		return nil
	}
	q, i := p.selected(m)
	if i < 0 {
		return nil
	}
	l := q[i]
	switch k.String() {
	case "enter":
		if acts := p.actions(m, q, i); len(acts) > 0 {
			m.menu(clean(l.User), fmt.Sprintf("%q · %s priority", clean(l.Task), l.Priority), acts)
			p.cancelOnChange(m, q)
		}
	case "up", "k":
		if i > 0 {
			p.sel = q[i-1].ID
		} else if pl, _ := p.platform(m); len(p.machines(m, pl)) > 0 {
			ms := p.machines(m, pl)
			p.onMachines, p.machineSel = true, ms[len(ms)-1].Key
		}
	case "down", "j":
		if i < len(q)-1 {
			p.sel = q[i+1].ID
		}
	case "e":
		if p.canEnd(m, l) {
			p.endLease(m, l)
		}
	case "shift+up", "K":
		if m.admin() && i > 0 {
			p.moveUp(m, q, i)
		}
	case "shift+down", "J":
		if m.admin() && i < len(q)-1 {
			p.moveDown(m, q, i)
		}
	case "p":
		if m.admin() {
			p.priority(m, l)
		}
	}
	return nil
}

func (p *platformPage) endLease(m *model, l protocol.LeaseStatus) {
	m.warn("End the lease", fmt.Sprintf("End the lease of %s (%q)?", clean(l.User), clean(l.Task)), func() tea.Cmd {
		return m.do("POST", "/v1/leases/"+l.ID+"/release", nil, "The lease of "+clean(l.User)+" ended.")
	})
}

func (p *platformPage) moveUp(m *model, q []protocol.LeaseStatus, i int) {
	l, before := q[i], q[i-1]
	m.ask("Move up", fmt.Sprintf("Move %s (%q) up, before %s?", clean(l.User), clean(l.Task), clean(before.User)), func() tea.Cmd {
		return m.do("POST", "/v1/leases/"+l.ID+"/move", protocol.MoveRequest{Before: before.ID}, "Moved.")
	})
	p.cancelOnChange(m, q)
}

func (p *platformPage) moveDown(m *model, q []protocol.LeaseStatus, i int) {
	l, next := q[i], q[i+1]
	// Down: before the lease after the next one; at the end of the queue,
	// the next lease moves up instead (the API has no "last").
	move := func() tea.Cmd {
		return m.do("POST", "/v1/leases/"+next.ID+"/move", protocol.MoveRequest{Before: l.ID}, "Moved.")
	}
	if i+2 < len(q) {
		after := q[i+2]
		move = func() tea.Cmd {
			return m.do("POST", "/v1/leases/"+l.ID+"/move", protocol.MoveRequest{Before: after.ID}, "Moved.")
		}
	}
	m.ask("Move down", fmt.Sprintf("Move %s (%q) down, after %s?", clean(l.User), clean(l.Task), clean(next.User)), move)
	p.cancelOnChange(m, q)
}

func (p *platformPage) priority(m *model, l protocol.LeaseStatus) {
	set := func(prio string) func() tea.Cmd {
		return func() tea.Cmd {
			return m.do("POST", "/v1/leases/"+l.ID+"/priority", protocol.PriorityRequest{Priority: prio}, "Priority of "+clean(l.User)+": "+prio+".")
		}
	}
	m.menu("Priority", fmt.Sprintf("Priority of %s (%q), now %s.", clean(l.User), clean(l.Task), l.Priority), []option{
		{label: "High", sub: "Before the normal and low leases", key: "h", run: set("high")},
		{label: "Normal", sub: "The default", key: "n", run: set("normal")},
		{label: "Low", sub: "After the high and normal leases", key: "l", run: set("low")},
	})
}

// cancelOnChange makes the open question stale when the queue order is not q
// any more: a move uses the neighbours that the question names.
func (p *platformPage) cancelOnChange(m *model, q []protocol.LeaseStatus) {
	ids := func(ls []protocol.LeaseStatus) []string {
		var out []string
		for _, l := range ls {
			out = append(out, l.ID)
		}
		return out
	}
	want, name := ids(q), p.name
	m.pop.stale = func() bool { return !slices.Equal(want, ids(queued(m.st, name))) }
}
