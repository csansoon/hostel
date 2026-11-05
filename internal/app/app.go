// Package app is the Hostel app: "hostel" without arguments in a terminal
// (owner decision, 2026-11-03; m10b-implementation.md). It is for people;
// agents use the "hostel <verb>" commands. The labels of the app may be
// themed (Lobby, Rooms, Front desk); the footer describes the selected item
// in plain words.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/csansoon/hostel/internal/client"
	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
)

const (
	// requestTimeout bounds one request; statusWait is the long poll of the
	// status (the server allows up to 50 s).
	requestTimeout = 30 * time.Second
	statusWait     = 45 * time.Second
	// retryAfter is the pause after a failed status request.
	retryAfter = 5 * time.Second
)

// Run starts the app on the terminal (full screen) and returns when the user
// quits.
func Run(version string, stdout io.Writer) error {
	m := newModel(version)
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	// The screen cuts long lines (invite codes, join commands): the app
	// prints them again after the full screen, where they can be copied.
	for _, s := range m.keep {
		fmt.Fprintln(stdout, s)
	}
	return err
}

// api is the server for the app: a client, or the demo world.
type api interface {
	Do(ctx context.Context, method, path string, in, out any) error
}

// page is one screen of the app. The model keeps a stack of pages; Esc goes
// back to the page below.
type page interface {
	// title is the page's name in the header ("" for the first page).
	title() string
	// body is the page without header and footer.
	body(m *model) string
	// keys are the keys of the page for the footer (the app adds esc, ?,
	// and q).
	keys(m *model) []binding
	// hint describes the selected item in the footer ("" for none).
	hint(m *model) string
	// key handles a key; it returns a command or nil.
	key(m *model, k tea.KeyMsg) tea.Cmd
}

// textPage is a page with a text field: q and Esc type into the field.
type textPage interface {
	page
	typing() bool
}

// loader is a page that reads its own data when it opens and when the
// status changes.
type loader interface {
	load(m *model) tea.Cmd
}

type model struct {
	version       string
	width, height int

	guest *config.Guest
	c     api
	// demo: made-up data, no server (RunDemo); nothing changes this computer.
	demo bool
	// gen counts connections: answers for an older connection are dropped.
	gen int

	st        protocol.Status
	loaded    bool
	platforms []protocol.Tool
	// platformsRead is true after the first answer of GET /v1/platforms;
	// platformsErr is the error of the last one.
	platformsRead bool
	platformsErr  string
	// platformsSeq numbers the platform reads: only the answer to the last
	// one counts (an older answer can come later).
	platformsSeq int
	info         protocol.Info
	// latest is the tag of the latest release (the background check);
	// updated is true after the app's Update installed it.
	latest  string
	updated bool

	pages []page
	// err is the last data error; msg is the result of the last action.
	err, msg string
	// pop is the open popup: a choice, or a question before a change.
	pop *popup
	// keep is the text to print after quit (codes and commands to copy).
	keep []string
	// help shows all keys.
	help   bool
	anim   animState
	street street
}

// binding is a key and what it does, for the footer and the key list.
type binding struct{ key, label string }

// width is the screen width (80 before the terminal tells it).
func (m *model) screenWidth() int {
	if m.width <= 0 {
		return 80
	}
	return m.width
}

// keepText shows text in full after quit.
func (m *model) keepText(s string) { m.keep = append(m.keep, s) }

// wrap cuts a long word (a code, a command) into lines of width cells.
func wrap(s string, width int) string {
	width = max(width, 20)
	var lines []string
	for len(s) > width {
		lines = append(lines, s[:width])
		s = s[width:]
	}
	return strings.Join(append(lines, s), "\n")
}

func newModel(version string) *model {
	m := &model{version: version}
	g, err := config.LoadGuest()
	if err == nil {
		m.useGuest(g)
	}
	if m.c == nil {
		m.pages = []page{newCheckIn("")}
	} else {
		m.pages = []page{newMainMenu()}
	}
	return m
}

// useGuest takes a connection (after check in, or at start).
func (m *model) useGuest(g *config.Guest) {
	c, err := client.New(g)
	if err != nil {
		m.err = err.Error()
		return
	}
	m.guest, m.c = g, c
	m.gen++
	m.st, m.loaded, m.platforms, m.info = protocol.Status{}, false, nil, protocol.Info{}
	m.platformsRead, m.platformsErr = false, ""
}

// checkOut forgets the connection and shows Check in.
func (m *model) checkOut(note string) {
	m.guest, m.c = nil, nil
	m.gen++
	m.st, m.loaded = protocol.Status{}, false
	m.pages, m.pop, m.msg, m.err = []page{newCheckIn(note)}, nil, "", ""
}

func (m *model) Init() tea.Cmd {
	if m.c == nil {
		return m.checkLatest()
	}
	return tea.Batch(m.pollStatus(""), m.readInfo(), m.checkLatest(), m.startCars())
}

func (m *model) top() page { return m.pages[len(m.pages)-1] }

// open puts a page on the stack and lets it read its data.
func (m *model) open(p page) tea.Cmd {
	m.pages = append(m.pages, p)
	m.msg = ""
	if l, ok := p.(loader); ok {
		return l.load(m)
	}
	return nil
}

func (m *model) back() {
	if len(m.pages) > 1 {
		m.pages = m.pages[:len(m.pages)-1]
	}
	m.msg = ""
}

// Messages from the commands. gen ties an answer to its connection.
type (
	statusMsg struct {
		gen int
		st  protocol.Status
		err error
	}
	platformsMsg struct {
		gen, seq int
		p        []protocol.Tool
		err      error
	}
	infoMsg struct {
		gen  int
		info protocol.Info
	}
	pollMsg struct {
		gen int
		rev string
	}
	// platformsRetryMsg reads the platforms again after an error.
	platformsRetryMsg struct{ gen int }
	// actionMsg is the result of a change (an admin action, an invite).
	actionMsg struct {
		gen  int
		text string
		err  error
	}
)

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		// A key can open a page with turning glyphs, or close the help: the
		// frames start again (frame does nothing while they run).
		cmd := m.handleKey(msg)
		return m, tea.Batch(cmd, m.frame())
	case frameMsg:
		return m, m.onFrame()
	case carMsg:
		return m, m.onCar()
	case statusMsg:
		if msg.gen != m.gen {
			return m, nil
		}
		if msg.err != nil {
			if denied(msg.err) {
				m.checkOut("The server refused this computer's connection (removed, or the token revoked). Check in again with a new invite code.")
				return m, nil
			}
			m.err = msg.err.Error()
			gen := m.gen
			return m, tea.Tick(retryAfter, func(time.Time) tea.Msg { return pollMsg{gen: gen, rev: ""} })
		}
		// After an error (a server restart, for example after its update),
		// the server info again.
		reconnected := m.err != ""
		m.st, m.loaded, m.err = msg.st, true, ""
		if p := m.pop; p != nil && p.stale != nil && p.stale() {
			m.pop, m.msg = nil, "The queue changed: the question is cancelled. Look again, and try again."
		}
		now := time.Now()
		for _, ms := range msg.st.Machines {
			m.note("m:"+ms.Key, machineState(msg.st, ms, now).word)
		}
		cmds := []tea.Cmd{m.pollStatus(msg.st.Revision), m.readPlatforms(), m.frame()}
		// The server info, until a read of it works.
		if m.info.Version == "" || reconnected {
			cmds = append(cmds, m.readInfo())
		}
		if l, ok := m.top().(loader); ok {
			cmds = append(cmds, l.load(m))
		}
		return m, tea.Batch(cmds...)
	case pollMsg:
		if msg.gen != m.gen {
			return m, nil
		}
		return m, m.pollStatus(msg.rev)
	case platformsMsg:
		if msg.gen != m.gen || msg.seq != m.platformsSeq {
			return m, nil
		}
		m.platformsErr = ""
		if msg.err != nil {
			m.platformsErr = msg.err.Error()
			gen := m.gen
			return m, tea.Tick(retryAfter, func(time.Time) tea.Msg { return platformsRetryMsg{gen: gen} })
		}
		m.platforms, m.platformsRead = msg.p, true
		for _, pl := range m.platforms {
			m.note("p:"+pl.Name, fmt.Sprint(freeMachines(m, pl), pl.Waiting))
		}
		return m, m.frame()
	case platformsRetryMsg:
		if msg.gen != m.gen {
			return m, nil
		}
		return m, m.readPlatforms()
	case inviteMsg:
		// Handled here: the code is made once, also when the user left the
		// page.
		if msg.gen != m.gen {
			return m, nil
		}
		if msg.err != nil {
			m.msg = "Failed: " + msg.err.Error()
			return m, nil
		}
		m.keepText(msg.who + "\n    hostel connect " + msg.resp.Code)
		if g, ok := m.top().(*guestList); ok {
			g.code, g.codeFor, m.msg = msg.resp.Code, msg.who, ""
		} else {
			m.msg = "The invite code is ready; Hostel prints it when you quit."
		}
		return m, nil
	case machineInviteMsg:
		if msg.gen != m.gen {
			return m, nil
		}
		if msg.err == nil {
			r := msg.resp
			m.keepText(fmt.Sprintf("Run one of these on the machine %s (as an administrator):\n\nmacOS or Linux:\n  %s\n\nWindows (PowerShell):\n  %s", r.Machine, r.Unix, r.Windows))
		}
		if a, ok := m.top().(*addMachine); ok {
			a.done(msg)
		} else if msg.err != nil {
			m.msg = "Failed: " + msg.err.Error()
		} else {
			m.msg = "The machine code is ready; Hostel prints the commands when you quit."
		}
		return m, nil
	case infoMsg:
		if msg.gen == m.gen {
			m.info = msg.info
		}
		return m, nil
	case latestMsg:
		if msg.tag != "" {
			m.latest = msg.tag
		}
		return m, tea.Tick(latestEvery, func(time.Time) tea.Msg { return latestTick{} })
	case latestTick:
		return m, m.checkLatest()
	case selfUpdated:
		if msg.err != nil {
			m.msg = "Update failed: " + msg.err.Error()
			return m, nil
		}
		m.updated, m.msg = true, msg.text
		return m, nil
	case checkedOutMsg:
		// Handled here, not by Front desk: the user can leave the page while
		// the check out runs.
		if msg.err != nil {
			m.msg = "Check out failed: " + msg.err.Error()
			return m, nil
		}
		note := "Checked out from " + msg.url + "."
		if msg.warning != nil {
			note += " Warning: " + msg.warning.Error()
		}
		m.checkOut(note)
		return m, nil
	case actionMsg:
		if msg.gen != m.gen {
			return m, nil
		}
		m.msg = msg.text
		if msg.err != nil {
			m.msg = "Failed: " + msg.err.Error()
		}
		if l, ok := m.top().(loader); ok {
			return m, l.load(m)
		}
		return m, nil
	}
	// Other messages (for example the cursor blink of a text field) go to
	// the current page.
	if h, ok := m.top().(interface {
		other(m *model, msg tea.Msg) tea.Cmd
	}); ok {
		return m, h.other(m, msg)
	}
	return m, nil
}

func (m *model) handleKey(k tea.KeyMsg) tea.Cmd {
	if k.Type == tea.KeyCtrlC {
		return tea.Quit
	}
	p := m.top()
	typing := false
	if t, ok := p.(textPage); ok {
		typing = t.typing()
	}
	// Fast typing can bring several letters in one event: each is a key of
	// its own (a paste goes only into a text field, and never answers a
	// question).
	if k.Type == tea.KeyRunes && !typing && (len(k.Runes) > 1 || k.Paste) {
		if k.Paste {
			if m.pop != nil {
				m.pop, m.msg = nil, "Cancelled."
			}
			return nil
		}
		var cmds []tea.Cmd
		for _, r := range k.Runes {
			cmds = append(cmds, m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}))
		}
		return tea.Sequence(cmds...)
	}
	if m.pop != nil {
		return m.popupKey(k)
	}
	if typing {
		return p.key(m, k)
	}
	if m.help {
		if k.String() == "q" {
			return tea.Quit
		}
		m.help = k.String() != "?" && k.String() != "esc"
		return nil
	}
	// A message stays until the next key; then the footer describes the
	// selection again.
	m.msg = ""
	switch k.String() {
	case "q":
		return tea.Quit
	case "?":
		m.help = true
		return nil
	case "esc", "backspace", "left":
		if len(m.pages) > 1 {
			m.back()
			return nil
		}
	case "1", "2", "3":
		// Jump to a section of the main menu from any page.
		if _, ok := m.pages[0].(*mainMenu); ok {
			e := menuEntries(m)[k.String()[0]-'1']
			m.pages = m.pages[:1]
			return m.open(e.open())
		}
	}
	return p.key(m, k)
}

// pollStatus asks for the status; with rev, it waits for a change (long
// poll).
func (m *model) pollStatus(rev string) tea.Cmd {
	c, gen := m.c, m.gen
	return func() tea.Msg {
		path := "/v1/status"
		if rev != "" {
			path += "?since=" + url.QueryEscape(rev) + "&wait=" + statusWait.String()
		}
		ctx, cancel := context.WithTimeout(context.Background(), statusWait+15*time.Second)
		defer cancel()
		var st protocol.Status
		err := c.Do(ctx, "GET", path, nil, &st)
		return statusMsg{gen: gen, st: st, err: err}
	}
}

func (m *model) readPlatforms() tea.Cmd {
	m.platformsSeq++
	c, gen, seq := m.c, m.gen, m.platformsSeq
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		var list protocol.Tools
		err := c.Do(ctx, "GET", "/v1/tools", nil, &list)
		return platformsMsg{gen: gen, seq: seq, p: list.Tools, err: err}
	}
}

func (m *model) readInfo() tea.Cmd {
	c, gen := m.c, m.gen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		var info protocol.Info
		if err := c.Do(ctx, "GET", "/v1/info", nil, &info); err != nil {
			return nil
		}
		return infoMsg{gen: gen, info: info}
	}
}

// do runs a change in the background; done gives the message to show.
func (m *model) do(method, path string, body any, done string) tea.Cmd {
	c, gen := m.c, m.gen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		return actionMsg{gen: gen, text: done, err: c.Do(ctx, method, path, body, nil)}
	}
}

func (m *model) admin() bool { return m.st.Role == protocol.RoleAdmin }

// denied reports a refused token: asking again does not help.
func denied(err error) bool {
	var e *client.APIError
	return errors.As(err, &e) && (e.Status == 401 || e.Status == 403)
}

func (m *model) View() string {
	w := m.screenWidth()
	// The header bar: the sign, the place, and who you are.
	left := signStyle.Render("⌂ HOSTEL")
	if m.guest != nil {
		left += " " + boldStyle.Render(clean(m.guest.URL()))
	}
	for _, p := range m.pages {
		if t := p.title(); t != "" {
			left += mutedStyle.Render(" › ") + clean(t)
		}
	}
	right := ""
	if m.st.User != "" {
		role := pill(clean(m.st.Role), colMuted)
		if m.admin() {
			role = pill(clean(m.st.Role), colBlue)
		}
		right = mutedStyle.Render(clean(m.st.User)) + " " + role
	}
	// A long place gives way to the user and role.
	left = ansi.Truncate(left, max(w-lipgloss.Width(right)-1, 10), "…")
	gap := max(w-lipgloss.Width(left)-lipgloss.Width(right), 1)
	line := dimStyle.Render(strings.Repeat("─", w))
	head := left + strings.Repeat(" ", gap) + right + "\n" + line + "\n"

	body := m.top().body(m)
	if m.help {
		body = m.keyList()
	}

	// The footer: the question, the message, or the description of the
	// selected item; then the keys.
	var foot []string
	foot = append(foot, line)
	switch {
	case m.pop != nil:
		foot = append(foot, mutedStyle.Render("Choose with ↑↓ and enter, or press the letter of an option. Esc cancels."))
	case m.msg != "":
		foot = append(foot, clean(m.msg))
	default:
		// The description of the selection: up to 2 lines.
		hint := lipgloss.NewStyle().Width(w).Render(clean(m.top().hint(m)))
		if lines := strings.Split(hint, "\n"); len(lines) > 2 {
			hint = strings.Join(lines[:2], "\n")
		}
		for _, l := range strings.Split(hint, "\n") {
			foot = append(foot, mutedStyle.Render(strings.TrimRight(l, " ")))
		}
	}
	if m.err != "" {
		foot = append(foot, errStyle.Render("! "+clean(m.err)+" (trying again)"))
	}
	foot = append(foot, keyLine(m.footerKeys()))
	footText := strings.Join(foot, "\n")
	screen := fitScreen(body, footText, w, m.height-2)
	if m.pop != nil {
		lines := strings.Split(screen, "\n")
		nb := len(lines) - strings.Count(footText, "\n") - 1
		screen = strings.Join(append(m.pop.overlay(lines[:nb], w), lines[nb:]...), "\n")
	}
	return head + screen
}

// footerKeys are the page's keys and the keys of the app.
func (m *model) footerKeys() []binding {
	if m.pop != nil {
		return []binding{{"↑↓", "choose"}, {"enter", "select"}, {"esc", "cancel"}}
	}
	if m.help {
		return []binding{{"?", "close"}, {"esc", "close"}, {"q", "quit"}}
	}
	ks := m.top().keys(m)
	if t, ok := m.top().(textPage); ok && t.typing() {
		return append(ks, binding{"esc", "back"}, binding{"ctrl+c", "quit"})
	}
	if len(m.pages) > 1 {
		ks = append(ks, binding{"esc", "back"})
	}
	return append(ks, binding{"?", "keys"}, binding{"q", "quit"})
}

func keyLine(ks []binding) string {
	var out []string
	for _, k := range ks {
		out = append(out, keyStyle.Render(k.key)+" "+k.label)
	}
	return strings.Join(out, "  ")
}

// keyList is the "?" page: all keys of the current page and of the app.
func (m *model) keyList() string {
	help := m.help
	m.help = false
	keys := m.footerKeys()
	m.help = help
	if _, ok := m.pages[0].(*mainMenu); ok {
		keys = append(keys, binding{"1 2 3", "go to Front desk, Lobby, Rooms"})
	}
	keys = append(keys, binding{"ctrl+c", "quit at once"})
	var b strings.Builder
	b.WriteString("\n" + section("Keys", contentWidth(m), false))
	for _, k := range keys {
		b.WriteString(line(cell(accentStyle.Render(k.key), 10) + k.label))
	}
	return b.String() + "\n" + note("Press ? or esc to close.")
}

// plural is "1 room" or "2 rooms".
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
