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

// lobby is the waiting line first, then the tools (screens with one line
// each, software in a short form). sel is the key of the selected item.
type lobby struct{ sel string }

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
	sortLine(out)
	return out
}

// sortLine orders leases as the waiting line (a lease that the scheduler has
// not placed yet comes last).
func sortLine(out []protocol.LeaseStatus) {
	slices.SortStableFunc(out, func(a, b protocol.LeaseStatus) int {
		if (a.Position == 0) != (b.Position == 0) {
			if a.Position == 0 {
				return 1
			}
			return -1
		}
		return a.Position - b.Position
	})
}

// asks reports whether a lease asked for a tool.
func asks(l protocol.LeaseStatus, tool string) bool {
	return slices.ContainsFunc(l.Tools, func(t string) bool { name, _, _ := strings.Cut(t, ":"); return name == tool })
}

// onlineTemplates are the machines of a tool that are not offline: the
// Lobby leaves out the offline ones (owner decision, 2026-11-03).
func onlineTemplates(m *model, p protocol.Tool) []protocol.TemplateStatus {
	var out []protocol.TemplateStatus
	for _, tm := range p.Templates {
		key := tm.Key
		if i := slices.IndexFunc(m.st.Templates, func(ms protocol.TemplateStatus) bool { return ms.Key == key }); i >= 0 && online(m.st, m.st.Templates[i]) {
			out = append(out, m.st.Templates[i])
		}
	}
	return out
}

// freeTemplates counts the machines of a tool that can take a lease now.
func freeTemplates(m *model, p protocol.Tool) int {
	n := 0
	for _, ms := range onlineTemplates(m, p) {
		if templateFree(m.st, ms, p.Name) {
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

// waitingLine is every queued lease in the order of the line.
func waitingLine(st protocol.Status) []protocol.LeaseStatus {
	var out []protocol.LeaseStatus
	for _, l := range st.Leases {
		if l.State == protocol.LeaseQueued {
			out = append(out, l)
		}
	}
	sortLine(out)
	return out
}

// lobbyTools splits the tools. A software tool with waiting guests comes
// first, as the line matters more than the order of the list.
func lobbyTools(m *model) (screens, software []protocol.Tool) {
	for _, t := range m.tools {
		if t.Screen {
			screens = append(screens, t)
		} else {
			software = append(software, t)
		}
	}
	slices.SortStableFunc(software, func(a, b protocol.Tool) int {
		if (a.Waiting > 0) != (b.Waiting > 0) {
			if a.Waiting > 0 {
				return -1
			}
			return 1
		}
		return 0
	})
	return screens, software
}

// order is the keys of the selectable items, with the selected index.
func (p *lobby) order(m *model) ([]string, int) {
	var keys []string
	for _, l := range waitingLine(m.st) {
		keys = append(keys, "l:"+l.ID)
	}
	screens, software := lobbyTools(m)
	for _, t := range append(screens, software...) {
		keys = append(keys, "t:"+t.Name)
	}
	i := slices.Index(keys, p.sel)
	if i < 0 && len(keys) > 0 {
		i, p.sel = 0, keys[0]
	}
	return keys, i
}

func (p *lobby) body(m *model) string {
	w := contentWidth(m)
	p.order(m)
	var b strings.Builder
	q := waitingLine(m.st)
	title := "Waiting line"
	if len(q) > 0 {
		title += fmt.Sprintf(" · %d waiting", len(q))
	}
	b.WriteString("\n" + heading(title, w) + "\n\n")
	if len(q) == 0 {
		b.WriteString(note("Nobody waiting."))
	}
	for i, l := range q {
		left := fmt.Sprintf("%d. ", i+1) + clean(l.User) + "  " + mutedStyle.Render(fmt.Sprintf("%q", clean(l.Task))) + "  " + fg(colBlue).Render(clean(strings.Join(l.Tools, " ")))
		b.WriteString(row(w, p.sel == "l:"+l.ID, spread(left, fg(colYellow).Render(waitText(l.EstimatedWaitSeconds)), w-3)) + "\n")
	}
	switch {
	case m.toolsErr != "" && !m.toolsRead:
		b.WriteString("\n" + note(errStyle.Render(clean("Cannot read the tools: "+m.toolsErr))))
		return b.String()
	case !m.toolsRead:
		b.WriteString("\n" + note("Reading the tools …"))
		return b.String()
	case len(m.tools) == 0:
		b.WriteString("\n" + note("No machine lists a tool yet. An admin adds a machine in Rooms."))
		return b.String()
	}
	if m.toolsErr != "" {
		b.WriteString("\n" + note(errStyle.Render(clean("! Cannot read the tools: "+m.toolsErr+" (older data below; trying again)"))))
	}
	screens, software := lobbyTools(m)
	if len(screens) > 0 {
		b.WriteString(section("Screens", w, true))
		nameW := 0
		for _, t := range screens {
			nameW = max(nameW, lipgloss.Width(clean(t.Name)))
		}
		for _, t := range screens {
			detail := plural(len(t.Templates), "machine")
			if t.Waiting > 0 {
				detail += fmt.Sprintf(" · %d waiting", t.Waiting)
			}
			b.WriteString(row(w, p.sel == "t:"+t.Name, spread(cell(boldStyle.Render(clean(t.Name)), nameW)+"   "+m.vacancy(t), mutedStyle.Render(detail), w-3)) + "\n")
		}
	}
	if len(software) > 0 {
		b.WriteString(section("Software", w, true))
		b.WriteString(p.softwareLines(m, software, w))
	}
	return b.String()
}

// softwareLines is the software tools, separated by dots and wrapped; the
// line with the selected tool has the bar.
func (p *lobby) softwareLines(m *model, software []protocol.Tool, w int) string {
	var b strings.Builder
	cur, width, sel := "", 0, false
	flush := func() {
		if cur != "" {
			b.WriteString(row(w, sel, cur) + "\n")
		}
		cur, width, sel = "", 0, false
	}
	sep := mutedStyle.Render("  ·  ")
	for _, t := range software {
		name := clean(t.Name)
		style := lipgloss.NewStyle()
		switch {
		case p.sel == "t:"+t.Name:
			style = accentStyle.Bold(true)
		case len(onlineTemplates(m, t)) == 0:
			style = mutedStyle
		case t.Waiting > 0:
			style = fg(colYellow)
		}
		n := lipgloss.Width(name)
		if cur != "" && width+5+n > w-3 {
			flush()
		}
		if cur != "" {
			cur += sep
			width += 5
		}
		cur += style.Render(name)
		width += n
		sel = sel || p.sel == "t:"+t.Name
	}
	flush()
	return b.String()
}

// vacancy is a tool's free machines: ● 2 vacant, ◉ all occupied, or no
// machine online; it glows when it changes.
func (m *model) vacancy(t protocol.Tool) string {
	free, total := freeTemplates(m, t), len(onlineTemplates(m, t))
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
	next := "next machine " + waitText(t.EstimatedWaitSeconds)
	if online == 0 {
		next = "no machine online"
	}
	return fg(colYellow).Render(spin("◴", "◷", "◶", "◵")+fmt.Sprintf(" %d waiting", t.Waiting)) + mutedStyle.Render(" · "+next)
}

func (p *lobby) tool(m *model) (protocol.Tool, bool) {
	i := slices.IndexFunc(m.tools, func(t protocol.Tool) bool { return "t:"+t.Name == p.sel })
	if i < 0 {
		return protocol.Tool{}, false
	}
	return m.tools[i], true
}

func (p *lobby) hint(m *model) string {
	p.order(m)
	if id, ok := strings.CutPrefix(p.sel, "l:"); ok {
		for _, l := range waitingLine(m.st) {
			if l.ID == id {
				return clean(l.User) + " waits for " + clean(strings.Join(l.Tools, " and ")) + "."
			}
		}
	}
	t, ok := p.tool(m)
	if !ok {
		return ""
	}
	h := t.Name + ": " + toolText(t)
	if t.Screen {
		h += " · a screen: it starts for each lease that asks for it"
	}
	return h
}

func (p *lobby) keys(m *model) []binding { return []binding{{"↑↓", "move"}, {"enter", "open"}} }

func (p *lobby) key(m *model, k tea.KeyMsg) tea.Cmd {
	keys, i := p.order(m)
	if i < 0 {
		return nil
	}
	if k.String() != "enter" {
		p.sel = keys[move(i, len(keys), k.String())]
		return nil
	}
	if id, ok := strings.CutPrefix(p.sel, "l:"); ok {
		for _, l := range waitingLine(m.st) {
			if l.ID == id && len(l.Tools) > 0 {
				name, _, _ := strings.Cut(l.Tools[0], ":")
				return m.open(&toolPage{name: name, sel: id})
			}
		}
		return nil
	}
	if t, ok := p.tool(m); ok {
		return m.open(&toolPage{name: t.Name, onTemplates: true})
	}
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
	onTemplates bool
	templateSel string
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
	templates := p.templates(m, t)
	q, qsel := p.selected(m)
	if len(q) == 0 {
		p.onTemplates = true
	}
	var b strings.Builder
	if m.toolsErr != "" {
		b.WriteString(margin + errStyle.Render(clean("! Cannot read the tools: "+m.toolsErr+" (older data below; trying again)")) + "\n")
	}

	typical := "typical stay not known yet"
	if t.TypicalLeaseSeconds > 0 {
		typical = "typical stay " + ago(time.Duration(t.TypicalLeaseSeconds)*time.Second)
	}
	b.WriteString(head(clean(t.Name), clean(toolText(t)), m.vacancy(t)+"     "+waitLine(t, len(templates))+"     "+mutedStyle.Render(typical)))

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
	if len(templates) == 0 {
		b.WriteString(margin + "   " + mutedStyle.Render("No machine with "+clean(t.Name)+" is online.") + "\n")
	}
	_, msel := p.selectedTemplate(m, t)
	nameW, optW := 0, 0
	for _, ms := range templates {
		nameW = max(nameW, lipgloss.Width(clean(ms.Key)))
		optW = max(optW, lipgloss.Width(clean(templateOptions(t, ms.Key))))
	}
	for i, ms := range templates {
		s := templateState(m.st, ms, now)
		word := m.word("m:"+ms.Key, s)
		line := s.dot() + " " + cell(boldStyle.Render(clean(ms.Key)), nameW) + "   " + cell(word, len("No new leases")) + "   " +
			cell(fg(colBlue).Render(clean(templateOptions(t, ms.Key))), optW) + "   " + mutedStyle.Render(s.detail)
		b.WriteString(row(w, p.onTemplates && i == msel, line) + "\n")
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
		right := fg(colYellow).Render(spin("◴", "◷", "◶", "◵") + " machine in " + waitText(l.EstimatedWaitSeconds))
		b.WriteString(item(w, !p.onTemplates && i == qsel, title, sub, right, "") + "\n\n")
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

// templateOptions are the options of a tool on one machine.
func templateOptions(t protocol.Tool, key string) string {
	i := slices.IndexFunc(t.Templates, func(tm protocol.ToolTemplate) bool { return tm.Key == key })
	if i < 0 {
		return ""
	}
	return strings.Join(t.Templates[i].Options, ", ")
}

// guideSummary is the first paragraph of a tool's guide, in one line.
func guideSummary(guide string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(guide), "\n\n")
	return strings.Join(strings.Fields(first), " ")
}

// selectedTemplate returns the machines of the card and the index of the
// selected one (the first when it is gone).
func (p *toolPage) selectedTemplate(m *model, t protocol.Tool) ([]protocol.TemplateStatus, int) {
	ms := p.templates(m, t)
	i := slices.IndexFunc(ms, func(x protocol.TemplateStatus) bool { return x.Key == p.templateSel })
	if i < 0 && len(ms) > 0 {
		i, p.templateSel = 0, ms[0].Key
	}
	return ms, i
}

// templates are the online machines that have the tool, in state order.
func (p *toolPage) templates(m *model, t protocol.Tool) []protocol.TemplateStatus {
	return byState(m.st, onlineTemplates(m, t), time.Now())
}

func (p *toolPage) hint(m *model) string {
	t, ok := p.tool(m)
	if !ok {
		return ""
	}
	if p.onTemplates {
		if ms, i := p.selectedTemplate(m, t); i >= 0 {
			s := templateState(m.st, ms[i], time.Now())
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
	if p.onTemplates {
		t, _ := p.tool(m)
		if ms, _ := p.selectedTemplate(m, t); len(ms) > 0 {
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
		p.onTemplates = !p.onTemplates
		return nil
	}
	if p.onTemplates {
		t, _ := p.tool(m)
		ms, i := p.selectedTemplate(m, t)
		// Down from the last machine goes on to the waiting line.
		if q, _ := p.selected(m); (k.String() == "down" || k.String() == "j") && i == len(ms)-1 && len(q) > 0 {
			p.onTemplates, p.sel = false, q[0].ID
			return nil
		}
		if i < 0 {
			return nil
		}
		if k.String() == "enter" {
			return m.open(&templatePage{templateKey: ms[i].Key})
		}
		p.templateSel = ms[move(i, len(ms), k.String())].Key
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
		} else if t, _ := p.tool(m); len(p.templates(m, t)) > 0 {
			ms := p.templates(m, t)
			p.onTemplates, p.templateSel = true, ms[len(ms)-1].Key
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
