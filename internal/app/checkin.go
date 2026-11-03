package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/csansoon/hostel/internal/connect"
	"github.com/csansoon/hostel/internal/invite"
)

// connectTimeout bounds the connection with an invite code (it can try
// several addresses).
const connectTimeout = 2 * time.Minute

// checkIn is the first page when the computer is not connected: Check in
// with an invite code, or Leave.
type checkIn struct {
	note  string
	sel   int
	step  int // 0: the menu, 1: the code, 2: the name, 3: connecting
	field textinput.Model
	code  invite.Code
	err   string
}

func newCheckIn(note string) *checkIn {
	f := textinput.New()
	f.Prompt = "> "
	f.CharLimit = 2048
	return &checkIn{note: note, field: f}
}

type connectedMsg struct {
	res connect.Result
	err error
}

func (p *checkIn) title() string { return "" }

func (p *checkIn) typing() bool { return p.step == 1 || p.step == 2 }

func (p *checkIn) body(m *model) string {
	var b strings.Builder
	if p.note != "" {
		b.WriteString("\n" + clean(p.note) + "\n")
	}
	w := min(m.screenWidth(), 90)
	switch p.step {
	case 0:
		b.WriteString("\n  This computer is not connected to a Hostel server.\n\n")
		b.WriteString(table([][]string{{accentStyle.Render("⚿") + "  Check in"}, {accentStyle.Render("⏻") + "  Leave"}}, p.sel, nil))
	case 1:
		b.WriteString("\n" + card("Check in", "Paste the invite code from an admin of your server.\n\n"+p.field.View(), w, true) + "\n")
	case 2:
		b.WriteString("\n" + card("Check in", field("Server", clean(strings.Join(p.code.Addresses, ", ")))+"\n\nYour name on this server:\n\n"+p.field.View(), w, true) + "\n")
	case 3:
		b.WriteString("\n  Checking in …\n")
	}
	if p.err != "" {
		b.WriteString("\n  " + errStyle.Render(clean(p.err)) + "\n")
	}
	return b.String()
}

func (p *checkIn) hint(m *model) string {
	switch p.step {
	case 0:
		if p.sel == 0 {
			return "Check in: connect this computer with an invite code. An admin makes one in Front desk › Guest list."
		}
		return "Leave: quit the app."
	case 1:
		return "The code starts with hostel1-. It works once."
	case 2:
		return "The name that others see on your leases."
	}
	return ""
}

func (p *checkIn) keys(m *model) []binding {
	switch p.step {
	case 1, 2:
		return []binding{{"enter", "continue"}}
	case 3:
		return nil
	}
	return []binding{{"↑↓", "move"}, {"enter", "open"}}
}

func (p *checkIn) key(m *model, k tea.KeyMsg) tea.Cmd {
	switch p.step {
	case 0:
		switch k.String() {
		case "enter":
			if p.sel == 1 {
				return tea.Quit
			}
			p.step, p.err = 1, ""
			p.field.SetValue("")
			p.field.Placeholder = "hostel1-…"
			return p.field.Focus()
		default:
			p.sel = move(p.sel, 2, k.String())
		}
	case 1, 2:
		switch k.Type {
		case tea.KeyEsc:
			p.step--
			if p.step == 1 {
				p.field.SetValue("")
			}
			return nil
		case tea.KeyEnter:
			return p.next(m)
		}
		var cmd tea.Cmd
		p.field, cmd = p.field.Update(k)
		return cmd
	}
	return nil
}

func (p *checkIn) other(m *model, msg tea.Msg) tea.Cmd {
	if r, ok := msg.(connectedMsg); ok {
		if r.err != nil {
			p.step, p.err = 1, "Check in failed: "+r.err.Error()
			p.field.SetValue("")
			return p.field.Focus()
		}
		m.useGuest(r.res.Guest)
		m.pages = []page{newMainMenu()}
		m.msg = fmt.Sprintf("Checked in to %s as %s (%s) from %s.", r.res.Guest.URL(), r.res.Response.User, r.res.Response.Role, r.res.Computer)
		return m.Init()
	}
	var cmd tea.Cmd
	p.field, cmd = p.field.Update(msg)
	return cmd
}

// next goes from the code to the name, and from the name to the connection.
func (p *checkIn) next(m *model) tea.Cmd {
	v := strings.TrimSpace(p.field.Value())
	if p.step == 1 {
		code, err := invite.Decode(v)
		if err != nil {
			p.err = err.Error()
			return nil
		}
		p.code, p.err, p.step = code, "", 2
		p.field.Placeholder = "Your Name"
		p.field.SetValue(connect.GitUserName())
		p.field.CursorEnd()
		return nil
	}
	if v == "" {
		p.err = "Give your name."
		return nil
	}
	p.step, p.err = 3, ""
	code := p.code
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
		defer cancel()
		res, err := connect.Connect(ctx, code, v)
		return connectedMsg{res: res, err: err}
	}
}
