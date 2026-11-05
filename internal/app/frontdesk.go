package app

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/csansoon/hostel/internal/connect"
	"github.com/csansoon/hostel/internal/protocol"
)

// frontDesk is the place for the server's people and rooms, the server
// itself, and check out (owner decision, 2026-11-05).
type frontDesk struct{ sel int }

func newFrontDesk() *frontDesk { return &frontDesk{} }

type checkedOutMsg struct {
	url     string
	warning error
	err     error
}

func (p *frontDesk) title() string { return "Front desk" }

// deskEntry is an entry of the Front desk: a page, or an action (run).
type deskEntry struct {
	icon, label, sub, hint string
	right                  string
	open                   func() page
	run                    func(m *model) tea.Cmd
}

func (p *frontDesk) entries(m *model) []deskEntry {
	out := []deskEntry{{icon: "👥", label: "Guests", sub: "Invite people; change roles; remove people",
		hint: "Guests: the people on this server, and invite codes.", open: func() page { return &guestList{} }}}
	if !m.admin() {
		out[0].sub = "The people on this server; add your other computer"
	}
	if m.admin() {
		out = append(out, deskEntry{icon: "🔑", label: "Rooms", sub: "Add a machine; stop new leases; repair",
			hint:  "Rooms: manage the machines of this server.",
			right: mutedStyle.Render(plural(len(m.st.Machines), "machine")), open: func() page { return &roomAdmin{} }})
	}
	server := deskEntry{icon: "🏨", label: "This server", sub: "Address, version, and updates",
		hint: "This server: its address, its version, and you.", open: func() page { return &serverPage{} }}
	if m.info.Version != "" {
		server.right = mutedStyle.Render(clean(m.info.Version))
	}
	if m.serverUpdateAvailable() != "" {
		server.right = fg(colYellow).Render(m.latest + " available")
	}
	out = append(out, server)
	return append(out, deskEntry{icon: "👋", label: "Check out", sub: "Disconnect this computer",
		hint: "Check out: disconnect this computer from the server (its agents too).", run: checkOut})
}

// checkOut asks, then disconnects this computer.
func checkOut(m *model) tea.Cmd {
	g := m.guest
	if m.demo {
		m.msg = "Demo: check out does nothing here."
		return nil
	}
	m.warn("Check out", fmt.Sprintf("Check out from %s? Agents on this computer lose the connection too.", g.URL()), func() tea.Cmd {
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
		b.WriteString(iconItem(w, i == p.sel, e.icon, e.label, e.sub, e.right, "") + "\n\n")
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

// serverPage is the server: its address, version, and ID, the user and the
// computer, and Update the server for admins when a release is newer.
type serverPage struct{ me protocol.Me }

type meMsg struct {
	gen int
	me  protocol.Me
}

func (p *serverPage) title() string { return "This server" }

// load reads /v1/me when the page opens, and again with each status answer
// until a read works.
func (p *serverPage) load(m *model) tea.Cmd {
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

func (p *serverPage) other(m *model, msg tea.Msg) tea.Cmd {
	if msg, ok := msg.(meMsg); ok && msg.gen == m.gen {
		p.me = msg.me
	}
	return nil
}

func (p *serverPage) body(m *model) string {
	w := contentWidth(m)
	var b strings.Builder
	b.WriteString(head("This server", clean(m.guest.URL()), ""))
	if tag := m.serverUpdateAvailable(); tag != "" {
		b.WriteString(iconItem(w, true, "⬆️", "Update the server", fmt.Sprintf("Install %s; the server starts again; leases and jobs continue", tag),
			fg(colYellow).Render(tag+" available"), "") + "\n\n\n")
	}
	b.WriteString(section("Server", w, false))
	b.WriteString(line(field("Address", boldStyle.Render(clean(m.guest.URL())))))
	if m.info.Version != "" {
		b.WriteString(line(field("Version", clean(m.info.Version))))
	}
	if m.st.ServerID != "" {
		b.WriteString(line(field("Server ID", mutedStyle.Render(clean(m.st.ServerID)))))
	}
	b.WriteString(section("You", w, true))
	role := mutedStyle.Render(clean(m.st.Role))
	if m.admin() {
		role = fg(colBlue).Render(clean(m.st.Role))
	}
	b.WriteString(line(field("Name", clean(m.st.User)+mutedStyle.Render(" · ")+role)))
	if p.me.Computer != "" {
		b.WriteString(line(field("Computer", clean(p.me.Computer))))
	}
	return b.String()
}

func (p *serverPage) hint(m *model) string {
	if tag := m.serverUpdateAvailable(); tag != "" {
		return fmt.Sprintf("Update the server: install Hostel %s on the server (it runs %s); it starts again by itself; leases and jobs continue.", tag, m.info.Version)
	}
	return "This server and you."
}

func (p *serverPage) keys(m *model) []binding {
	if m.serverUpdateAvailable() != "" {
		return []binding{{"enter", "update the server"}}
	}
	return nil
}

func (p *serverPage) key(m *model, k tea.KeyMsg) tea.Cmd {
	if tag := m.serverUpdateAvailable(); tag != "" && k.String() == "enter" {
		m.ask("Update the server", fmt.Sprintf("Update the server from %s to %s?", m.info.Version, tag), func() tea.Cmd { return m.startServerUpdate(tag) })
	}
	return nil
}
