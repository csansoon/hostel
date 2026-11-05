package app

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/csansoon/hostel/internal/connect"
	"github.com/csansoon/hostel/internal/protocol"
)

// frontDesk is the server page: the server, you, and check out.
type frontDesk struct {
	sel int
	me  protocol.Me
}

func newFrontDesk() *frontDesk { return &frontDesk{} }

type meMsg struct {
	gen int
	me  protocol.Me
}

type checkedOutMsg struct {
	url     string
	warning error
	err     error
}

func (p *frontDesk) title() string { return "Front desk" }

// load reads /v1/me when the page opens, and again with each status answer
// until a read works.
func (p *frontDesk) load(m *model) tea.Cmd {
	if p.me.User != "" {
		return nil
	}
	c, gen := m.c, m.gen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		var me protocol.Me
		if err := c.Do(ctx, "GET", "/v1/me", nil, &me); err != nil {
			return nil
		}
		return meMsg{gen: gen, me: me}
	}
}

// deskEntry is an entry of the Front desk: a page, or an action that asks
// first (run).
type deskEntry struct {
	icon, label, sub, hint string
	open                   func() page
	run                    func(m *model) tea.Cmd
}

func (p *frontDesk) entries(m *model) []deskEntry {
	var out []deskEntry
	if m.admin() {
		out = append(out, deskEntry{"✉", "Invite a guest", "A code for a new member or admin", "Invite a guest: make an invite code for a new member or admin.", func() page { return &guestList{start: "invite"} }, nil})
	} else {
		out = append(out, deskEntry{"✉", "Add your other computer", "A code that connects it as you", "Add your other computer: make a code that connects it as you.", func() page { return &guestList{start: "self"} }, nil})
	}
	out = append(out, deskEntry{"☰", "Guest list", "The people on this server", "Guest list: the people on this server; change a role, or remove someone.", func() page { return &guestList{} }, nil})
	if m.admin() {
		out = append(out, deskEntry{"+", "Add a room", "Connect a new machine to this server", "Add a room: connect a new machine to this server; it shows in Rooms.", func() page { return newAddMachine() }, nil})
	}
	if tag := m.serverUpdateAvailable(); tag != "" {
		out = append(out, deskEntry{"⇪", "Update the server", "Install " + tag + " on the server",
			fmt.Sprintf("Update the server: install Hostel %s on the server (it runs %s); it starts again by itself; leases and jobs continue.", tag, m.info.Version),
			nil, func(m *model) tea.Cmd {
				m.ask(fmt.Sprintf("Update the server from %s to %s?", m.info.Version, tag), func() tea.Cmd { return m.startServerUpdate(tag) })
				return nil
			}})
	}
	return append(out, deskEntry{"⏻", "Check out", "Disconnect this computer", "Check out: disconnect this computer from the server (its agents too).", nil, checkOut})
}

// checkOut asks, then disconnects this computer.
func checkOut(m *model) tea.Cmd {
	g := m.guest
	if m.demo {
		m.msg = "Demo: check out does nothing here."
		return nil
	}
	m.ask(fmt.Sprintf("Check out from %s? Agents on this computer lose the connection too.", g.URL()), func() tea.Cmd {
		return func() tea.Msg {
			warning, err := connect.Disconnect(context.Background(), g)
			return checkedOutMsg{url: g.URL(), warning: warning, err: err}
		}
	})
	return nil
}

func (p *frontDesk) body(m *model) string {
	w := contentWidth(m)
	var b strings.Builder
	b.WriteString("\n")
	for i, e := range p.entries(m) {
		b.WriteString(item(w, i == p.sel, e.icon+"  "+e.label, "   "+e.sub, "", "") + "\n\n")
	}
	b.WriteString(section("This server", w, true))
	b.WriteString(line(field("Server", boldStyle.Render(clean(m.guest.URL())))))
	if m.info.Version != "" {
		b.WriteString(line(field("Version", clean(m.info.Version))))
	}
	if m.st.ServerID != "" {
		b.WriteString(line(field("Server ID", mutedStyle.Render(clean(m.st.ServerID)))))
	}
	role := mutedStyle.Render(clean(m.st.Role))
	if m.admin() {
		role = fg(colBlue).Render(clean(m.st.Role))
	}
	b.WriteString(line(field("You", clean(m.st.User)+mutedStyle.Render(" · ")+role)))
	if p.me.Computer != "" {
		b.WriteString(line(field("Computer", clean(p.me.Computer))))
	}
	return b.String()
}

func (p *frontDesk) hint(m *model) string {
	es := p.entries(m)
	return es[min(p.sel, len(es)-1)].hint
}

func (p *frontDesk) keys(m *model) []binding { return []binding{{"↑↓", "move"}, {"enter", "open"}} }

func (p *frontDesk) key(m *model, k tea.KeyMsg) tea.Cmd {
	entries := p.entries(m)
	if k.String() != "enter" {
		p.sel = move(p.sel, len(entries), k.String())
		return nil
	}
	e := entries[min(p.sel, len(entries)-1)]
	if e.open != nil {
		return m.open(e.open())
	}
	return e.run(m)
}

func (p *frontDesk) other(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case meMsg:
		if msg.gen == m.gen {
			p.me = msg.me
		}
	}
	return nil
}
