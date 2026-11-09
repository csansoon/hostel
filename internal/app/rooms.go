package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/csansoon/hostel/internal/protocol"
)

// usageInterval is the time between two reads of GET /v1/usage while the
// Rooms screen is open.
const usageInterval = 10 * time.Second

// rooms is the Rooms screen: one list of providers, their machines, and the
// guests in the rooms of each machine. sel is the key of the selected item.
type rooms struct{ sel string }

type roomItemKind int

const (
	itemProvider roomItemKind = iota
	itemMachine
	itemLease
	itemRoom
	itemTemplate
)

type roomItem struct {
	kind     roomItemKind
	provider protocol.ProviderStatus
	tmpl     protocol.TemplateStatus
	machine  *protocol.MachineStatus // nil: a static machine that has not started
	lease    protocol.LeaseStatus
	room     protocol.RoomStatus // itemRoom: a room without a lease
}

func (it roomItem) key() string {
	switch it.kind {
	case itemProvider:
		return "p:" + it.provider.Name
	case itemLease:
		return "l:" + it.lease.ID
	case itemTemplate:
		return "t:" + it.tmpl.Key
	}
	if it.machine != nil {
		return "m:" + it.machine.Name
	}
	return "m:" + it.tmpl.Key
}

// selectable: a static provider is only a heading, and a room without a
// lease has no page.
func (it roomItem) selectable() bool {
	return it.kind != itemRoom && (it.kind != itemProvider || it.provider.Kind != "static")
}

func providerTitle(p protocol.ProviderStatus) string {
	if p.Label != "" {
		return p.Label
	}
	return p.Name
}

// roomItems is the list, in configuration order: lines must not move while
// people read them.
func roomItems(m *model) []roomItem {
	var out []roomItem
	for _, pv := range m.st.Providers {
		var tmpls []protocol.TemplateStatus
		for _, ms := range m.st.Templates {
			if name, _, _ := strings.Cut(ms.Key, "/"); name == pv.Name {
				tmpls = append(tmpls, ms)
			}
		}
		if len(tmpls) == 0 {
			continue
		}
		out = append(out, roomItem{kind: itemProvider, provider: pv})
		for _, ms := range tmpls {
			rms := templateMachines(m.st, ms.Key)
			if pv.Kind == "static" && len(rms) == 0 {
				out = append(out, roomItem{kind: itemMachine, provider: pv, tmpl: ms})
			}
			for i := range rms {
				out = append(out, roomItem{kind: itemMachine, provider: pv, tmpl: ms, machine: &rms[i]})
				out = append(out, machineRoomItems(m, pv, ms, &rms[i])...)
			}
		}
		if pv.Kind != "static" {
			for _, ms := range tmpls {
				out = append(out, roomItem{kind: itemTemplate, provider: pv, tmpl: ms})
			}
		}
	}
	return out
}

// machineRoomItems is one line for each room of a running machine, in the
// order of its rooms: the guest, or the room's state. A lease that has no room
// yet comes last.
func machineRoomItems(m *model, pv protocol.ProviderStatus, ms protocol.TemplateStatus, r *protocol.MachineStatus) []roomItem {
	var out []roomItem
	leases := machineLeases(m.st, r.ID)
	for _, s := range r.Rooms {
		i := slices.IndexFunc(leases, func(l protocol.LeaseStatus) bool { return l.Room == s.Name })
		switch {
		case i >= 0:
			out = append(out, roomItem{kind: itemLease, provider: pv, tmpl: ms, machine: r, lease: leases[i]})
			leases = slices.Delete(leases, i, i+1)
		case r.State == protocol.MachineRunning:
			out = append(out, roomItem{kind: itemRoom, provider: pv, tmpl: ms, machine: r, room: s})
		}
	}
	for _, l := range leases {
		out = append(out, roomItem{kind: itemLease, provider: pv, tmpl: ms, machine: r, lease: l})
	}
	return out
}

// selected returns the selectable items and the index of the selected one.
func (p *rooms) selected(m *model) ([]roomItem, int) {
	var items []roomItem
	for _, it := range roomItems(m) {
		if it.selectable() {
			items = append(items, it)
		}
	}
	i := slices.IndexFunc(items, func(it roomItem) bool { return it.key() == p.sel })
	if i < 0 && len(items) > 0 {
		i, p.sel = 0, items[0].key()
	}
	return items, i
}

func (p *rooms) title() string { return "Rooms" }

func (p *rooms) load(m *model) tea.Cmd { return m.readUsage() }

func (p *rooms) body(m *model) string {
	if len(m.st.Templates) == 0 {
		return "\n" + note("The server has no machines yet. An admin presses + to add one.")
	}
	p.selected(m)
	w := contentWidth(m)
	now := time.Now()
	var b strings.Builder
	b.WriteString("\n")
	items := roomItems(m)
	for i, it := range items {
		sel := it.selectable() && it.key() == p.sel
		if it.kind == itemProvider {
			if i > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(providerHeading(it.provider, w, sel) + "\n\n")
			continue
		}
		if it.kind != itemLease && it.kind != itemRoom && items[i-1].kind != itemProvider {
			b.WriteString("\n")
		}
		switch it.kind {
		case itemMachine:
			b.WriteString(m.machineItem(it, w, sel, now) + "\n")
		case itemLease:
			b.WriteString(row(w, sel, "  "+spread(leaseLeft(it.lease), leaseRight(it.lease), w-5)) + "\n")
		case itemRoom:
			b.WriteString(row(w, sel, "  "+spread(roomLeft(it.room), roomRight(it.room), w-5)) + "\n")
		case itemTemplate:
			b.WriteString(row(w, sel, "  "+spread(mutedStyle.Render(templateLeft(it.tmpl)), mutedStyle.Render(templateRight(m, it.tmpl)), w-5)) + "\n")
		}
	}
	return b.String()
}

// providerHeading says what a provider does only when it is special.
func providerHeading(p protocol.ProviderStatus, w int, sel bool) string {
	style := mutedStyle.Bold(true)
	lead := margin
	if sel {
		style, lead = accentStyle.Bold(true), accentStyle.Render(barMark+" ")
	}
	t := style.Render(strings.ToUpper(clean(providerTitle(p))))
	right := ""
	if p.Kind != "static" {
		right = " " + mutedStyle.Render("creates machines when needed")
	}
	return lead + t + " " + dimStyle.Render(strings.Repeat("─", max(w-lipgloss.Width(t)-lipgloss.Width(right)-1, 0))) + right
}

func machineName(m *model, it roomItem) string {
	if it.machine == nil || it.provider.Kind == "static" {
		_, name, _ := strings.Cut(it.tmpl.Key, "/")
		return clean(name)
	}
	return clean(strings.TrimPrefix(it.machine.Name, "hostel-"+m.st.ServerID+"-"))
}

func machineSize(it roomItem) string {
	if it.machine != nil && it.machine.Hardware != nil {
		return it.machine.Hardware.Size()
	}
	if it.tmpl.Hardware != nil {
		return it.tmpl.Hardware.Size()
	}
	return ""
}

// machineItem is the two lines of a machine: its name and rooms, then its
// size and use. A machine that does not run shows its state instead.
func (m *model) machineItem(it roomItem, w int, sel bool, now time.Time) string {
	right, sub := "", ""
	r := it.machine
	switch {
	case r != nil && r.State == protocol.MachineRunning:
		right = doors(*r)
		sub = m.useGauges(r.Name)
		if r.Drained {
			sub = mutedStyle.Render("◌ no new leases")
		}
	case r == nil || it.provider.Kind == "static":
		s := templateState(m.st, it.tmpl, now)
		detail, _, _ := strings.Cut(s.detail, " · ")
		right = s.dot() + " " + m.word("m:"+it.tmpl.Key, s)
		if s.word == "Offline" {
			right = fg(colRed).Render("✕ Offline")
		}
		sub = mutedStyle.Render(ansi.Truncate(clean(detail), w/2, "…"))
	case r.State == protocol.MachineBroken:
		right, sub = fg(colRed).Render("✕ Broken"), mutedStyle.Render(ansi.Truncate(clean(r.Reason), w/2, "…"))
	default:
		right = fg(colYellow).Render(spin("◐", "◓", "◑", "◒") + " " + strings.ToUpper(r.State[:1]) + r.State[1:])
	}
	size := clean(machineSize(it))
	if m.outOfDate(it.tmpl) {
		size = strings.TrimPrefix(size+" · ", " · ") + fg(colYellow).Render("out of date")
	}
	return item(w, sel, machineName(m, it), size, right, sub)
}

// door is the square of a room: blue occupied, green vacant, yellow in
// cleanup or setup, red broken.
func door(s protocol.RoomStatus) string {
	switch s.State {
	case protocol.RoomFree:
		return fg(colGreen).Render("□")
	case protocol.RoomLeased:
		return fg(colBlue).Render("■")
	case protocol.RoomBroken:
		return fg(colRed).Render("■")
	}
	return fg(colYellow).Render("□")
}

// doors is one square for each room.
func doors(r protocol.MachineStatus) string {
	var out []string
	for _, s := range r.Rooms {
		out = append(out, door(s))
	}
	return strings.Join(out, " ")
}

func roomLeft(s protocol.RoomStatus) string {
	return door(s) + " " + mutedStyle.Render(clean(s.Name))
}

func roomRight(s protocol.RoomStatus) string {
	switch s.State {
	case protocol.RoomFree:
		return fg(colGreen).Render("Vacant")
	case protocol.RoomBroken:
		return fg(colRed).Render("Broken")
	case protocol.RoomDirty:
		return fg(colYellow).Render("Needs cleanup")
	case protocol.RoomCleaning:
		return fg(colYellow).Render("Cleaning up")
	}
	return fg(colYellow).Render("Getting ready")
}

// useGauges is the live use of a machine; "" until the first sample.
func (m *model) useGauges(name string) string {
	u, ok := m.usage[name]
	if !ok {
		return ""
	}
	const width = 6
	cpu := u.CPUPercent / 100
	out := mutedStyle.Render("CPU ") + gauge(cpu, width, shareColor(cpu))
	if u.MemoryTotalBytes > 0 {
		mem := float64(u.MemoryUsedBytes) / float64(u.MemoryTotalBytes)
		out += mutedStyle.Render("  Memory ") + gauge(mem, width, shareColor(mem))
	}
	return out
}

func leaseLeft(l protocol.LeaseStatus) string {
	glyph := fg(colBlue).Render("◉")
	if l.State != protocol.LeaseReady {
		glyph = fg(colYellow).Render(spin("◐", "◓", "◑", "◒"))
	}
	return glyph + " " + clean(l.User) + " " + mutedStyle.Render(clean(l.Task))
}

func leaseRight(l protocol.LeaseStatus) string {
	switch {
	case l.State != protocol.LeaseReady:
		return fg(colYellow).Render(plainLeaseState(l.State))
	case !l.ExpiresAt.IsZero():
		return mutedStyle.Render("out by " + l.ExpiresAt.Local().Format("15:04"))
	}
	return ""
}

func templateLeft(t protocol.TemplateStatus) string {
	_, name, _ := strings.Cut(t.Key, "/")
	s := "◌ " + clean(name)
	if t.Hardware != nil && t.Hardware.Size() != "" {
		s += ", " + clean(t.Hardware.Size())
	}
	return s
}

func templateRight(m *model, t protocol.TemplateStatus) string {
	if n := t.MaxMachines - len(templateMachines(m.st, t.Key)); n > 0 {
		return fmt.Sprintf("%d more can start", n)
	}
	return "at its limit"
}

func (p *rooms) hint(m *model) string {
	items, i := p.selected(m)
	if i < 0 {
		return ""
	}
	it := items[i]
	switch it.kind {
	case itemProvider:
		return providerTitle(it.provider) + ": a provider that creates machines when a lease needs one."
	case itemLease:
		return clean(it.lease.User) + "'s lease in " + clean(it.lease.Room) + " of " + machineName(m, it) + "."
	case itemTemplate:
		return it.tmpl.Key + ": a template; the provider creates machines from it."
	}
	if it.machine != nil && it.machine.State == protocol.MachineRunning {
		used, free := 0, 0
		for _, s := range it.machine.Rooms {
			switch s.State {
			case protocol.RoomLeased:
				used++
			case protocol.RoomFree:
				free++
			}
		}
		return fmt.Sprintf("%s: %d occupied, %d vacant", machineName(m, it), used, free)
	}
	s := templateState(m.st, it.tmpl, time.Now())
	return machineName(m, it) + ": " + strings.TrimPrefix(s.word+" · "+s.detail, " · ")
}

func (p *rooms) keys(m *model) []binding {
	ks := []binding{{"↑↓", "move"}, {"enter", "open"}}
	if !m.admin() {
		return ks
	}
	if items, i := p.selected(m); i >= 0 && items[i].kind == itemMachine && len(machineActions(m, items[i].tmpl)) > 0 {
		ks = append(ks, binding{"a", "actions"})
	}
	return append(ks, binding{"+", "add a machine"})
}

func (p *rooms) key(m *model, k tea.KeyMsg) tea.Cmd {
	if k.String() == "+" && m.admin() {
		return m.open(newAddMachine())
	}
	items, i := p.selected(m)
	if i < 0 {
		return nil
	}
	it := items[i]
	switch k.String() {
	case "enter":
		switch it.kind {
		case itemProvider:
			return m.open(&providerPage{name: it.provider.Name})
		case itemLease:
			return m.open(&roomPage{leaseID: it.lease.ID})
		case itemTemplate:
			return m.open(&templatePage{templateKey: it.tmpl.Key})
		}
		page := &templatePage{templateKey: it.tmpl.Key}
		if it.machine != nil && it.provider.Kind != "static" {
			page.machine = it.machine.Name
		}
		return m.open(page)
	case "a":
		if opts := machineActions(m, it.tmpl); it.kind == itemMachine && m.admin() && len(opts) > 0 {
			m.menu(machineName(m, it), templateState(m.st, it.tmpl, time.Now()).word, opts)
		}
		return nil
	}
	p.sel = items[move(i, len(items), k.String())].key()
	return nil
}

// usageMsg is the answer of GET /v1/usage.
type usageMsg struct {
	gen  int
	list protocol.UsageList
	err  error
}

type usageTickMsg struct{ gen int }

// readUsage reads the live use, at most once in each usageInterval.
func (m *model) readUsage() tea.Cmd {
	if m.usageLoading {
		return nil
	}
	if time.Since(m.usageRead) < usageInterval {
		return m.usageTick()
	}
	m.usageLoading = true
	c, gen := m.c, m.gen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		var list protocol.UsageList
		err := c.Do(ctx, "GET", "/v1/usage", nil, &list)
		return usageMsg{gen: gen, list: list, err: err}
	}
}

func (m *model) usageTick() tea.Cmd {
	if m.usageTicking {
		return nil
	}
	m.usageTicking = true
	gen := m.gen
	return tea.Tick(usageInterval, func(time.Time) tea.Msg { return usageTickMsg{gen: gen} })
}

// providerPage is one provider that creates machines: its limit, its
// templates, and its machines. A provider has no hardware.
type providerPage struct{ name string }

func (p *providerPage) provider(m *model) (protocol.ProviderStatus, bool) {
	i := slices.IndexFunc(m.st.Providers, func(x protocol.ProviderStatus) bool { return x.Name == p.name })
	if i < 0 {
		return protocol.ProviderStatus{}, false
	}
	return m.st.Providers[i], true
}

func (p *providerPage) title() string { return p.name }

func (p *providerPage) body(m *model) string {
	pv, ok := p.provider(m)
	if !ok {
		return "\n" + note("This provider is not on the server any more.")
	}
	w := contentWidth(m)
	summary := "Creates machines when a lease needs one"
	if pv.MaxMachines > 0 {
		summary += ", up to " + plural(pv.MaxMachines, "machine")
	}
	var b strings.Builder
	b.WriteString(head(clean(providerTitle(pv)), clean(pv.Kind), summary))
	b.WriteString(section("Templates", w, false))
	for _, ms := range m.st.Templates {
		if name, _, _ := strings.Cut(ms.Key, "/"); name != pv.Name {
			continue
		}
		_, name, _ := strings.Cut(ms.Key, "/")
		size := ""
		if ms.Hardware != nil {
			size = ms.Hardware.Size()
		}
		b.WriteString(item(w, false, clean(name), clean(strings.Join(templateTools(ms), " · ")), clean(size),
			mutedStyle.Render(fmt.Sprintf("%d of %d running", len(templateMachines(m.st, ms.Key)), ms.MaxMachines))) + "\n\n")
	}
	return b.String()
}

func (p *providerPage) keys(m *model) []binding            { return nil }
func (p *providerPage) hint(m *model) string               { return "" }
func (p *providerPage) key(m *model, k tea.KeyMsg) tea.Cmd { return nil }

// templateTools are the OS and the tools of a template.
func templateTools(ms protocol.TemplateStatus) []string {
	out := []string{ms.OS}
	for _, t := range ms.Tools {
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

// roomPage is one occupied room: the guest, what it does, and its jobs.
type roomPage struct {
	leaseID string
	details *protocol.MachineDetails
	err     string
	loaded  time.Time
	loading bool
}

type roomDetailsMsg struct {
	gen     int
	lease   string
	details protocol.MachineDetails
	err     error
}

func (p *roomPage) lease(m *model) (protocol.LeaseStatus, bool) {
	i := slices.IndexFunc(m.st.Leases, func(l protocol.LeaseStatus) bool { return l.ID == p.leaseID })
	if i < 0 {
		return protocol.LeaseStatus{}, false
	}
	return m.st.Leases[i], true
}

func (p *roomPage) machine(m *model, l protocol.LeaseStatus) (protocol.MachineStatus, bool) {
	i := slices.IndexFunc(m.st.Machines, func(r protocol.MachineStatus) bool { return r.ID == l.MachineID })
	if i < 0 {
		return protocol.MachineStatus{}, false
	}
	return m.st.Machines[i], true
}

func (p *roomPage) title() string { return "Room" }

func (p *roomPage) load(m *model) tea.Cmd {
	l, ok := p.lease(m)
	if !ok || p.loading || time.Since(p.loaded) < detailsInterval {
		return nil
	}
	r, ok := p.machine(m, l)
	if !ok {
		return nil
	}
	p.loading = true
	c, gen, id := m.c, m.gen, p.leaseID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		var d protocol.MachineDetails
		err := c.Do(ctx, "GET", "/v1/machines/"+r.Name, nil, &d)
		return roomDetailsMsg{gen: gen, lease: id, details: d, err: err}
	}
}

func (p *roomPage) other(m *model, msg tea.Msg) tea.Cmd {
	if d, ok := msg.(roomDetailsMsg); ok && d.gen == m.gen && d.lease == p.leaseID {
		p.loading, p.loaded, p.err = false, time.Now(), ""
		if d.err != nil {
			p.err = d.err.Error()
			return nil
		}
		p.details = &d.details
	}
	return nil
}

func (p *roomPage) body(m *model) string {
	l, ok := p.lease(m)
	if !ok {
		return "\n" + note("This lease has ended.")
	}
	now := time.Now()
	w := contentWidth(m)
	state := mutedStyle.Render("checked in " + ago(now.Sub(leaseSince(l))) + " ago")
	if l.State != protocol.LeaseReady {
		state = fg(colYellow).Render(plainLeaseState(l.State))
	}
	var b strings.Builder
	b.WriteString(head(clean(l.User), clean(l.Task), state))
	b.WriteString(section("Room", w, false))
	b.WriteString(line(field("Room", clean(l.Room))))
	if r, ok := p.machine(m, l); ok {
		b.WriteString(line(field("Machine", clean(strings.TrimPrefix(r.Name, "hostel-"+m.st.ServerID+"-")))))
	}
	b.WriteString(line(field("Tools", fg(colBlue).Render(clean(strings.Join(l.Tools, "   "))))))
	repo := clean(l.Guest) + " · " + clean(l.Repo)
	if l.Branch != "" {
		repo += " · ⎇ " + clean(l.Branch)
	}
	b.WriteString(line(field("From", repo)))
	if !l.ExpiresAt.IsZero() {
		b.WriteString(line(field("Out by", l.ExpiresAt.Local().Format("15:04"))))
	}
	b.WriteString(section("Jobs", w, true))
	switch {
	case p.err != "":
		b.WriteString(line(errStyle.Render(clean("Cannot read the jobs: " + p.err))))
	case p.details == nil:
		b.WriteString(note("Reading …"))
	case p.details.JobsError != "":
		b.WriteString(note(clean(p.details.JobsError)))
	default:
		n := 0
		for _, j := range p.details.Jobs[l.ID] {
			if j.State == "running" {
				n++
				b.WriteString(line(fg(colGreen).Render("▶ ") + clean(strings.Join(j.Command, " ")) + mutedStyle.Render("   running "+ago(now.Sub(j.StartedAt)))))
			}
		}
		if n == 0 {
			b.WriteString(note("No job runs now."))
		}
	}
	return b.String()
}

func (p *roomPage) canEnd(m *model, l protocol.LeaseStatus) bool {
	return m.admin() || (m.st.User != "" && l.User == m.st.User)
}

func (p *roomPage) keys(m *model) []binding {
	if l, ok := p.lease(m); ok && p.canEnd(m, l) {
		return []binding{{"e", "end the lease"}}
	}
	return nil
}

func (p *roomPage) hint(m *model) string { return "" }

func (p *roomPage) key(m *model, k tea.KeyMsg) tea.Cmd {
	l, ok := p.lease(m)
	if k.String() == "e" && ok && p.canEnd(m, l) {
		m.warn("End the lease", fmt.Sprintf("End the lease of %s (%q)?", clean(l.User), clean(l.Task)), func() tea.Cmd {
			return m.do("POST", "/v1/leases/"+l.ID+"/release", nil, "The lease of "+clean(l.User)+" ended.")
		})
	}
	return nil
}
