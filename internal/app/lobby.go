package app

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/csansoon/hostel/internal/protocol"
)

// lobby lists the tools (screen tools first), each with its free machines and
// its waiting leases.
type lobby struct{ sel int }

func (p *lobby) title() string { return "Lobby" }

// queued returns the queued leases that asked for a tool, in the order of the
// waiting line (a lease that the scheduler has not placed yet comes last).
func queued(st protocol.Status, tool string) []protocol.LeaseStatus {
	var out []protocol.LeaseStatus
	for _, l := range st.Leases {
		if l.State == protocol.LeaseQueued && asks(l, tool) {
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

// asks reports whether a lease asked for a tool.
func asks(l protocol.LeaseStatus, tool string) bool {
	return slices.ContainsFunc(l.Tools, func(t string) bool { name, _, _ := strings.Cut(t, ":"); return name == tool })
}

// onlineMachines are the machines of a tool that are not offline: the
// Lobby leaves out the offline ones (owner decision, 2026-11-03).
func onlineMachines(m *model, p protocol.Tool) []protocol.MachineStatus {
	var out []protocol.MachineStatus
	for _, tm := range p.Machines {
		key := tm.Key
		if i := slices.IndexFunc(m.st.Machines, func(ms protocol.MachineStatus) bool { return ms.Key == key }); i >= 0 && online(m.st, m.st.Machines[i]) {
			out = append(out, m.st.Machines[i])
		}
	}
	return out
}

// freeMachines counts the machines of a tool that can take a lease now.
func freeMachines(m *model, p protocol.Tool) int {
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

// toolText is what a tool is, in a few words.
func toolText(t protocol.Tool) string {
	if t.Mise || t.Description == "" {
		return "Installed with mise"
	}
	return t.Description
}

func (p *lobby) body(m *model) string {
	switch {
	case m.toolsErr != "" && !m.toolsRead:
		return "\n  " + errStyle.Render(clean("Cannot read the tools: "+m.toolsErr)) + "\n"
	case !m.toolsRead:
		return "\n  Reading the tools …\n"
	case len(m.tools) == 0:
		return "\n  No machine lists a tool yet. An admin adds a machine in Front desk › Rooms.\n"
	}
	stale := ""
	if m.toolsErr != "" {
		stale = "  " + errStyle.Render(clean("! Cannot read the tools: "+m.toolsErr+" (older data below; trying again)")) + "\n"
	}
	w := contentWidth(m)
	var b strings.Builder
	b.WriteString(stale)
	// The server sends the screen tools first.
	for i, t := range m.tools {
		if i == 0 || t.Screen != m.tools[i-1].Screen {
			title := "Software"
			if t.Screen {
				title = "Screens"
			}
			b.WriteString(section(title, w, i > 0))
		}
		total := len(onlineMachines(m, t))
		// The description gets the space that the options and the machines
		// leave.
		rest := " · " + plural(len(t.Machines), "machine")
		if len(t.Options) > 0 {
			rest = " · " + clean(strings.Join(t.Options, ", ")) + rest
		}
		sub := ansi.Truncate(clean(toolText(t)), max(w*2/3-lipgloss.Width(rest), 12), "…") + rest
		b.WriteString(item(w, i == p.sel, clean(t.Name), sub, m.vacancy(t), waitLine(t, total)) + "\n\n")
	}
	return b.String()
}

// vacancy is a tool's free machines: ● 2 vacant, ◉ all occupied, or no
// machine online; it glows when it changes.
func (m *model) vacancy(t protocol.Tool) string {
	free, total := freeMachines(m, t), len(onlineMachines(m, t))
	s := state{fmt.Sprintf("%d vacant", free), colGreen, ""}
	glyph := "●"
	switch {
	case total == 0:
		return mutedStyle.Render("○ no machine online")
	case free == 0:
		s, glyph = state{"all occupied", colBlue, ""}, "◉"
	}
	if m.glowing("t:" + t.Name) {
		return fg(s.color).Render(glyph) + " " + lipgloss.NewStyle().Background(s.color).Foreground(colInk).Render(s.word)
	}
	return fg(s.color).Render(glyph + " " + s.word)
}

// waitLine is a tool's waiting line in a few words.
func waitLine(t protocol.Tool, online int) string {
	if t.Waiting == 0 {
		return mutedStyle.Render("nobody waiting")
	}
	next := "next room " + waitText(t.EstimatedWaitSeconds)
	if online == 0 {
		next = "no machine online"
	}
	return fg(colYellow).Render(spin("◴", "◷", "◶", "◵")+fmt.Sprintf(" %d waiting", t.Waiting)) + mutedStyle.Render(" · "+next)
}

func (p *lobby) hint(m *model) string {
	if p.sel >= len(m.tools) {
		return ""
	}
	t := m.tools[p.sel]
	h := t.Name + ": " + toolText(t)
	if t.Screen {
		h += " · a screen: it starts for each lease that asks for it"
	}
	return h
}

func (p *lobby) keys(m *model) []binding { return []binding{{"↑↓", "move"}, {"enter", "open"}} }

func (p *lobby) key(m *model, k tea.KeyMsg) tea.Cmd {
	if k.String() == "enter" && p.sel < len(m.tools) {
		return m.open(&toolPage{name: m.tools[p.sel].Name, onMachines: true})
	}
	p.sel = move(p.sel, len(m.tools), k.String())
	return nil
}

// toolPage is one tool: its options, its machines, a summary of its guide,
// and the waiting leases that asked for it, with the actions on them.
type toolPage struct {
	name string
	// sel is the selected lease (its ID: the queue order changes).
	sel string
	// onMachines: the Machines card has the keyboard (tab), not the Waiting
	// line; machineSel is the key of its selected machine (the order
	// follows the states).
	onMachines bool
	machineSel string
}

func (p *toolPage) title() string { return p.name }

func (p *toolPage) tool(m *model) (protocol.Tool, bool) {
	i := slices.IndexFunc(m.tools, func(t protocol.Tool) bool { return t.Name == p.name })
	if i < 0 {
		return protocol.Tool{}, false
	}
	return m.tools[i], true
}

// selected returns the queue and the index of the selected lease (the first
// one when the selected lease left the queue).
func (p *toolPage) selected(m *model) ([]protocol.LeaseStatus, int) {
	q := queued(m.st, p.name)
	i := slices.IndexFunc(q, func(l protocol.LeaseStatus) bool { return l.ID == p.sel })
	if i < 0 && len(q) > 0 {
		i = 0
		p.sel = q[0].ID
	}
	return q, i
}

func (p *toolPage) body(m *model) string {
	t, ok := p.tool(m)
	if !ok {
		return "\n  No machine of the server has this tool any more.\n"
	}
	now := time.Now()
	w := contentWidth(m)
	machines := p.machines(m, t)
	q, qsel := p.selected(m)
	if len(q) == 0 {
		p.onMachines = true
	}
	var b strings.Builder
	if m.toolsErr != "" {
		b.WriteString(margin + errStyle.Render(clean("! Cannot read the tools: "+m.toolsErr+" (older data below; trying again)")) + "\n")
	}

	typical := "typical stay not known yet"
	if t.TypicalLeaseSeconds > 0 {
		typical = "typical stay " + ago(time.Duration(t.TypicalLeaseSeconds)*time.Second)
	}
	b.WriteString(head(clean(t.Name), clean(toolText(t)), m.vacancy(t)+"     "+waitLine(t, len(machines))+"     "+mutedStyle.Render(typical)))

	// The options: the phones and tablets of a screen tool, or the versions
	// of a software tool.
	b.WriteString(heading("Options", w) + "\n\n")
	var opts []string
	for _, o := range t.Options {
		opts = append(opts, fg(colBlue).Render(clean(o)))
	}
	var about []string
	switch {
	case len(t.Options) == 0:
		about = append(about, "no options: ask for "+clean(t.Name))
	case t.Screen:
		about = append(about, "the first is the default")
		if t.MaxOptions > 1 {
			about = append(about, fmt.Sprintf("up to %d in one lease", t.MaxOptions))
		}
	default:
		about = append(about, "the versions on the machines; "+clean(t.Name)+":<version> asks for one")
	}
	if t.WholeMachine {
		about = append(about, "a lease takes the whole machine")
	}
	b.WriteString(line(strings.Join(append(opts, mutedStyle.Render(strings.Join(about, " · "))), "    ")))

	// The machines: one line each, with the options of the tool on it.
	b.WriteString("\n\n" + heading("Machines", w) + "\n\n")
	if len(machines) == 0 {
		b.WriteString(margin + "   " + mutedStyle.Render("No machine with "+clean(t.Name)+" is online.") + "\n")
	}
	_, msel := p.selectedMachine(m, t)
	nameW, optW := 0, 0
	for _, ms := range machines {
		nameW = max(nameW, lipgloss.Width(clean(ms.Key)))
		optW = max(optW, lipgloss.Width(clean(machineOptions(t, ms.Key))))
	}
	for i, ms := range machines {
		s := machineState(m.st, ms, now)
		word := m.word("m:"+ms.Key, s)
		line := s.dot() + " " + cell(boldStyle.Render(clean(ms.Key)), nameW) + "   " + cell(word, len("No new leases")) + "   " +
			cell(fg(colBlue).Render(clean(machineOptions(t, ms.Key))), optW) + "   " + mutedStyle.Render(s.detail)
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
		sub := "   " + prio + mutedStyle.Render(" · waiting "+ago(now.Sub(l.CreatedAt))+" · asks for "+clean(strings.Join(l.Tools, " ")))
		right := fg(colYellow).Render(spin("◴", "◷", "◶", "◵") + " room in " + waitText(l.EstimatedWaitSeconds))
		b.WriteString(item(w, !p.onMachines && i == qsel, title, sub, right, "") + "\n\n")
	}

	// The guide: its first paragraph.
	if g := guideSummary(t.Guide); g != "" {
		b.WriteString("\n\n" + heading("Guide", w) + "\n\n")
		for _, l := range strings.Split(lipgloss.NewStyle().Width(w-3).Render(clean(g)), "\n") {
			b.WriteString(line(strings.TrimRight(l, " ")))
		}
		b.WriteString(note("hostel tools " + clean(t.Name) + " shows the whole guide."))
	}
	return b.String()
}

// machineOptions are the options of a tool on one machine.
func machineOptions(t protocol.Tool, key string) string {
	i := slices.IndexFunc(t.Machines, func(tm protocol.ToolMachine) bool { return tm.Key == key })
	if i < 0 {
		return ""
	}
	return strings.Join(t.Machines[i].Options, ", ")
}

// guideSummary is the first paragraph of a tool's guide, in one line.
func guideSummary(guide string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(guide), "\n\n")
	return strings.Join(strings.Fields(first), " ")
}

// selectedMachine returns the machines of the card and the index of the
// selected one (the first when it is gone).
func (p *toolPage) selectedMachine(m *model, t protocol.Tool) ([]protocol.MachineStatus, int) {
	ms := p.machines(m, t)
	i := slices.IndexFunc(ms, func(x protocol.MachineStatus) bool { return x.Key == p.machineSel })
	if i < 0 && len(ms) > 0 {
		i, p.machineSel = 0, ms[0].Key
	}
	return ms, i
}

// machines are the online machines that have the tool, in state order.
func (p *toolPage) machines(m *model, t protocol.Tool) []protocol.MachineStatus {
	return byState(m.st, onlineMachines(m, t), time.Now())
}

func (p *toolPage) hint(m *model) string {
	t, ok := p.tool(m)
	if !ok {
		return ""
	}
	if p.onMachines {
		if ms, i := p.selectedMachine(m, t); i >= 0 {
			s := machineState(m.st, ms[i], time.Now())
			return ms[i].Key + ": " + strings.TrimPrefix(s.word+" · "+s.detail, " · ")
		}
		return ""
	}
	q, i := p.selected(m)
	if i < 0 {
		return "The Waiting line: the agents that wait for a machine with " + t.Name + ", first in, first out inside each priority."
	}
	l := q[i]
	h := fmt.Sprintf("%s · %q · from %s", clean(l.User), clean(l.Task), clean(l.Guest))
	if l.Reason != "" {
		h += " · waits because: " + clean(l.Reason)
	}
	return h
}

func (p *toolPage) canEnd(m *model, l protocol.LeaseStatus) bool {
	return m.admin() || (m.st.User != "" && l.User == m.st.User)
}

func (p *toolPage) keys(m *model) []binding {
	ks := []binding{{"↑↓", "move"}, {"tab", "next section"}}
	if p.onMachines {
		t, _ := p.tool(m)
		if ms, _ := p.selectedMachine(m, t); len(ms) > 0 {
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
func (p *toolPage) actions(m *model, q []protocol.LeaseStatus, i int) []option {
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

func (p *toolPage) key(m *model, k tea.KeyMsg) tea.Cmd {
	if k.String() == "tab" || k.String() == "shift+tab" {
		p.onMachines = !p.onMachines
		return nil
	}
	if p.onMachines {
		t, _ := p.tool(m)
		ms, i := p.selectedMachine(m, t)
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
		} else if t, _ := p.tool(m); len(p.machines(m, t)) > 0 {
			ms := p.machines(m, t)
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

func (p *toolPage) endLease(m *model, l protocol.LeaseStatus) {
	m.warn("End the lease", fmt.Sprintf("End the lease of %s (%q)?", clean(l.User), clean(l.Task)), func() tea.Cmd {
		return m.do("POST", "/v1/leases/"+l.ID+"/release", nil, "The lease of "+clean(l.User)+" ended.")
	})
}

func (p *toolPage) moveUp(m *model, q []protocol.LeaseStatus, i int) {
	l, before := q[i], q[i-1]
	m.ask("Move up", fmt.Sprintf("Move %s (%q) up, before %s?", clean(l.User), clean(l.Task), clean(before.User)), func() tea.Cmd {
		return m.do("POST", "/v1/leases/"+l.ID+"/move", protocol.MoveRequest{Before: before.ID}, "Moved.")
	})
	p.cancelOnChange(m, q)
}

func (p *toolPage) moveDown(m *model, q []protocol.LeaseStatus, i int) {
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

func (p *toolPage) priority(m *model, l protocol.LeaseStatus) {
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
func (p *toolPage) cancelOnChange(m *model, q []protocol.LeaseStatus) {
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
