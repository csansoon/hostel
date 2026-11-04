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
	icon, label, hint string
	open              func() page
	run               func(m *model) tea.Cmd
}

func (p *frontDesk) entries(m *model) []deskEntry {
	var out []deskEntry
	if m.admin() {
		out = append(out, deskEntry{"✉", "Invite a guest", "Invite a guest: make an invite code for a new member or admin.", func() page { return &guestList{start: "invite"} }, nil})
	} else {
		out = append(out, deskEntry{"✉", "Add your other computer", "Add your other computer: make a code that connects it as you.", func() page { return &guestList{start: "self"} }, nil})
	}
	out = append(out, deskEntry{"☰", "Guest list", "Guest list: the people on this server; change a role, or remove someone.", func() page { return &guestList{} }, nil})
	if m.admin() {
		out = append(out, deskEntry{"＋", "Add a room", "Add a room: connect a new machine to this server; it shows in Rooms.", func() page { return newAddMachine() }, nil})
	}
	if tag := m.serverUpdateAvailable(); tag != "" {
		out = append(out, deskEntry{"⇪", "Update the server",
			fmt.Sprintf("Update the server: install Hostel %s on the server (it runs %s); it starts again by itself, and leases continue.", tag, m.info.Version),
			nil, func(m *model) tea.Cmd {
				m.ask(fmt.Sprintf("Update the server from %s to %s?", m.info.Version, tag), func() tea.Cmd { return m.startServerUpdate(tag) })
				return nil
			}})
	}
	return append(out, deskEntry{"⏻", "Check out", "Check out: disconnect this computer from the server (its agents too).", nil, checkOut})
}

// checkOut asks, then disconnects this computer.
func checkOut(m *model) tea.Cmd {
	g := m.guest
	m.ask(fmt.Sprintf("Check out from %s? Agents on this computer lose the connection too.", g.URL()), func() tea.Cmd {
		return func() tea.Msg {
			warning, err := connect.Disconnect(context.Background(), g)
			return checkedOutMsg{url: g.URL(), warning: warning, err: err}
		}
	})
	return nil
}

func (p *frontDesk) body(m *model) string {
	var info strings.Builder
	info.WriteString(field("Server", boldStyle.Render(clean(m.guest.URL()))) + "\n")
	if m.st.ServerID != "" {
		info.WriteString(field("Server ID", clean(m.st.ServerID)) + "\n")
	}
	if m.info.Version != "" {
		info.WriteString(field("Version", clean(m.info.Version)) + "\n")
	}
	role := pill(m.st.Role, colMuted)
	if m.admin() {
		role = pill(m.st.Role, colBlue)
	}
	info.WriteString(field("You", clean(m.st.User)+" "+role) + "\n")
	if p.me.Computer != "" {
		info.WriteString(field("Computer", clean(p.me.Computer)))
	}
	var rows [][]string
	for _, e := range p.entries(m) {
		rows = append(rows, []string{accentStyle.Render(e.icon) + "  " + e.label})
	}
	return "\n" + card("Reception", info.String(), min(m.screenWidth(), 90), false) + "\n\n" + table(rows, p.sel, nil)
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
