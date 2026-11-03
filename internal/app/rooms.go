package app

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/csansoon/hostel/internal/protocol"
)

// detailsInterval is the shortest time between two reads of a machine's
// details (each read opens SSH to its rooms).
const detailsInterval = 10 * time.Second

// rooms lists the machines with their platforms and who uses them.
// rooms lists the machines in state order (online first); sel is the key of
// the selected machine, so that the selection follows it when the order
// changes.
type rooms struct{ sel string }

// selected returns the machines in order and the index of the selected one.
func (p *rooms) selected(m *model) ([]protocol.MachineStatus, int) {
	ms := byState(m.st, m.st.Machines, time.Now())
	i := slices.IndexFunc(ms, func(x protocol.MachineStatus) bool { return x.Key == p.sel })
	if i < 0 && len(ms) > 0 {
		i, p.sel = 0, ms[0].Key
	}
	return ms, i
}

func (p *rooms) title() string { return "Rooms" }

func (p *rooms) body(m *model) string {
	if len(m.st.Machines) == 0 {
		return "\n  The server has no machines yet. An admin adds one in Front desk.\n"
	}
	now := time.Now()
	machines, sel := p.selected(m)
	var rows [][]string
	for _, ms := range machines {
		s := machineState(m.st, ms, now)
		guest := ""
		var leases []protocol.LeaseStatus
		for _, r := range machineRooms(m.st, ms.Key) {
			leases = append(leases, roomLeases(m.st, r.ID)...)
		}
		if len(leases) > 0 {
			guest = mutedStyle.Render(leaseLine(leases[0], now))
			if len(leases) > 1 {
				guest += mutedStyle.Render(fmt.Sprintf(" (+%d)", len(leases)-1))
			}
		}
		rows = append(rows, []string{s.dot() + " " + clean(ms.Key), fg(s.color).Render(s.word), chips(machinePlatforms(ms), colBlue), guest})
	}
	return "\n" + table(rows, sel, []string{"ROOM", "STATUS", "RUNS", "GUEST"})
}

// machinePlatforms are the OS and the platforms of a machine.
func machinePlatforms(ms protocol.MachineStatus) []string {
	return append([]string{ms.OS}, ms.Platforms...)
}

func (p *rooms) hint(m *model) string {
	machines, i := p.selected(m)
	if i < 0 {
		return ""
	}
	ms := machines[i]
	s := machineState(m.st, ms, time.Now())
	return ms.Key + ": " + strings.TrimPrefix(s.word+" · "+s.detail, " · ")
}

func (p *rooms) keys(m *model) []binding { return []binding{{"↑↓", "move"}, {"enter", "open"}} }

func (p *rooms) key(m *model, k tea.KeyMsg) tea.Cmd {
	machines, i := p.selected(m)
	if i < 0 {
		return nil
	}
	if k.String() == "enter" {
		return m.open(&machinePage{machineKey: machines[i].Key})
	}
	p.sel = machines[move(i, len(machines), k.String())].Key
	return nil
}

// machinePage is one machine: its hardware and live use, and each lease with
// its jobs; the admin actions.
type machinePage struct {
	machineKey string
	// details of each room of the machine, by room name.
	details map[string]protocol.RoomDetails
	errs    []string
	loaded  time.Time
	loading bool
	// sel is the selected lease (its ID).
	sel string
}

type detailsMsg struct {
	gen     int
	key     string
	details map[string]protocol.RoomDetails
	errs    []string
}

func (p *machinePage) title() string { return p.machineKey }

func (p *machinePage) machine(m *model) (protocol.MachineStatus, bool) {
	i := slices.IndexFunc(m.st.Machines, func(ms protocol.MachineStatus) bool { return ms.Key == p.machineKey })
	if i < 0 {
		return protocol.MachineStatus{}, false
	}
	return m.st.Machines[i], true
}

func (p *machinePage) load(m *model) tea.Cmd {
	if p.loading || time.Since(p.loaded) < detailsInterval && p.details != nil {
		return nil
	}
	p.loading = true
	c, gen, key := m.c, m.gen, p.machineKey
	var names []string
	for _, r := range machineRooms(m.st, key) {
		names = append(names, r.Name)
	}
	return func() tea.Msg {
		out := detailsMsg{gen: gen, key: key, details: map[string]protocol.RoomDetails{}}
		var mu sync.Mutex
		var wg sync.WaitGroup
		// The rooms in parallel, each with its own deadline: each read can
		// take seconds (jobs and live use over SSH).
		for _, name := range names {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
				defer cancel()
				var d protocol.RoomDetails
				err := c.Do(ctx, "GET", "/v1/rooms/"+name, nil, &d)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					out.errs = append(out.errs, name+": "+err.Error())
					return
				}
				out.details[name] = d
			}()
		}
		wg.Wait()
		return out
	}
}

func (p *machinePage) other(m *model, msg tea.Msg) tea.Cmd {
	if d, ok := msg.(detailsMsg); ok && d.gen == m.gen && d.key == p.machineKey {
		p.details, p.errs, p.loaded, p.loading = d.details, d.errs, time.Now(), false
	}
	return nil
}

// leases returns the leases on the machine's rooms (from the status, so that
// they follow each change) and the index of the selected one.
func (p *machinePage) leases(m *model) ([]protocol.LeaseStatus, int) {
	var out []protocol.LeaseStatus
	for _, r := range machineRooms(m.st, p.machineKey) {
		out = append(out, roomLeases(m.st, r.ID)...)
	}
	i := slices.IndexFunc(out, func(l protocol.LeaseStatus) bool { return l.ID == p.sel })
	if i < 0 && len(out) > 0 {
		i, p.sel = 0, out[0].ID
	}
	return out, i
}

func gb(n int64) string { return strconv.FormatFloat(float64(n)/(1<<30), 'f', 1, 64) + " GB" }

func (p *machinePage) body(m *model) string {
	ms, ok := p.machine(m)
	if !ok {
		return "\n  This machine is not on the server any more.\n"
	}
	now := time.Now()
	w := m.screenWidth()
	st := machineState(m.st, ms, now)
	kind := "on demand · up to " + plural(ms.MaxRooms, "room")
	if ms.Kind == "static" {
		kind = "fixed machine"
		if ms.Connect {
			kind += " · connects to the server"
		}
	}
	var b strings.Builder
	b.WriteString("\n")
	for _, r := range machineRooms(m.st, p.machineKey) {
		var info strings.Builder
		if hw := r.Hardware; hw != nil {
			var parts []string
			add := func(ok bool, s string) {
				if ok {
					parts = append(parts, s)
				}
			}
			add(hw.CPU != "", clean(hw.CPU))
			add(hw.Cores > 0, plural(hw.Cores, "core"))
			add(hw.MemoryBytes > 0, gb(hw.MemoryBytes))
			add(hw.OSVersion != "", clean(hw.OSVersion))
			add(r.Arch != "", clean(r.Arch))
			info.WriteString(field("Hardware", strings.Join(parts, mutedStyle.Render(" · "))) + "\n")
		}
		runs := chips(machinePlatforms(ms), colBlue)
		if len(ms.Labels) > 0 {
			runs += mutedStyle.Render("  labels ") + chips(ms.Labels, colMuted)
		}
		info.WriteString(field("Runs", runs) + "\n")
		d, ok := p.details[r.Name]
		switch {
		case ok && d.Usage != nil:
			u := d.Usage
			info.WriteString(field("CPU", gauge(u.CPUPercent/100, 24, shareColor(u.CPUPercent/100))+fmt.Sprintf("  %.0f%%", u.CPUPercent)) + "\n")
			if u.MemoryTotalBytes > 0 {
				share := float64(u.MemoryUsedBytes) / float64(u.MemoryTotalBytes)
				info.WriteString(field("Memory", gauge(share, 24, shareColor(share))+"  "+gb(u.MemoryUsedBytes)+" of "+gb(u.MemoryTotalBytes)) + "\n")
			}
			if u.DiskFreeBytes > 0 {
				info.WriteString(field("Disk", gb(u.DiskFreeBytes)+" free") + "\n")
			}
		case p.details == nil && r.State == protocol.RoomRunning:
			info.WriteString(field("Now", mutedStyle.Render("reading …")) + "\n")
		}
		if r.Reason != "" {
			info.WriteString(field("Reason", fg(colRed).Render(clean(r.Reason))) + "\n")
		}
		title := boldStyle.Render(p.machineKey) + " " + pill(st.word, st.color) + " " + mutedStyle.Render(kind)
		if ms.Kind != "static" {
			title = boldStyle.Render(clean(r.Name)) + " " + mutedStyle.Render(clean(r.State+" · "+p.machineKey))
		}
		b.WriteString(card(title, info.String(), w, false) + "\n")
	}
	if len(machineRooms(m.st, p.machineKey)) == 0 {
		b.WriteString(card(boldStyle.Render(p.machineKey)+" "+pill(st.word, st.color)+" "+mutedStyle.Render(kind), field("Runs", chips(machinePlatforms(ms), colBlue)), w, false) + "\n")
	}
	for _, e := range p.errs {
		b.WriteString("  " + errStyle.Render(clean("Cannot read "+e)) + "\n")
	}

	leases, sel := p.leases(m)
	var g strings.Builder
	if len(leases) == 0 {
		g.WriteString(mutedStyle.Render("Nobody uses this machine now."))
	}
	for i, l := range leases {
		mark, name := "  ", clean(l.User)
		if i == sel {
			mark, name = accentStyle.Render("❯ "), boldStyle.Render(name)
		}
		when := "checked in " + ago(now.Sub(leaseSince(l))) + " ago"
		if !l.ExpiresAt.IsZero() {
			when += " · out by " + l.ExpiresAt.Local().Format("15:04")
		}
		if l.State != protocol.LeaseReady {
			when += " · " + plainLeaseState(l.State)
		}
		g.WriteString(mark + name + " · " + fmt.Sprintf("%q", clean(l.Task)) + "   " + mutedStyle.Render(when) + "\n")
		from := clean(l.Guest) + " · repository " + clean(l.Repo)
		if l.Branch != "" {
			from += " · " + accentStyle.Render("⎇ "+clean(l.Branch))
		}
		g.WriteString("    " + mutedStyle.Render(from) + "\n")
		for _, d := range p.details {
			for _, j := range d.Jobs[l.ID] {
				if j.State == "running" {
					g.WriteString("    " + fg(colGreen).Render("▶") + " " + clean(strings.Join(j.Command, " ")) + "  " + mutedStyle.Render("running "+ago(now.Sub(j.StartedAt))) + "\n")
				}
			}
			if d.JobsError != "" && d.Room.ID == l.RoomID {
				g.WriteString("    " + mutedStyle.Render(clean(d.JobsError)) + "\n")
			}
		}
	}
	b.WriteString(card(boldStyle.Render("Guest"), g.String(), w, true))
	return b.String()
}

func (p *machinePage) hint(m *model) string {
	leases, i := p.leases(m)
	if i < 0 {
		ms, _ := p.machine(m)
		s := machineState(m.st, ms, time.Now())
		return p.machineKey + ": " + strings.TrimPrefix(s.word+" · "+s.detail, " · ")
	}
	l := leases[i]
	h := clean(l.User) + "'s lease"
	if !l.ExpiresAt.IsZero() {
		h += ": ends at " + l.ExpiresAt.Local().Format("15:04") + " at the latest, or earlier after a time without activity"
	}
	return h + "."
}

// static reports whether the page's machine is a static machine (one room).
func (p *machinePage) static(m *model) bool {
	ms, ok := p.machine(m)
	return ok && ms.Kind == "static"
}

// repairable: repair is for a broken room of a static machine (a failed
// health check, a changed host key); the server destroys a broken on-demand
// room. An offline room comes back by itself; a running room needs no repair.
func repairable(ms protocol.MachineStatus, r protocol.RoomStatus) bool {
	return ms.Kind == "static" && r.State == protocol.RoomBroken && r.Reason != "unreachable"
}

func (p *machinePage) canEnd(m *model, l protocol.LeaseStatus) bool {
	return m.admin() || (m.st.User != "" && l.User == m.st.User)
}

// actionRoom is the room for drain and repair: the room of the selected lease,
// else the first room of the machine.
func (p *machinePage) actionRoom(m *model) (protocol.RoomStatus, bool) {
	rms := machineRooms(m.st, p.machineKey)
	if leases, i := p.leases(m); i >= 0 {
		if j := slices.IndexFunc(rms, func(r protocol.RoomStatus) bool { return r.ID == leases[i].RoomID }); j >= 0 {
			return rms[j], true
		}
	}
	if len(rms) == 0 {
		return protocol.RoomStatus{}, false
	}
	return rms[0], true
}

func (p *machinePage) keys(m *model) []binding {
	leases, sel := p.leases(m)
	var ks []binding
	if len(leases) > 1 {
		ks = append(ks, binding{"↑↓", "move"})
	}
	if sel >= 0 && p.canEnd(m, leases[sel]) {
		ks = append(ks, binding{"e", "end lease"})
	}
	// Drain and repair are for the room of a static machine: on-demand rooms
	// come and go (admins limit them with max_rooms).
	if r, ok := p.actionRoom(m); ok && m.admin() && p.static(m) {
		ks = append(ks, binding{"d", map[bool]string{false: "no new leases", true: "allow new leases"}[r.Drained]})
		if ms, _ := p.machine(m); repairable(ms, r) {
			ks = append(ks, binding{"r", "repair"})
		}
	}
	return ks
}

func (p *machinePage) key(m *model, k tea.KeyMsg) tea.Cmd {
	leases, i := p.leases(m)
	switch k.String() {
	case "up", "k":
		if i > 0 {
			p.sel = leases[i-1].ID
		}
	case "down", "j":
		if i >= 0 && i < len(leases)-1 {
			p.sel = leases[i+1].ID
		}
	case "e":
		if i >= 0 && p.canEnd(m, leases[i]) {
			l := leases[i]
			m.ask(fmt.Sprintf("End the lease of %s (%q)?", clean(l.User), clean(l.Task)), func() tea.Cmd {
				p.loaded = time.Time{}
				return m.do("POST", "/v1/leases/"+l.ID+"/release", nil, "The lease of "+clean(l.User)+" ended.")
			})
		}
	case "d", "r":
		r, ok := p.actionRoom(m)
		if !ok || !m.admin() || !p.static(m) {
			return nil
		}
		id := strconv.FormatInt(r.ID, 10)
		if k.String() == "r" {
			if ms, _ := p.machine(m); !repairable(ms, r) {
				return nil
			}
			repair := func(accept bool) func() tea.Cmd {
				return func() tea.Cmd {
					return m.do("POST", "/v1/rooms/"+id+"/repair", map[string]bool{"accept_host_key": accept}, "Repair of "+r.Name+" requested.")
				}
			}
			if r.Reason == "host key changed" {
				// The new key must be accepted on purpose: it can also be
				// another machine at the same address.
				m.choose("The host key of "+r.Name+" changed. a accept the new key and repair · another key cancels",
					map[string]func() tea.Cmd{"a": repair(true)})
				return nil
			}
			m.ask("Repair "+r.Name+"? The server checks the room again.", repair(false))
			return nil
		}
		verb, q, done := "drain", "Stop new leases on "+r.Name+"? The current leases continue.", "No new leases on "+r.Name+"."
		if r.Drained {
			verb, q, done = "undrain", "Allow new leases on "+r.Name+" again?", "New leases on "+r.Name+" are allowed again."
		}
		m.ask(q, func() tea.Cmd { return m.do("POST", "/v1/rooms/"+id+"/"+verb, nil, done) })
	}
	return nil
}
