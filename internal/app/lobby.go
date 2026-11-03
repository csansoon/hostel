package app

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

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
		return "\n  No platform has a machine yet. An admin adds a machine in Front desk.\n"
	}
	stale := ""
	if m.platformsErr != "" {
		stale = "  " + errStyle.Render(clean("! Cannot read the platforms: "+m.platformsErr+" (older data below; trying again)")) + "\n"
	}
	var rows [][]string
	for _, pl := range m.platforms {
		free, total := freeMachines(m, pl), len(onlineMachines(m, pl))
		vacant := fg(colGreen).Render(fmt.Sprintf("%d of %d vacant", free, total))
		switch {
		case total == 0:
			vacant = mutedStyle.Render("no machine online")
		case free == 0:
			vacant = fg(colBlue).Render(fmt.Sprintf("0 of %d vacant", total))
		}
		line := mutedStyle.Render("nobody waiting")
		if pl.Queue > 0 {
			line = fg(colYellow).Render(fmt.Sprintf("%d waiting", pl.Queue)) + mutedStyle.Render(" · next room "+waitText(pl.EstimatedWaitSeconds))
		}
		rows = append(rows, []string{clean(pl.Name), vacant, line})
	}
	return stale + "\n" + table(rows, p.sel, []string{"PLATFORM", "MACHINES", "WAITING LINE"})
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
		return m.open(&platformPage{name: m.platforms[p.sel].Name})
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
	w := m.screenWidth()
	var about strings.Builder
	if len(pl.Devices) > 0 {
		about.WriteString(field("Devices", chips(pl.Devices, colBlue)+mutedStyle.Render(fmt.Sprintf("  up to %d", pl.MaxDevices))) + "\n")
	}
	typical := mutedStyle.Render("not known yet")
	if pl.TypicalLeaseSeconds > 0 {
		typical = boldStyle.Render(ago(time.Duration(pl.TypicalLeaseSeconds) * time.Second))
	}
	about.WriteString(field("Typical stay", typical) + "\n")
	next := fg(colGreen).Render("now")
	if pl.EstimatedWaitSeconds > 0 {
		next = fg(colYellow).Render(waitText(pl.EstimatedWaitSeconds))
	}
	about.WriteString(field("Next room", next))

	lw, rw, wide := side(w)
	machines := p.machines(m, pl)
	var mrows [][]string
	for _, ms := range machines {
		s := machineState(m.st, ms, now)
		mrows = append(mrows, []string{s.dot() + " " + clean(ms.Key), fg(s.color).Render(s.word)})
	}
	msel := -1
	if p.onMachines {
		_, msel = p.selectedMachine(m, pl)
	}
	free := freeMachines(m, pl)
	vacantColor := colGreen
	if free == 0 {
		vacantColor = colBlue
	}
	mbody := table(mrows, msel, nil)
	if len(mrows) == 0 {
		mbody = mutedStyle.Render("No machine for " + clean(pl.Name) + " is online.")
	}
	mcard := card(boldStyle.Render("Machines")+" "+fg(vacantColor).Render(fmt.Sprintf("%d of %d vacant", free, len(machines))), mbody, lw, p.onMachines)

	q, sel := p.selected(m)
	var qbody string
	if len(q) == 0 {
		qbody = mutedStyle.Render("Nobody is waiting.")
	} else {
		var rows [][]string
		for _, l := range q {
			pos := "-"
			if l.Position > 0 {
				pos = fmt.Sprintf("%d", l.Position)
			}
			prio := mutedStyle.Render("○ " + l.Priority)
			switch l.Priority {
			case "high":
				prio = fg(colRed).Render("● high")
			case "normal":
				prio = fg(colYellow).Render("● normal")
			}
			rows = append(rows, []string{accentStyle.Render(pos), clean(l.User), clean(l.Task), prio, ago(now.Sub(l.CreatedAt)), fg(colYellow).Render(waitText(l.EstimatedWaitSeconds))})
		}
		if p.onMachines {
			sel = -1
		}
		qbody = table(rows, sel, []string{"#", "GUEST", "TASK", "PRIORITY", "WAITING", "ROOM IN"})
	}
	qcard := card(boldStyle.Render("Waiting line")+" "+fg(colYellow).Render(fmt.Sprint(len(q))), qbody, rw, !p.onMachines)

	head := boldStyle.Render(clean(pl.Name)) + mutedStyle.Render(" · "+clean(pl.Description))
	stale := ""
	if m.platformsErr != "" {
		stale = "  " + errStyle.Render(clean("! Cannot read the platforms: "+m.platformsErr+" (older data below; trying again)")) + "\n"
	}
	return stale + "\n" + card(head, about.String(), w, false) + "\n" + join(mcard, qcard, wide)
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
	ks := []binding{{"↑↓", "move"}, {"tab", "next card"}}
	if p.onMachines {
		return append(ks, binding{"enter", "open the machine"})
	}
	q, sel := p.selected(m)
	if sel >= 0 && p.canEnd(m, q[sel]) {
		ks = append(ks, binding{"e", "end lease"})
	}
	if m.admin() && len(q) > 0 {
		if len(q) > 1 {
			ks = append(ks, binding{"⇧↑↓", "reorder"})
		}
		ks = append(ks, binding{"p", "priority"})
	}
	return ks
}

func (p *platformPage) key(m *model, k tea.KeyMsg) tea.Cmd {
	if k.String() == "tab" || k.String() == "shift+tab" {
		p.onMachines = !p.onMachines
		return nil
	}
	if p.onMachines {
		pl, _ := p.platform(m)
		ms, i := p.selectedMachine(m, pl)
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
	case "up", "k":
		if i > 0 {
			p.sel = q[i-1].ID
		}
	case "down", "j":
		if i < len(q)-1 {
			p.sel = q[i+1].ID
		}
	case "e":
		if p.canEnd(m, l) {
			m.ask(fmt.Sprintf("End the lease of %s (%q)?", clean(l.User), clean(l.Task)), func() tea.Cmd {
				return m.do("POST", "/v1/leases/"+l.ID+"/release", nil, "The lease of "+clean(l.User)+" ended.")
			})
		}
	case "shift+up", "K":
		if m.admin() && i > 0 {
			before := q[i-1]
			m.ask(fmt.Sprintf("Move %s (%q) up, before %s?", clean(l.User), clean(l.Task), clean(before.User)), func() tea.Cmd {
				return m.do("POST", "/v1/leases/"+l.ID+"/move", protocol.MoveRequest{Before: before.ID}, "Moved.")
			})
			p.cancelOnChange(m, q)
		}
	case "shift+down", "J":
		if m.admin() && i < len(q)-1 {
			next := q[i+1]
			// Down: before the lease after the next one; at the end of the
			// queue, the next lease moves up instead (the API has no "last").
			move := func() tea.Cmd {
				return m.do("POST", "/v1/leases/"+next.ID+"/move", protocol.MoveRequest{Before: l.ID}, "Moved.")
			}
			if i+2 < len(q) {
				after := q[i+2]
				move = func() tea.Cmd {
					return m.do("POST", "/v1/leases/"+l.ID+"/move", protocol.MoveRequest{Before: after.ID}, "Moved.")
				}
			}
			m.ask(fmt.Sprintf("Move %s (%q) down, after %s?", clean(l.User), clean(l.Task), clean(next.User)), move)
			p.cancelOnChange(m, q)
		}
	case "p":
		if m.admin() {
			set := func(prio string) func() tea.Cmd {
				return func() tea.Cmd {
					return m.do("POST", "/v1/leases/"+l.ID+"/priority", protocol.PriorityRequest{Priority: prio}, "Priority of "+clean(l.User)+": "+prio+".")
				}
			}
			m.choose(fmt.Sprintf("Priority of %s (%q), now %s: h high · n normal · l low · another key cancels", clean(l.User), clean(l.Task), l.Priority),
				map[string]func() tea.Cmd{"h": set("high"), "n": set("normal"), "l": set("low")})
		}
	}
	return nil
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
	m.confirm.stale = func() bool { return !slices.Equal(want, ids(queued(m.st, name))) }
}
