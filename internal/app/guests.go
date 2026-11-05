package app

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/csansoon/hostel/internal/protocol"
)

// guestList is the people on the server; admins invite, change roles, and
// remove; everyone can make a code for their other computer.
type guestList struct {
	sel   int
	users []protocol.User
	// code is the last invite code, shown once.
	code, codeFor string
	// err is the error of the last read of the users.
	err string
	// start is a question to ask when the page opens: "invite" (an admin
	// invites someone) or "self" (a code for the caller's other computer).
	start string
}

// usersRetryMsg reads the users again after an error.
type usersRetryMsg struct{ gen int }

type usersMsg struct {
	gen   int
	users []protocol.User
	err   error
}

type inviteMsg struct {
	gen  int
	resp protocol.InviteResponse
	who  string
	err  error
}

func (p *guestList) title() string { return "Guest list" }

func (p *guestList) load(m *model) tea.Cmd {
	switch p.start {
	case "invite":
		p.askInvite(m)
	case "self":
		p.askSelf(m)
	}
	p.start = ""
	c, gen := m.c, m.gen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		var users []protocol.User
		err := c.Do(ctx, "GET", "/v1/users", nil, &users)
		return usersMsg{gen: gen, users: users, err: err}
	}
}

func (p *guestList) other(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case usersMsg:
		if msg.gen != m.gen {
			return nil
		}
		if msg.err != nil {
			// Ask again after the same pause as the status.
			p.err = msg.err.Error()
			gen := m.gen
			return tea.Tick(retryAfter, func(time.Time) tea.Msg { return usersRetryMsg{gen: gen} })
		}
		p.users, p.err = msg.users, ""
		p.sel = min(p.sel, max(len(p.users)-1, 0))
	case usersRetryMsg:
		if msg.gen == m.gen {
			return p.load(m)
		}
	}
	return nil
}

func (p *guestList) body(m *model) string {
	var b strings.Builder
	if p.err != "" {
		b.WriteString("\n" + line(errStyle.Render(clean("! Cannot read the guest list: "+p.err+" (trying again)"))))
	}
	if p.users == nil {
		if p.err != "" {
			return b.String()
		}
		return "\n" + note("Reading the guest list …")
	}
	w := contentWidth(m)
	// The new code first: it is what the user waits for.
	if p.code != "" {
		b.WriteString("\n" + section("Invite code", w, false))
		b.WriteString(line(clean(p.codeFor)) + "\n")
		for _, l := range strings.Split(wrap("hostel connect "+p.code, w-3), "\n") {
			b.WriteString(line(accentStyle.Render(l)))
		}
		b.WriteString("\n" + note("Or paste the code in Hostel › Check in. It works once and ends in 24 hours."))
		b.WriteString(note("Hostel prints it again when you quit, to copy it."))
		b.WriteString(section("Guests", w, true))
	} else {
		b.WriteString("\n")
	}
	for i, u := range p.users {
		role := mutedStyle.Render(u.Role)
		if u.Role == protocol.RoleAdmin {
			role = fg(colBlue).Render(u.Role)
		}
		name := clean(u.Name)
		if u.Name == m.st.User {
			name += mutedStyle.Render("  (you)")
		}
		sub := plural(len(u.Computers), "computer") + " · since " + u.CreatedAt.Local().Format("2 Jan 2006")
		b.WriteString(item(w, i == p.sel, name, sub, role, "") + "\n\n")
	}
	return b.String()
}

func (p *guestList) hint(m *model) string {
	if p.sel >= len(p.users) {
		return ""
	}
	u := p.users[p.sel]
	computers := clean(strings.Join(u.Computers, ", "))
	if computers == "" {
		computers = "no computer"
	}
	return clean(u.Name) + " · " + u.Role + " since " + u.CreatedAt.Local().Format("2006-01-02") + " · " + computers
}

func (p *guestList) keys(m *model) []binding {
	ks := []binding{{"↑↓", "move"}}
	if m.admin() {
		ks = append(ks, binding{"i", "invite"}, binding{"r", "role"}, binding{"x", "remove"})
	}
	return append(ks, binding{"c", "your other computer"})
}

func (p *guestList) askSelf(m *model) {
	m.ask("Make a code for your other computer?", p.invite(m, protocol.InviteRequest{Self: true}, "Run this on your other computer:"))
}

func (p *guestList) askInvite(m *model) {
	m.choose("Invite a guest as: m member · a admin · another key cancels", map[string]func() tea.Cmd{
		"m": p.invite(m, protocol.InviteRequest{Role: protocol.RoleMember}, "Send this to the new member. They run it on their computer:"),
		"a": p.invite(m, protocol.InviteRequest{Role: protocol.RoleAdmin}, "Send this to the new admin. They run it on their computer:"),
	})
}

func (p *guestList) invite(m *model, req protocol.InviteRequest, who string) func() tea.Cmd {
	return func() tea.Cmd {
		c, gen := m.c, m.gen
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
			defer cancel()
			var resp protocol.InviteResponse
			err := c.Do(ctx, "POST", "/v1/invites", req, &resp)
			return inviteMsg{gen: gen, resp: resp, who: who, err: err}
		}
	}
}

func (p *guestList) key(m *model, k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "c":
		p.askSelf(m)
		return nil
	case "i":
		if m.admin() {
			p.askInvite(m)
		}
		return nil
	}
	if p.sel >= len(p.users) {
		return nil
	}
	u := p.users[p.sel]
	path := "/v1/users/" + url.PathEscape(u.Name)
	switch k.String() {
	case "r":
		if m.admin() {
			role := protocol.RoleAdmin
			if u.Role == protocol.RoleAdmin {
				role = protocol.RoleMember
			}
			m.ask(fmt.Sprintf("Make %s %s?", clean(u.Name), map[string]string{protocol.RoleAdmin: "an admin", protocol.RoleMember: "a member"}[role]), func() tea.Cmd {
				return m.do("PATCH", path, protocol.SetUserRequest{Role: role}, clean(u.Name)+" is now "+role+".")
			})
		}
	case "x":
		if m.admin() {
			m.ask(fmt.Sprintf("Remove %s? Their computers are disconnected and their leases end.", clean(u.Name)), func() tea.Cmd {
				return m.do("DELETE", path, nil, "Removed "+clean(u.Name)+".")
			})
		}
	default:
		p.sel = move(p.sel, len(p.users), k.String())
	}
	return nil
}

// addMachine makes a machine code (admins): the name, the platforms, and the
// labels of the machine, then the commands to run on it.
type addMachine struct {
	step   int // 0: name, 1: platforms, 2: labels, 3: working, 4: done
	field  textinput.Model
	req    protocol.MachineInviteRequest
	resp   protocol.MachineInviteResponse
	errMsg string
}

type machineInviteMsg struct {
	gen  int
	resp protocol.MachineInviteResponse
	err  error
}

func newAddMachine() *addMachine {
	f := textinput.New()
	f.Prompt = "> "
	f.Placeholder = "macbook"
	f.Focus()
	return &addMachine{field: f}
}

func (p *addMachine) title() string { return "Add a room" }

func (p *addMachine) typing() bool { return p.step <= 2 }

func (p *addMachine) body(m *model) string {
	w := contentWidth(m)
	var b strings.Builder
	b.WriteString(head("Add a room", "Connect a new machine to this server", ""))
	if p.step <= 2 {
		b.WriteString(section(fmt.Sprintf("Step %d of 3", p.step+1), w, false))
		if p.step > 0 {
			b.WriteString(line(field("Name", clean(p.req.Name))))
		}
		if p.step > 1 {
			b.WriteString(line(field("Platforms", clean(strings.Join(p.req.Platforms, ", ")))))
		}
		if p.step > 0 {
			b.WriteString("\n")
		}
		q := []string{"The name of the new machine (for example macbook or win-pc):", "Its platforms besides its OS, separated by commas:", "Its labels, separated by commas:"}[p.step]
		b.WriteString(line(q) + "\n" + line(p.field.View()))
	}
	switch p.step {
	case 3:
		b.WriteString(note("Making the machine code …"))
	case 4:
		b.WriteString(section("Machine code", w, false))
		b.WriteString(line("Run one of these on the machine "+boldStyle.Render(clean(p.resp.Machine))+", as an administrator.") + "\n")
		b.WriteString(note("macOS or Linux"))
		for _, l := range strings.Split(wrap(p.resp.Unix, w-3), "\n") {
			b.WriteString(line(accentStyle.Render(l)))
		}
		b.WriteString("\n" + note("Windows (PowerShell)"))
		for _, l := range strings.Split(wrap(p.resp.Windows, w-3), "\n") {
			b.WriteString(line(accentStyle.Render(l)))
		}
		b.WriteString("\n" + note("The code works once and ends at "+p.resp.ExpiresAt.Local().Format("2006-01-02 15:04")+"."))
		b.WriteString(note("The machine shows in Rooms when it connects."))
		b.WriteString(note("Hostel prints these commands again when you quit, to copy them."))
	}
	if p.errMsg != "" {
		b.WriteString("\n" + line(errStyle.Render(clean(p.errMsg))))
	}
	return b.String()
}

func (p *addMachine) keys(m *model) []binding {
	if p.typing() {
		return []binding{{"enter", "continue"}}
	}
	return nil
}

func (p *addMachine) hint(m *model) string {
	switch p.step {
	case 0:
		return "A short name for the machine: letters, digits, and dashes. The room is then home/<name>."
	case 1:
		return "For example android, or empty when the machine runs only its OS."
	case 2:
		return "Labels select machines, for example desktop for apps with a desktop."
	}
	return ""
}

func split(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func (p *addMachine) key(m *model, k tea.KeyMsg) tea.Cmd {
	if !p.typing() {
		return nil
	}
	switch k.Type {
	case tea.KeyEsc:
		if p.step == 0 {
			m.back()
			return nil
		}
		p.step, p.errMsg = p.step-1, ""
		p.field.SetValue(map[int]string{0: p.req.Name, 1: strings.Join(p.req.Platforms, ", ")}[p.step])
		p.field.CursorEnd()
		return nil
	case tea.KeyEnter:
		v := strings.TrimSpace(p.field.Value())
		switch p.step {
		case 0:
			if v == "" {
				p.errMsg = "Give a name."
				return nil
			}
			p.req.Name = v
		case 1:
			p.req.Platforms = split(v)
		case 2:
			p.req.Labels = split(v)
			p.step, p.errMsg = 3, ""
			c, gen, req := m.c, m.gen, p.req
			return func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
				defer cancel()
				var resp protocol.MachineInviteResponse
				err := c.Do(ctx, "POST", "/v1/machines/invites", req, &resp)
				return machineInviteMsg{gen: gen, resp: resp, err: err}
			}
		}
		p.step, p.errMsg = p.step+1, ""
		p.field.SetValue("")
		p.field.Placeholder = ""
		return nil
	}
	var cmd tea.Cmd
	p.field, cmd = p.field.Update(k)
	return cmd
}

// done takes the answer of the machine code request.
func (p *addMachine) done(r machineInviteMsg) {
	if r.err != nil {
		p.step, p.errMsg = 0, "Failed: "+r.err.Error()
		p.field.SetValue(p.req.Name)
		return
	}
	p.step, p.resp = 4, r.resp
}

func (p *addMachine) other(m *model, msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	p.field, cmd = p.field.Update(msg)
	return cmd
}
