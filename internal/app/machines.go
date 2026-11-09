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
	"github.com/charmbracelet/lipgloss"

	"github.com/csansoon/hostel/internal/protocol"
)

// detailsInterval is the shortest time between two reads of a machine's
// details (each read opens SSH to its machines).
const detailsInterval = 10 * time.Second

// templatePage is one machine: its hardware and live use, and each lease with
// its jobs; the admin actions.
type templatePage struct {
	templateKey string
	// machine limits the page to one machine of an on-demand template.
	machine string
	// details of each machine of the machine, by machine name.
	details map[string]protocol.MachineDetails
	errs    []string
	loaded  time.Time
	loading bool
	// sel is the selected lease (its ID).
	sel string
}

type detailsMsg struct {
	gen     int
	key     string
	details map[string]protocol.MachineDetails
	errs    []string
}

func (p *templatePage) title() string {
	if p.machine != "" {
		return p.machine
	}
	return p.templateKey
}

func (p *templatePage) machines(m *model) []protocol.MachineStatus {
	out := templateMachines(m.st, p.templateKey)
	if p.machine != "" {
		out = slices.DeleteFunc(out, func(r protocol.MachineStatus) bool { return r.Name != p.machine })
	}
	return out
}

func (p *templatePage) template(m *model) (protocol.TemplateStatus, bool) {
	i := slices.IndexFunc(m.st.Templates, func(ms protocol.TemplateStatus) bool { return ms.Key == p.templateKey })
	if i < 0 {
		return protocol.TemplateStatus{}, false
	}
	return m.st.Templates[i], true
}

func (p *templatePage) load(m *model) tea.Cmd {
	if p.loading || time.Since(p.loaded) < detailsInterval && p.details != nil {
		return nil
	}
	p.loading = true
	c, gen, key := m.c, m.gen, p.templateKey
	var names []string
	for _, r := range p.machines(m) {
		names = append(names, r.Name)
	}
	return func() tea.Msg {
		out := detailsMsg{gen: gen, key: key, details: map[string]protocol.MachineDetails{}}
		var mu sync.Mutex
		var wg sync.WaitGroup
		// The machines in parallel, each with its own deadline: each read can
		// take seconds (jobs and live use over SSH).
		for _, name := range names {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
				defer cancel()
				var d protocol.MachineDetails
				err := c.Do(ctx, "GET", "/v1/machines/"+name, nil, &d)
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

func (p *templatePage) other(m *model, msg tea.Msg) tea.Cmd {
	if d, ok := msg.(detailsMsg); ok && d.gen == m.gen && d.key == p.templateKey {
		p.details, p.errs, p.loaded, p.loading = d.details, d.errs, time.Now(), false
	}
	return nil
}

// leases returns the leases on the machine's machines (from the status, so that
// they follow each change) and the index of the selected one.
func (p *templatePage) leases(m *model) ([]protocol.LeaseStatus, int) {
	var out []protocol.LeaseStatus
	for _, r := range p.machines(m) {
		out = append(out, machineLeases(m.st, r.ID)...)
	}
	i := slices.IndexFunc(out, func(l protocol.LeaseStatus) bool { return l.ID == p.sel })
	if i < 0 && len(out) > 0 {
		i, p.sel = 0, out[0].ID
	}
	return out, i
}

func gb(n int64) string { return strconv.FormatFloat(float64(n)/(1<<30), 'f', 1, 64) + " GB" }

func (p *templatePage) body(m *model) string {
	ms, ok := p.template(m)
	if !ok {
		return "\n" + note("This machine is not on the server any more.")
	}
	now := time.Now()
	w := contentWidth(m)
	st := templateState(m.st, ms, now)
	kind := "On demand, up to " + plural(ms.MaxMachines, "machine")
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
	b.WriteString(head(clean(p.templateKey), kind, summary))

	// What the machine is: one section, or one for each on-demand machine.
	about := line(field("OS", clean(ms.OS)))
	if rms := p.machines(m); len(rms) == 0 || ms.Kind != "static" {
		// The configured tools; the inventory is in each machine's section.
		var ts []string
		for _, t := range ms.Tools {
			ts = append(ts, fg(colBlue).Render(clean(t)))
		}
		about += line(field("Tools", strings.Join(ts, "   ")))
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
	rms := p.machines(m)
	b.WriteString(section("Machine", w, false) + about)
	for _, r := range rms {
		title := "Now"
		if ms.Kind != "static" {
			title = clean("Machine " + r.Name + " · " + r.State)
		}
		b.WriteString(section(title, w, true) + p.machineInfo(r))
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
			if d.JobsError != "" && d.Machine.ID == l.MachineID {
				b.WriteString(note("   " + clean(d.JobsError)))
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// inventoryLines are the tools of a machine with their options and check state, one
// line each.
func inventoryLines(r protocol.MachineStatus) string {
	if len(r.Tools) == 0 {
		return line(field("Tools", mutedStyle.Render("not installed yet")))
	}
	nameW, optW := 0, 0
	for _, t := range r.Tools {
		nameW = max(nameW, lipgloss.Width(clean(t.Name)))
		optW = max(optW, lipgloss.Width(clean(strings.Join(t.Options, ", "))))
	}
	var b strings.Builder
	for i, t := range r.Tools {
		label := ""
		if i == 0 {
			label = "Tools"
		}
		s := state{"ready", colGreen, ""}
		glyph := "●"
		switch t.State {
		case "installing":
			s, glyph = state{"installing", colYellow, ""}, "◐"
		case "failed":
			s, glyph = state{"failed", colRed, ""}, "✕"
		}
		kind := ""
		if t.Screen {
			kind = "   " + mutedStyle.Render("screen")
		}
		b.WriteString(line(field(label, cell(fg(colBlue).Render(clean(t.Name)), nameW)+"   "+cell(clean(strings.Join(t.Options, ", ")), optW)+
			"   "+fg(s.color).Render(glyph+" "+s.word)+kind)))
	}
	return b.String()
}

// machineInfo is the hardware, the inventory, and the live use of one machine.
func (p *templatePage) machineInfo(r protocol.MachineStatus) string {
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
	b.WriteString(inventoryLines(r))
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
	case p.details == nil && r.State == protocol.MachineRunning:
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

func (p *templatePage) hint(m *model) string {
	leases, i := p.leases(m)
	if i < 0 {
		ms, _ := p.template(m)
		s := templateState(m.st, ms, time.Now())
		return p.templateKey + ": " + strings.TrimPrefix(s.word+" · "+s.detail, " · ")
	}
	l := leases[i]
	h := clean(l.User) + "'s lease"
	if !l.ExpiresAt.IsZero() {
		h += ": ends at " + l.ExpiresAt.Local().Format("15:04") + " at the latest, or earlier after a time without activity"
	}
	return h + "."
}

// repairable: repair is for a broken machine of a static machine (a failed
// health check, a changed host key); the server destroys a broken on-demand
// machine. An offline machine comes back by itself; a running machine needs no repair.
func repairable(ms protocol.TemplateStatus, r protocol.MachineStatus) bool {
	return ms.Kind == "static" && r.State == protocol.MachineBroken && r.Reason != "unreachable"
}

func (p *templatePage) canEnd(m *model, l protocol.LeaseStatus) bool {
	return m.admin() || (m.st.User != "" && l.User == m.st.User)
}

func (p *templatePage) keys(m *model) []binding {
	leases, sel := p.leases(m)
	var ks []binding
	if len(leases) > 1 {
		ks = append(ks, binding{"↑↓", "move"})
	}
	if sel >= 0 && p.canEnd(m, leases[sel]) {
		ks = append(ks, binding{"enter", "actions"})
	}
	if ms, ok := p.template(m); ok && m.admin() && len(machineActions(m, ms)) > 0 {
		ks = append(ks, binding{"a", "machine actions"})
	}
	return ks
}

func (p *templatePage) key(m *model, k tea.KeyMsg) tea.Cmd {
	leases, i := p.leases(m)
	switch k.String() {
	case "a":
		if ms, ok := p.template(m); ok && m.admin() {
			if opts := machineActions(m, ms); len(opts) > 0 {
				m.menu(clean(ms.Key), templateState(m.st, ms, time.Now()).word, opts)
			}
		}
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

func (p *templatePage) endLease(m *model, l protocol.LeaseStatus) {
	m.warn("End the lease", fmt.Sprintf("End the lease of %s (%q)?", clean(l.User), clean(l.Task)), func() tea.Cmd {
		p.loaded = time.Time{}
		return m.do("POST", "/v1/leases/"+l.ID+"/release", nil, "The lease of "+clean(l.User)+" ended.")
	})
}

// machineActions are the admin actions on the machine of a static machine: stop or
// allow new leases, and repair. On-demand machines come and go (admins limit
// them with max_machines).
func machineActions(m *model, ms protocol.TemplateStatus) []option {
	rms := templateMachines(m.st, ms.Key)
	if ms.Kind != "static" || len(rms) == 0 {
		return nil
	}
	r := rms[0]
	id := strconv.FormatInt(r.ID, 10)
	var out []option
	if r.Drained {
		out = append(out, option{label: "Allow new leases", sub: "The machine takes new leases again", key: "d", run: func() tea.Cmd {
			m.ask("Allow new leases", "Allow new leases on "+clean(r.Name)+" again?", func() tea.Cmd {
				return m.do("POST", "/v1/machines/"+id+"/undrain", nil, "New leases on "+clean(r.Name)+" are allowed again.")
			})
			return nil
		}})
	} else {
		out = append(out, option{label: "Stop new leases", sub: "The current leases continue", key: "d", run: func() tea.Cmd {
			m.ask("Stop new leases", "Stop new leases on "+clean(r.Name)+"? The current leases continue.", func() tea.Cmd {
				return m.do("POST", "/v1/machines/"+id+"/drain", nil, "No new leases on "+clean(r.Name)+".")
			})
			return nil
		}})
	}
	if repairable(ms, r) {
		repair := func(accept bool) func() tea.Cmd {
			return func() tea.Cmd {
				return m.do("POST", "/v1/machines/"+id+"/repair", map[string]bool{"accept_host_key": accept}, "Repair of "+clean(r.Name)+" requested.")
			}
		}
		out = append(out, option{label: "Repair", sub: "The server checks the machine again", key: "r", run: func() tea.Cmd {
			if r.Reason == "host key changed" {
				// The new key must be accepted on purpose: it can also be
				// another machine at the same address.
				m.warn("Accept the new key", "The host key of "+clean(r.Name)+" changed. It can also be another machine at the same address. Accept the new key and repair?", repair(true))
				return nil
			}
			m.ask("Repair", "Repair "+clean(r.Name)+"? The server checks the machine again.", repair(false))
			return nil
		}})
	}
	return out
}
