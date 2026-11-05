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
	"github.com/charmbracelet/x/ansi"

	"github.com/csansoon/hostel/internal/protocol"
)

// detailsInterval is the shortest time between two reads of a machine's
// details (each read opens SSH to its rooms).
const detailsInterval = 10 * time.Second

// rooms lists the machines, grouped by provider; inside a provider in state
// order (online first). sel is the key of the selected machine, so that the
// selection follows it when the order changes.
type rooms struct{ sel string }

// providerGroups returns the providers in configuration order, each with its
// machines in state order.
func providerGroups(st protocol.Status, now time.Time) (names []string, groups map[string][]protocol.MachineStatus) {
	groups = map[string][]protocol.MachineStatus{}
	for _, ms := range st.Machines {
		pv, _, _ := strings.Cut(ms.Key, "/")
		if _, ok := groups[pv]; !ok {
			names = append(names, pv)
		}
		groups[pv] = append(groups[pv], ms)
	}
	for _, n := range names {
		groups[n] = byState(st, groups[n], now)
	}
	return names, groups
}

// selected returns the machines in screen order and the index of the
// selected one.
func (p *rooms) selected(m *model) ([]protocol.MachineStatus, int) {
	names, groups := providerGroups(m.st, time.Now())
	var ms []protocol.MachineStatus
	for _, n := range names {
		ms = append(ms, groups[n]...)
	}
	i := slices.IndexFunc(ms, func(x protocol.MachineStatus) bool { return x.Key == p.sel })
	if i < 0 && len(ms) > 0 {
		i, p.sel = 0, ms[0].Key
	}
	return ms, i
}

func (p *rooms) title() string { return "Rooms" }

func (p *rooms) body(m *model) string {
	if len(m.st.Machines) == 0 {
		return "\n" + note("The server has no machines yet. An admin adds one in Front desk › Rooms.")
	}
	p.selected(m)
	return "\n" + machineList(m, contentWidth(m), p.sel)
}

// machineList is the machines, grouped by provider, with sel selected.
func machineList(m *model, w int, sel string) string {
	now := time.Now()
	names, groups := providerGroups(m.st, now)
	var b strings.Builder
	for gi, n := range names {
		title := clean(n)
		if groups[n][0].Kind != "static" {
			title += " · on demand"
		}
		b.WriteString(section(title, w, gi > 0))
		for _, ms := range groups[n] {
			s := machineState(m.st, ms, now)
			_, name, _ := strings.Cut(ms.Key, "/")
			sub := clean(strings.Join(machinePlatforms(ms), " · "))
			if m.outOfDate(ms) {
				sub += " · " + fg(colYellow).Render("out of date")
			}
			right := s.dot() + " " + m.word("m:"+ms.Key, s)
			b.WriteString(item(w, ms.Key == sel, clean(name), sub, right, mutedStyle.Render(ansi.Truncate(clean(s.detail), w/2, "…"))) + "\n\n")
		}
	}
	return b.String()
}

// machinePlatforms are the OS and the platforms of a machine.
func machinePlatforms(ms protocol.MachineStatus) []string {
	out := []string{ms.OS}
	for _, p := range ms.Platforms {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

func (p *rooms) hint(m *model) string {
	machines, i := p.selected(m)
	if i < 0 {
		return ""
	}
	ms := machines[i]
	s := machineState(m.st, ms, time.Now())
	h := ms.Key + ": " + strings.TrimPrefix(s.word+" · "+s.detail, " · ")
	if m.outOfDate(ms) {
		h += " · its connect service runs " + clean(ms.ConnectVersion) + ", older than the server"
	}
	return h
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
		return "\n" + note("This machine is not on the server any more.")
	}
	now := time.Now()
	w := contentWidth(m)
	st := machineState(m.st, ms, now)
	kind := "On demand, up to " + plural(ms.MaxRooms, "room")
	if ms.Kind == "static" {
		kind = "A fixed machine"
		if ms.Connect {
			kind += " that connects to the server"
		}
	}
	summary := st.dot() + " " + m.word("m:"+ms.Key, st)
	if st.detail != "" {
		summary += "     " + mutedStyle.Render(clean(st.detail))
	}
	var b strings.Builder
	b.WriteString(head(clean(p.machineKey), kind, summary))

	// What the machine is: one section, or one for each on-demand room.
	var platforms []string
	for _, n := range machinePlatforms(ms) {
		platforms = append(platforms, fg(colBlue).Render(clean(n)))
	}
	about := line(field("Runs", strings.Join(platforms, "   ")))
	if len(ms.Labels) > 0 {
		about += line(field("Labels", clean(strings.Join(ms.Labels, ", "))))
	}
	if ms.ConnectVersion != "" {
		v := clean(ms.ConnectVersion)
		if m.outOfDate(ms) {
			v += "   " + fg(colYellow).Render("◆ out of date")
		}
		about += line(field("Connect", v))
		if m.outOfDate(ms) {
			about += line(field("", mutedStyle.Render("On the machine, as an administrator: ")+accentStyle.Render(machineUpdateCommand(ms.OS))))
		}
	}
	rms := machineRooms(m.st, p.machineKey)
	b.WriteString(section("Machine", w, false) + about)
	for _, r := range rms {
		title := "Now"
		if ms.Kind != "static" {
			title = clean("Room " + r.Name + " · " + r.State)
		}
		b.WriteString(section(title, w, true) + p.roomInfo(r))
	}
	for _, e := range p.errs {
		b.WriteString(line(errStyle.Render(clean("Cannot read " + e))))
	}

	// The guests: two lines each, and their running jobs.
	b.WriteString(section("Guests", w, true))
	leases, sel := p.leases(m)
	if len(leases) == 0 {
		b.WriteString(note("Nobody uses this machine now."))
	}
	for i, l := range leases {
		title := clean(l.User) + "  " + fmt.Sprintf("%q", clean(l.Task))
		right := mutedStyle.Render("checked in " + ago(now.Sub(leaseSince(l))) + " ago")
		if l.State != protocol.LeaseReady {
			right = fg(colYellow).Render(spin("◐", "◓", "◑", "◒") + " " + plainLeaseState(l.State))
		}
		from := clean(l.Guest) + " · " + clean(l.Repo)
		if l.Branch != "" {
			from += " · ⎇ " + clean(l.Branch)
		}
		out := ""
		if !l.ExpiresAt.IsZero() {
			out = mutedStyle.Render("out by " + l.ExpiresAt.Local().Format("15:04"))
		}
		b.WriteString(item(w, i == sel, title, from, right, out) + "\n")
		for _, d := range p.details {
			for _, j := range d.Jobs[l.ID] {
				if j.State == "running" {
					b.WriteString(line("   " + fg(colGreen).Render("▶ ") + clean(strings.Join(j.Command, " ")) + mutedStyle.Render("   running "+ago(now.Sub(j.StartedAt)))))
				}
			}
			if d.JobsError != "" && d.Room.ID == l.RoomID {
				b.WriteString(note("   " + clean(d.JobsError)))
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// roomInfo is the hardware and the live use of one room.
func (p *machinePage) roomInfo(r protocol.RoomStatus) string {
	var b strings.Builder
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
		b.WriteString(line(field("Hardware", strings.Join(parts, mutedStyle.Render(" · ")))))
	}
	d, ok := p.details[r.Name]
	switch {
	case ok && d.Usage != nil:
		u := d.Usage
		b.WriteString(line(field("CPU", gauge(u.CPUPercent/100, 24, shareColor(u.CPUPercent/100))+fmt.Sprintf("  %.0f%%", u.CPUPercent))))
		if u.MemoryTotalBytes > 0 {
			share := float64(u.MemoryUsedBytes) / float64(u.MemoryTotalBytes)
			b.WriteString(line(field("Memory", gauge(share, 24, shareColor(share))+"  "+gb(u.MemoryUsedBytes)+" of "+gb(u.MemoryTotalBytes))))
		}
		if u.DiskFreeBytes > 0 {
			b.WriteString(line(field("Disk", gb(u.DiskFreeBytes)+" free")))
		}
	case p.details == nil && r.State == protocol.RoomRunning:
		b.WriteString(line(field("Use", mutedStyle.Render("reading …"))))
	}
	if r.Reason != "" {
		b.WriteString(line(field("Reason", fg(colRed).Render(clean(r.Reason)))))
	}
	if r.Drained {
		b.WriteString(line(field("Leases", mutedStyle.Render("◌ no new leases"))))
	}
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

// repairable: repair is for a broken room of a static machine (a failed
// health check, a changed host key); the server destroys a broken on-demand
// room. An offline room comes back by itself; a running room needs no repair.
func repairable(ms protocol.MachineStatus, r protocol.RoomStatus) bool {
	return ms.Kind == "static" && r.State == protocol.RoomBroken && r.Reason != "unreachable"
}

func (p *machinePage) canEnd(m *model, l protocol.LeaseStatus) bool {
	return m.admin() || (m.st.User != "" && l.User == m.st.User)
}

func (p *machinePage) keys(m *model) []binding {
	leases, sel := p.leases(m)
	var ks []binding
	if len(leases) > 1 {
		ks = append(ks, binding{"↑↓", "move"})
	}
	if sel >= 0 && p.canEnd(m, leases[sel]) {
		ks = append(ks, binding{"enter", "actions"})
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
	case "enter":
		if i >= 0 && p.canEnd(m, leases[i]) {
			// The option keeps the lease: the list can change meanwhile.
			l := leases[i]
			m.menu(clean(l.User), fmt.Sprintf("%q", clean(l.Task)), []option{
				{label: "End the lease", key: "e", run: func() tea.Cmd { p.endLease(m, l); return nil }},
			})
		}
	case "e":
		if i >= 0 && p.canEnd(m, leases[i]) {
			p.endLease(m, leases[i])
		}
	}
	return nil
}

func (p *machinePage) endLease(m *model, l protocol.LeaseStatus) {
	m.warn("End the lease", fmt.Sprintf("End the lease of %s (%q)?", clean(l.User), clean(l.Task)), func() tea.Cmd {
		p.loaded = time.Time{}
		return m.do("POST", "/v1/leases/"+l.ID+"/release", nil, "The lease of "+clean(l.User)+" ended.")
	})
}

// roomAdmin is Front desk › Rooms (admins): Add a room first, then the
// machines; Enter on a machine shows what an admin can do with it.
type roomAdmin struct {
	// sel is "" for Add a room, else the key of the selected machine.
	sel string
}

func (p *roomAdmin) title() string { return "Rooms" }

// order returns the items: "" (Add a room), then the machine keys in the
// order of the screen, and the index of the selected one.
func (p *roomAdmin) order(m *model) ([]string, int) {
	keys := []string{""}
	names, groups := providerGroups(m.st, time.Now())
	for _, n := range names {
		for _, ms := range groups[n] {
			keys = append(keys, ms.Key)
		}
	}
	i := slices.Index(keys, p.sel)
	if i < 0 {
		i, p.sel = 0, ""
	}
	return keys, i
}

func (p *roomAdmin) body(m *model) string {
	w := contentWidth(m)
	p.order(m)
	var b strings.Builder
	b.WriteString("\n" + iconItem(w, p.sel == "", "+", "Add a room", "Connect a new machine to this server", "", "") + "\n\n")
	b.WriteString(machineList(m, w, p.sel))
	return b.String()
}

func (p *roomAdmin) hint(m *model) string {
	if p.sel == "" {
		return "Add a room: connect a new machine to this server; it shows in Rooms."
	}
	i := slices.IndexFunc(m.st.Machines, func(ms protocol.MachineStatus) bool { return ms.Key == p.sel })
	if i < 0 {
		return ""
	}
	s := machineState(m.st, m.st.Machines[i], time.Now())
	return p.sel + ": " + strings.TrimPrefix(s.word+" · "+s.detail, " · ")
}

func (p *roomAdmin) keys(m *model) []binding {
	return []binding{{"↑↓", "move"}, {"enter", map[bool]string{true: "add", false: "actions"}[p.sel == ""]}}
}

func (p *roomAdmin) key(m *model, k tea.KeyMsg) tea.Cmd {
	keys, i := p.order(m)
	if k.String() != "enter" {
		p.sel = keys[move(i, len(keys), k.String())]
		return nil
	}
	if p.sel == "" {
		return m.open(newAddMachine())
	}
	j := slices.IndexFunc(m.st.Machines, func(ms protocol.MachineStatus) bool { return ms.Key == p.sel })
	if j < 0 {
		return nil
	}
	ms := m.st.Machines[j]
	opts := []option{{label: "Show the room", sub: "Hardware, use, and guests", key: "o", run: func() tea.Cmd { return m.open(&machinePage{machineKey: ms.Key}) }}}
	opts = append(opts, roomActions(m, ms)...)
	m.menu(clean(ms.Key), machineState(m.st, ms, time.Now()).word, opts)
	return nil
}

// roomActions are the admin actions on the room of a static machine: stop or
// allow new leases, and repair. On-demand rooms come and go (admins limit
// them with max_rooms).
func roomActions(m *model, ms protocol.MachineStatus) []option {
	rms := machineRooms(m.st, ms.Key)
	if ms.Kind != "static" || len(rms) == 0 {
		return nil
	}
	r := rms[0]
	id := strconv.FormatInt(r.ID, 10)
	var out []option
	if r.Drained {
		out = append(out, option{label: "Allow new leases", sub: "The machine takes new leases again", key: "d", run: func() tea.Cmd {
			m.ask("Allow new leases", "Allow new leases on "+clean(r.Name)+" again?", func() tea.Cmd {
				return m.do("POST", "/v1/rooms/"+id+"/undrain", nil, "New leases on "+clean(r.Name)+" are allowed again.")
			})
			return nil
		}})
	} else {
		out = append(out, option{label: "Stop new leases", sub: "The current leases continue", key: "d", run: func() tea.Cmd {
			m.ask("Stop new leases", "Stop new leases on "+clean(r.Name)+"? The current leases continue.", func() tea.Cmd {
				return m.do("POST", "/v1/rooms/"+id+"/drain", nil, "No new leases on "+clean(r.Name)+".")
			})
			return nil
		}})
	}
	if repairable(ms, r) {
		repair := func(accept bool) func() tea.Cmd {
			return func() tea.Cmd {
				return m.do("POST", "/v1/rooms/"+id+"/repair", map[string]bool{"accept_host_key": accept}, "Repair of "+clean(r.Name)+" requested.")
			}
		}
		out = append(out, option{label: "Repair", sub: "The server checks the room again", key: "r", run: func() tea.Cmd {
			if r.Reason == "host key changed" {
				// The new key must be accepted on purpose: it can also be
				// another machine at the same address.
				m.warn("Accept the new key", "The host key of "+clean(r.Name)+" changed. It can also be another machine at the same address. Accept the new key and repair?", repair(true))
				return nil
			}
			m.ask("Repair", "Repair "+clean(r.Name)+"? The server checks the room again.", repair(false))
			return nil
		}})
	}
	return out
}
