package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"

	"golang.org/x/term"

	"github.com/csansoon/hostel/internal/client"
	"github.com/csansoon/hostel/internal/protocol"
)

const (
	// statusWait is the long poll of the view (the server allows up to
	// 50 s); statusPollTimeout bounds one long-poll request.
	statusWait        = 45 * time.Second
	statusPollTimeout = statusWait + 15*time.Second
	// statusRetry is the pause after a failed request.
	statusRetry = 5 * time.Second
	// escWait is how long a lone ESC waits for the rest of an escape
	// sequence before it is the Esc key.
	escWait = 50 * time.Millisecond
	// detailsInterval is the shortest time between two reads of the room
	// details.
	detailsInterval = 10 * time.Second
)

// Key names of the view.
const (
	keyUp    = "up"
	keyDown  = "down"
	keyEnter = "enter"
	keyEsc   = "esc"
	keyQuit  = "quit"
	// keyShiftUp and keyShiftDown move a queued lease (also K and J).
	keyShiftUp   = "shift-up"
	keyShiftDown = "shift-down"
)

// view is the interactive status (multiplayer.md, section 8): the same lines
// as the plain output, drawn inline under the prompt and redrawn when the
// status changes. It does not use the alternate screen, so the last frame
// stays in the terminal.
type view struct {
	c   *client.Client
	out *os.File

	st     protocol.Status
	loaded bool
	err    string
	sel    int
	// open is the item whose details show (nil: the overview).
	open    *statusItem
	details *protocol.RoomDetails
	// detailsErr is the error of the last details request.
	detailsErr string
	loading    bool
	// detailsAt is the start of the last details request; detailsStale
	// says that the status changed after it.
	detailsAt    time.Time
	detailsStale bool

	// leaseSel is the selected lease in the details (actions).
	leaseSel string
	// prompt asks to confirm an action; msg is the result of the last one.
	prompt  *prompt
	msg     string
	actions chan string

	// lineCols are the column counts of the lines of the last frame.
	lineCols []int
}

type statusResult struct {
	st  protocol.Status
	err error
}

type detailsResult struct {
	name string
	d    protocol.RoomDetails
	err  error
}

// viewSupported reports whether the view can run: stdin and stdout are
// terminals that take VT sequences.
func viewSupported(stdout any) (*os.File, bool) {
	out, ok := stdout.(*os.File)
	if !ok || !term.IsTerminal(int(out.Fd())) || !term.IsTerminal(int(os.Stdin.Fd())) || os.Getenv("TERM") == "dumb" {
		return nil, false
	}
	return out, enableVT(out) == nil
}

func runView(c *client.Client, out *os.File) error {
	old, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("set the terminal to raw mode: %w", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), old)
	fmt.Fprint(out, "\x1b[?25l")
	defer fmt.Fprint(out, "\x1b[?25h")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	statuses := make(chan statusResult)
	go pollStatus(ctx, c, statuses)
	keys := make(chan string)
	go readKeys(os.Stdin, keys)
	detailsCh := make(chan detailsResult)

	v := &view{c: c, out: out, actions: make(chan string)}
	v.draw()
	// The tick reads stale details again and updates the times.
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			v.refreshDetails(ctx, detailsCh)
		case r := <-statuses:
			if r.err != nil {
				if denied(r.err) {
					v.draw()
					fmt.Fprint(out, "\r\n")
					return r.err
				}
				v.err = r.err.Error()
				break
			}
			v.setStatus(r.st)
			if v.prompt != nil && v.prompt.queue != nil && !slices.Equal(v.prompt.queue, leaseIDs(v.detailLeases())) {
				v.prompt, v.msg = nil, "The queue changed: the move is cancelled."
			}
			// The jobs are not in the status: the details are read again
			// after a status answer (a change, or the end of the wait).
			v.detailsStale = true
			v.refreshDetails(ctx, detailsCh)
		case r := <-detailsCh:
			v.loading = false
			if v.open == nil || !v.open.room {
				break
			}
			if v.open.name != r.name {
				// The answer for a room that is closed now.
				v.loadDetails(ctx, detailsCh)
				break
			}
			if r.err != nil {
				v.detailsErr = r.err.Error()
				break
			}
			v.details, v.detailsErr = &r.d, ""
		case msg := <-v.actions:
			v.msg = msg
			v.detailsStale, v.detailsAt = true, time.Time{}
			v.refreshDetails(ctx, detailsCh)
		case k, ok := <-keys:
			if !ok || k == keyQuit {
				v.draw()
				fmt.Fprint(out, "\r\n")
				return nil
			}
			v.key(ctx, k, detailsCh)
		}
		v.draw()
	}
}

func (v *view) key(ctx context.Context, k string, detailsCh chan detailsResult) {
	v.msg = ""
	if p := v.prompt; p != nil {
		v.prompt = nil
		if run, ok := p.choices[k]; ok {
			v.run(ctx, run)
		}
		return
	}
	items := statusItems(v.st)
	switch {
	case v.open != nil && k == keyEsc:
		v.open, v.details, v.detailsErr, v.leaseSel = nil, nil, "", ""
	case v.open != nil:
		v.detailsKey(k)
	case k == keyUp:
		v.sel = max(v.sel-1, 0)
	case k == keyDown:
		v.sel = min(v.sel+1, max(len(items)-1, 0))
	case k == keyEnter && v.sel < len(items):
		it := items[v.sel]
		v.open = &it
		if it.room && !v.loading {
			v.loadDetails(ctx, detailsCh)
		}
	}
}

// setStatus takes a new status and keeps the selected item, if it is
// still there.
func (v *view) setStatus(st protocol.Status) {
	items := statusItems(v.st)
	var cur *statusItem
	if v.loaded && v.sel < len(items) {
		cur = &items[v.sel]
	}
	v.st, v.loaded, v.err = st, true, ""
	items = statusItems(st)
	if cur != nil {
		if i := slices.Index(items, *cur); i >= 0 {
			v.sel = i
			return
		}
	}
	v.sel = min(v.sel, max(len(items)-1, 0))
}

// denied reports a refused token (revoked, or the user removed): asking again
// does not help.
func denied(err error) bool {
	var e *client.APIError
	return errors.As(err, &e) && (e.Status == 401 || e.Status == 403)
}

// refreshDetails reads stale details again, at most every detailsInterval
// (each read opens SSH to the room).
func (v *view) refreshDetails(ctx context.Context, ch chan detailsResult) {
	if v.detailsStale && v.open != nil && v.open.room && !v.loading && time.Since(v.detailsAt) >= detailsInterval {
		v.loadDetails(ctx, ch)
	}
}

// loadDetails asks for the details of the open room in the background.
func (v *view) loadDetails(ctx context.Context, ch chan detailsResult) {
	name := v.open.name
	v.loading, v.detailsStale = true, false
	v.detailsAt = time.Now()
	go func() {
		rctx, cancel := context.WithTimeout(ctx, statusTimeout)
		defer cancel()
		var d protocol.RoomDetails
		err := v.c.Do(rctx, "GET", "/v1/rooms/"+name, nil, &d)
		cleanDetails(&d)
		select {
		case ch <- detailsResult{name: name, d: d, err: err}:
		case <-ctx.Done():
		}
	}()
}

// frame returns the lines of the current screen: the body (it can scroll)
// and the tail (keys, prompt, messages; always shown).
func (v *view) frame() (body, tail []string) {
	var b, t bytes.Buffer
	now := time.Now()
	switch {
	case !v.loaded:
		fmt.Fprintln(&b, "Reading the status …")
	case v.open == nil:
		printStatus(&b, v.st, now, v.sel)
		fmt.Fprintln(&t, "↑↓ select · enter details · q quit")
	case v.open.room:
		switch {
		case v.details != nil:
			printRoomDetails(&b, *v.details, now, v.selectedLease().ID)
		case v.detailsErr == "":
			fmt.Fprintf(&b, "%s · reading the details …\n", v.open.name)
		}
		if v.detailsErr != "" {
			fmt.Fprintf(&b, "\nCannot read the details: %s\n", v.detailsErr)
		}
		fmt.Fprintln(&t, v.detailsFooter())
	default:
		if err := printQueue(&b, v.st, v.open.name, now, v.selectedLease().ID); err != nil {
			fmt.Fprintf(&b, "%s · the queue is empty\n", v.open.name)
		}
		fmt.Fprintln(&t, v.detailsFooter())
	}
	switch {
	case v.prompt != nil:
		fmt.Fprintln(&t, v.prompt.text)
	case v.msg != "":
		fmt.Fprintln(&t, v.msg)
	}
	if v.err != "" {
		fmt.Fprintf(&t, "! %s (trying again)\n", v.err)
	}
	split := func(s string) []string {
		if s = strings.TrimRight(s, "\n"); s == "" {
			return nil
		}
		return strings.Split(s, "\n")
	}
	body, tail = split(b.String()), split(t.String())
	if len(tail) > 0 {
		body = append(body, "")
	}
	return body, tail
}

// fitLines makes the frame fit in height lines: the tail always shows; the
// body shows a window around the selected line (the line with "❯"), with "…"
// for the cut parts.
func fitLines(body, tail []string, height int) []string {
	room := height - len(tail)
	if room < 1 {
		return tail[len(tail)-height:]
	}
	if len(body) <= room {
		return append(body, tail...)
	}
	sel := slices.IndexFunc(body, func(l string) bool { return strings.Contains(l, "❯") })
	start := 0
	if sel >= 0 {
		start = min(max(sel-room/2, 0), len(body)-room)
	}
	win := slices.Clone(body[start : start+room])
	if start > 0 && sel != start {
		win[0] = "…"
	}
	if start+room < len(body) && sel != start+room-1 {
		win[room-1] = "…"
	}
	return append(win, tail...)
}

// draw replaces the last frame with the current one. Lines are cut to the
// terminal width and the frame to its height, so the cursor moves stay
// correct.
func (v *view) draw() {
	width, height, err := term.GetSize(int(v.out.Fd()))
	if err != nil || width < 10 || height < 3 {
		width, height = 80, 24
	}
	body, tail := v.frame()
	lines := fitLines(body, tail, height-1)
	for i, l := range lines {
		lines[i] = cutColumns(l, width-1)
	}
	var b strings.Builder
	b.WriteString("\r")
	// After the terminal got narrower, the lines of the last frame wrap: count
	// the rows that they take now.
	if rows := frameRows(v.lineCols, width); rows > 1 {
		fmt.Fprintf(&b, "\x1b[%dA", rows-1)
	}
	b.WriteString("\x1b[J")
	b.WriteString(strings.Join(lines, "\r\n"))
	v.out.WriteString(b.String())
	v.lineCols = v.lineCols[:0]
	for _, l := range lines {
		v.lineCols = append(v.lineCols, lineColumns(l))
	}
}

// pollStatus sends the status, then each change (long poll). After an error
// it waits statusRetry and asks again.
func pollStatus(ctx context.Context, c *client.Client, out chan<- statusResult) {
	rev := ""
	for ctx.Err() == nil {
		path := "/v1/status"
		if rev != "" {
			path += "?since=" + url.QueryEscape(rev) + "&wait=" + statusWait.String()
		}
		rctx, cancel := context.WithTimeout(ctx, statusPollTimeout)
		var st protocol.Status
		err := c.Do(rctx, "GET", path, nil, &st)
		cleanStatus(&st)
		cancel()
		if ctx.Err() != nil {
			return
		}
		select {
		case out <- statusResult{st: st, err: err}:
		case <-ctx.Done():
			return
		}
		if denied(err) {
			return
		}
		if err != nil {
			select {
			case <-time.After(statusRetry):
			case <-ctx.Done():
				return
			}
			continue
		}
		rev = st.Revision
	}
}

// readKeys reads the terminal input and sends the keys of the view. It ends
// (and closes keys) when the input ends.
func readKeys(in *os.File, keys chan<- string) {
	defer close(keys)
	chunks := make(chan string)
	go func() {
		defer close(chunks)
		buf := make([]byte, 64)
		for {
			n, err := in.Read(buf)
			if err != nil {
				return
			}
			chunks <- string(buf[:n])
		}
	}()
	rest := ""
	var lone <-chan time.Time
	for {
		select {
		case c, ok := <-chunks:
			if !ok {
				return
			}
			var ks []string
			ks, rest = parseKeys(rest + c)
			for _, k := range ks {
				keys <- k
			}
			lone = nil
			if rest == "\x1b" {
				lone = time.After(escWait)
			}
		case <-lone:
			// No more bytes after ESC: the Esc key.
			rest, lone = "", nil
			keys <- keyEsc
		}
	}
}

// parseKeys splits terminal input into keys. ESC with more bytes is an escape
// sequence (ESC ESC: the Esc key). An escape sequence without its final byte,
// or a lone ESC at the end, is returned as rest: the next read can complete
// it (readKeys makes a lone ESC the Esc key after escWait).
func parseKeys(s string) ([]string, string) {
	var out []string
	for s != "" {
		if incompleteEscape(s) {
			return out, s
		}
		switch {
		case strings.HasPrefix(s, "\x1b[1;2A"):
			out, s = append(out, keyShiftUp), s[6:]
		case strings.HasPrefix(s, "\x1b[1;2B"):
			out, s = append(out, keyShiftDown), s[6:]
		case strings.HasPrefix(s, "\x1b[A"), strings.HasPrefix(s, "\x1bOA"):
			out, s = append(out, keyUp), s[3:]
		case strings.HasPrefix(s, "\x1b[B"), strings.HasPrefix(s, "\x1bOB"):
			out, s = append(out, keyDown), s[3:]
		case s == "\x1b":
			return out, s
		case strings.HasPrefix(s, "\x1b\x1b"):
			out, s = append(out, keyEsc), s[1:]
		case s[0] == '\x1b':
			// Another escape sequence: skip it up to its final byte.
			i := 1
			if i < len(s) && (s[i] == '[' || s[i] == 'O') {
				i++
				for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
					i++
				}
			}
			s = s[min(i+1, len(s)):]
		case s[0] == '\r' || s[0] == '\n':
			out, s = append(out, keyEnter), s[1:]
		case s[0] == 'q' || s[0] == 'Q' || s[0] == 0x03:
			out, s = append(out, keyQuit), s[1:]
		case s[0] > 0x20 && s[0] < 0x7f:
			out, s = append(out, s[:1]), s[1:]
		default:
			s = s[1:]
		}
	}
	return out, ""
}

// incompleteEscape reports an escape sequence that has not got its final byte
// yet: ESC [ or ESC O, then only parameter bytes.
func incompleteEscape(s string) bool {
	if len(s) < 2 || s[0] != '\x1b' || (s[1] != '[' && s[1] != 'O') {
		return false
	}
	for i := 2; i < len(s); i++ {
		if s[i] >= 0x40 && s[i] <= 0x7e {
			return false
		}
	}
	return true
}

// cutColumns cuts a line to n terminal columns (with "…" at the end when it
// cuts), so that no line wraps and the cursor moves of draw stay correct.
func cutColumns(s string, n int) string {
	type cell struct{ at, cols int }
	var cells []cell
	total := 0
	for i, r := range s {
		w := runeColumns(r)
		// VS16 shows the character before it as an emoji (2 columns).
		if r == 0xfe0f && len(cells) > 0 && cells[len(cells)-1].cols == 1 {
			cells[len(cells)-1].cols, total = 2, total+1
		}
		cells = append(cells, cell{i, w})
		total += w
	}
	if total <= n {
		return s
	}
	cols := 0
	for _, c := range cells {
		if cols+c.cols > n-1 {
			return s[:c.at] + "…"
		}
		cols += c.cols
	}
	return s
}

// lineColumns is the width of a line in terminal columns.
func lineColumns(s string) int {
	n, prev := 0, 0
	for _, r := range s {
		w := runeColumns(r)
		if r == 0xfe0f && prev == 1 {
			n++
		}
		n, prev = n+w, w
	}
	return n
}

// frameRows is the number of terminal rows that lines of these widths take at
// this terminal width (a line wider than the terminal wraps).
func frameRows(lineCols []int, width int) int {
	rows := 0
	for _, c := range lineCols {
		rows += max(1, (c+width-1)/width)
	}
	return rows
}

// runeColumns is the width of a rune in a terminal: 0 for combining marks, 2
// for East Asian wide characters and emoji (also the symbols that show as
// emoji by default), else 1.
func runeColumns(r rune) int {
	switch {
	case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf):
		return 0
	case r >= 0x1100 && r <= 0x115f, r >= 0x2e80 && r <= 0xa4cf && r != 0x303f, r >= 0xac00 && r <= 0xd7a3,
		r >= 0xf900 && r <= 0xfaff, r >= 0xfe30 && r <= 0xfe4f, r >= 0xff00 && r <= 0xff60, r >= 0xffe0 && r <= 0xffe6,
		r >= 0x1f000 && r <= 0x1faff, r >= 0x20000 && r <= 0x3fffd, emojiPresentation(r):
		return 2
	}
	return 1
}

// emojiPresentation: symbols below U+1F000 that terminals show as emoji
// without VS16 (Unicode Emoji_Presentation).
func emojiPresentation(r rune) bool {
	for _, rg := range [][2]rune{{0x231a, 0x231b}, {0x23e9, 0x23ec}, {0x23f0, 0x23f0}, {0x23f3, 0x23f3}, {0x25fd, 0x25fe},
		{0x2614, 0x2615}, {0x2648, 0x2653}, {0x267f, 0x267f}, {0x2693, 0x2693}, {0x26a1, 0x26a1}, {0x26aa, 0x26ab},
		{0x26bd, 0x26be}, {0x26c4, 0x26c5}, {0x26ce, 0x26ce}, {0x26d4, 0x26d4}, {0x26ea, 0x26ea}, {0x26f2, 0x26f3},
		{0x26f5, 0x26f5}, {0x26fa, 0x26fa}, {0x26fd, 0x26fd}, {0x2705, 0x2705}, {0x270a, 0x270b}, {0x2728, 0x2728},
		{0x274c, 0x274c}, {0x274e, 0x274e}, {0x2753, 0x2755}, {0x2757, 0x2757}, {0x2795, 0x2797}, {0x27b0, 0x27b0},
		{0x27bf, 0x27bf}, {0x2b1b, 0x2b1c}, {0x2b50, 0x2b50}, {0x2b55, 0x2b55}} {
		if r >= rg[0] && r <= rg[1] {
			return true
		}
	}
	return false
}
