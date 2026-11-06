package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/client"
	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
)

// statusTimeout bounds one status request (the details ask the room for the
// jobs, up to 10 s on the server).
const statusTimeout = 30 * time.Second

func apiClient() (*client.Client, error) {
	g, err := config.LoadGuest()
	if err != nil {
		return nil, err
	}
	return client.New(g)
}

// statusCmd is "hostel status [room | machine | tool]" (aliases ls, rooms): the
// overview, the details of a room or a machine, or the waiting leases that
// asked for a tool.
func statusCmd(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel status", stderr)
	fs.Bool("plain", false, "print plain text (the only output; people use the app: \"hostel\")")
	rest, err := cli.Parse(fs, args)
	if err != nil {
		return 2
	}
	if len(rest) > 1 {
		fmt.Fprintln(stderr, "hostel status: too many arguments; usage: hostel status [room | machine | tool]")
		return 2
	}
	c, err := apiClient()
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), statusTimeout)
	defer cancel()
	now := time.Now()
	var st protocol.Status
	if err := c.Do(ctx, "GET", "/v1/status", nil, &st); err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	cleanStatus(&st)
	if len(rest) == 1 {
		if slices.ContainsFunc(st.Rooms, func(r protocol.RoomStatus) bool { return r.Name == rest[0] }) {
			var d protocol.RoomDetails
			if err := c.Do(ctx, "GET", "/v1/rooms/"+rest[0], nil, &d); err != nil {
				fmt.Fprintln(stderr, "hostel:", err)
				return 1
			}
			cleanDetails(&d)
			printRoomDetails(stdout, d, now)
			return 0
		}
		if i := slices.IndexFunc(st.Machines, func(m protocol.MachineStatus) bool { return m.Key == rest[0] }); i >= 0 {
			printMachine(stdout, st, st.Machines[i])
			return 0
		}
		if err := printQueue(stdout, st, rest[0], now); err != nil {
			fmt.Fprintln(stderr, "hostel:", err)
			return 1
		}
		return 0
	}
	printStatus(stdout, st, now)
	return 0
}

// printStatus prints the overview for agents and scripts, with the next
// commands at the end.
func printStatus(w io.Writer, st protocol.Status, now time.Time) {
	inUse, waiting := 0, 0
	for _, r := range st.Rooms {
		if len(roomLeases(st, r.ID)) > 0 {
			inUse++
		}
	}
	for _, l := range st.Leases {
		if l.State == protocol.LeaseQueued {
			waiting++
		}
	}
	you := "-"
	if st.User != "" {
		you = fmt.Sprintf("%s (%s)", st.User, st.Role)
	}
	fmt.Fprintf(w, "%s · %s (%d in use) · %d waiting · you: %s\n\n", st.ServerID, plural(len(st.Rooms), "room"), inUse, waiting, you)
	if st.Warning != "" {
		fmt.Fprintf(w, "Warning: %s\n\n", st.Warning)
	}

	tw := newTable(w)
	fmt.Fprintln(tw, "ROOM\tTOOLS\tSTATE\tUSER\tTASK\tTIME\tLEASE")
	withRooms := map[string]bool{}
	for _, r := range st.Rooms {
		withRooms[r.Provider+"/"+r.Machine] = true
		leases := roomLeases(st, r.ID)
		state := roomState(r, len(leases))
		if len(leases) == 0 {
			fmt.Fprintf(tw, "%s\t%s\t%s\t\t\t\t\n", r.Name, inventoryText(r.Tools), state)
		}
		for i, l := range leases {
			name, tools := r.Name, inventoryText(r.Tools)
			if i > 0 {
				name, tools, state = "", "", ""
			}
			if l.State != protocol.LeaseReady && state != "" {
				state += " (" + l.State + ")"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", name, tools, state, l.User, l.Task, leaseAge(l, now), l.ID)
		}
	}
	for _, m := range st.Machines {
		if withRooms[m.Key] {
			continue
		}
		note := "(no room yet)"
		if m.Kind != "static" {
			note = fmt.Sprintf("(on demand, up to %d)", m.MaxRooms)
		}
		fmt.Fprintf(tw, "%s\t%s\tnone\t%s\t\t\t\n", m.Key, strings.Join(append([]string{m.OS}, m.Tools...), ", "), note)
	}
	tw.flush()

	if q := waitingLine(st, ""); len(q) > 0 {
		fmt.Fprintln(w)
		printWaiting(w, q, now)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Details: hostel status <room or machine> (its leases, jobs, events) or hostel status <tool> (the leases that wait for it).")
	fmt.Fprintln(w, "To get a room: hostel acquire [<tool>...] --task \"...\" (\"hostel tools\" lists the tools).")
}

// printMachine prints a machine that has no room of that name.
func printMachine(w io.Writer, st protocol.Status, m protocol.MachineStatus) {
	head := []string{m.Key}
	if m.Kind == "static" {
		head = append(head, "static")
	} else {
		head = append(head, "on demand, up to "+plural(m.MaxRooms, "room"))
	}
	head = append(head, m.OS)
	if size := m.Hardware.Size(); size != "" {
		head = append(head, size)
	}
	fmt.Fprintln(w, strings.Join(head, " · "))
	fmt.Fprintf(w, "Tools %s\n\n", strings.Join(m.Tools, ", "))
	tw := newTable(w)
	fmt.Fprintln(tw, "ROOM\tSTATE")
	n := 0
	for _, r := range st.Rooms {
		if r.Provider+"/"+r.Machine == m.Key {
			fmt.Fprintf(tw, "%s\t%s\n", r.Name, roomState(r, len(roomLeases(st, r.ID))))
			n++
		}
	}
	if n > 0 {
		tw.flush()
		return
	}
	if m.Kind == "static" {
		fmt.Fprintln(w, "No room now: the server has not prepared this machine yet.")
		return
	}
	fmt.Fprintln(w, "No room now: the next lease that needs this machine gets one.")
}

// printWaiting prints waiting leases in the order of the waiting line.
func printWaiting(w io.Writer, q []protocol.LeaseStatus, now time.Time) {
	tw := newTable(w)
	fmt.Fprintln(tw, "#\tUSER\tTASK\tTOOLS\tPRIORITY\tWAITING\tEST.\tLEASE\tREASON")
	for _, l := range q {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", position(l), l.User, l.Task, toolsText(l.Tools), l.Priority,
			shortDur(now.Sub(l.CreatedAt)), estimate(l), l.ID, l.Reason)
	}
	tw.flush()
}

func toolsText(tools []string) string {
	if len(tools) == 0 {
		return "(any)"
	}
	return strings.Join(tools, " ")
}

// inventoryText is a room's inventory in a few words: each tool with its
// options.
func inventoryText(tools []protocol.InventoryTool) string {
	var out []string
	for _, t := range tools {
		s := t.Name
		if len(t.Options) > 0 {
			s += " " + strings.Join(t.Options, "/")
		}
		if t.State != "ready" {
			s += " (" + t.State + ")"
		}
		out = append(out, s)
	}
	return strings.Join(out, ", ")
}

// asks reports whether a lease asked for a tool.
func asks(l protocol.LeaseStatus, tool string) bool {
	return slices.ContainsFunc(l.Tools, func(t string) bool { name, _, _ := strings.Cut(t, ":"); return name == tool })
}

// printQueue prints the waiting leases that asked for a tool.
func printQueue(w io.Writer, st protocol.Status, tool string, now time.Time) error {
	q := waitingLine(st, tool)
	used, free := 0, 0
	known := false
	for _, r := range st.Rooms {
		if !slices.ContainsFunc(r.Tools, func(t protocol.InventoryTool) bool { return t.Name == tool }) {
			continue
		}
		known = true
		if n := len(roomLeases(st, r.ID)); n > 0 {
			used++
		} else if r.State == protocol.RoomRunning && !r.Drained {
			free++
		}
	}
	for _, m := range st.Machines {
		known = known || slices.ContainsFunc(m.Tools, func(t string) bool { name, _, _ := strings.Cut(t, ":"); return name == tool })
	}
	if !known && len(q) == 0 {
		return fmt.Errorf("no room or tool %q; \"hostel status\" lists the rooms and the waiting line", tool)
	}
	fmt.Fprintf(w, "%s · %d in use, %d free · %d waiting\n", tool, used, free, len(q))
	if len(q) == 0 {
		return nil
	}
	fmt.Fprintln(w)
	printWaiting(w, q, now)
	return nil
}

// printRoomDetails prints the details of a room.
func printRoomDetails(w io.Writer, d protocol.RoomDetails, now time.Time) {
	r := d.Room
	kind := "on demand"
	if d.Machine != nil && d.Machine.Kind == "static" {
		kind = "static"
		if d.Machine.Connect {
			kind += ", connects to the server"
		}
	}
	osLine := r.OS
	if r.Hardware != nil && r.Hardware.OSVersion != "" {
		osLine = r.Hardware.OSVersion
	}
	if r.Arch != "" {
		osLine += ", " + r.Arch
	}
	fmt.Fprintf(w, "%s · room %d · %s · %s · %s\n", r.Name, r.ID, kind, osLine, roomState(r, len(d.Leases)))
	if hw := r.Hardware; hw != nil {
		var parts []string
		if hw.CPU != "" {
			parts = append(parts, hw.CPU)
		}
		if hw.Cores > 0 {
			parts = append(parts, plural(hw.Cores, "core"))
		}
		if hw.MemoryBytes > 0 {
			parts = append(parts, gb(hw.MemoryBytes))
		}
		if hw.DiskFreeBytes > 0 {
			parts = append(parts, "disk "+gb(hw.DiskFreeBytes)+" free")
		}
		fmt.Fprintln(w, strings.Join(parts, " · "))
	}
	line := "Tools " + inventoryText(r.Tools)
	fmt.Fprintf(w, "%s · %s\n", line, plural(len(r.Slots), "slot"))
	if r.Reason != "" {
		fmt.Fprintf(w, "Reason: %s\n", r.Reason)
	}

	fmt.Fprintln(w)
	if len(d.Leases) == 0 {
		fmt.Fprintln(w, "NOW      free")
	}
	for i, l := range d.Leases {
		label := "NOW"
		if i > 0 {
			label = ""
		}
		fmt.Fprintf(w, "%-8s %s · %s · lease %s (%s, slot %s)\n", label, l.User, l.Task, l.ID, l.State, l.Slot)
		var times []string
		if !l.ReadyAt.IsZero() {
			times = append(times, "ready "+shortDur(now.Sub(l.ReadyAt))+" ago")
		} else {
			times = append(times, "since "+shortDur(now.Sub(l.CreatedAt)))
		}
		if !l.ExpiresAt.IsZero() {
			times = append(times, "ends at "+l.ExpiresAt.Local().Format("15:04")+" at the latest")
		}
		fmt.Fprintf(w, "         %s\n", strings.Join(times, " · "))
		from := fmt.Sprintf("from %s · repository %s", l.Guest, l.Repo)
		if l.Branch != "" {
			from += " · branch " + l.Branch
		}
		fmt.Fprintf(w, "         %s\n", from)
		for _, j := range d.Jobs[l.ID] {
			state := j.State
			if j.ExitCode != nil {
				state = fmt.Sprintf("%s %d", state, *j.ExitCode)
			}
			fmt.Fprintf(w, "         job %s %s %s: %s\n", j.ID, state, shortDur(now.Sub(j.StartedAt)), strings.Join(j.Command, " "))
		}
	}
	if d.JobsError != "" {
		fmt.Fprintf(w, "         %s\n", d.JobsError)
	}

	if len(d.Queue) > 0 {
		fmt.Fprintln(w)
		for i, l := range d.Queue {
			label := "WAITING"
			if i > 0 {
				label = ""
			}
			fmt.Fprintf(w, "%-8s %s. %s · %s · %s · %s · waiting %s · %s · lease %s\n", label, position(l), l.User, l.Task,
				toolsText(l.Tools), l.Priority, shortDur(now.Sub(l.CreatedAt)), estimate(l), l.ID)
		}
	}

	if len(d.Events) > 0 {
		fmt.Fprintln(w)
		for i, e := range d.Events {
			label := "EVENTS"
			if i > 0 {
				label = ""
			}
			msg := e.Message
			if msg == "" {
				msg = e.Kind
			}
			fmt.Fprintf(w, "%-8s %s %s\n", label, e.Time.Local().Format("15:04"), msg)
		}
	}
}

// waitingLine returns the waiting leases (that asked for tool, if not "")
// in the order of the one waiting line.
func waitingLine(st protocol.Status, tool string) []protocol.LeaseStatus {
	var out []protocol.LeaseStatus
	for _, l := range st.Leases {
		if l.State == protocol.LeaseQueued && (tool == "" || asks(l, tool)) {
			out = append(out, l)
		}
	}
	slices.SortStableFunc(out, byPosition)
	return out
}

// roomLeases returns the leases on a room that are not queued.
func roomLeases(st protocol.Status, roomID int64) []protocol.LeaseStatus {
	var out []protocol.LeaseStatus
	for _, l := range st.Leases {
		if l.RoomID == roomID && l.State != protocol.LeaseQueued {
			out = append(out, l)
		}
	}
	return out
}

// byPosition orders queued leases by position; a lease that the scheduler
// has not placed yet (position 0) comes last.
func byPosition(a, b protocol.LeaseStatus) int {
	if (a.Position == 0) != (b.Position == 0) {
		if a.Position == 0 {
			return 1
		}
		return -1
	}
	return a.Position - b.Position
}

func position(l protocol.LeaseStatus) string {
	if l.Position == 0 {
		return "-"
	}
	return strconv.Itoa(l.Position)
}

func roomState(r protocol.RoomStatus, leases int) string {
	state := r.State
	if r.State == protocol.RoomBroken && r.Reason == "unreachable" {
		state = "offline"
		if !r.LastSeen.IsZero() {
			state += " (last seen " + shortDur(time.Since(r.LastSeen)) + " ago)"
		}
	}
	if r.State == protocol.RoomRunning {
		state = "free"
		if leases > 0 {
			state = "in use"
			if len(r.Slots) > 1 {
				// An exclusive lease holds all slots: count the slots.
				used := 0
				for _, sl := range r.Slots {
					if sl.LeaseID != "" {
						used++
					}
				}
				state = fmt.Sprintf("in use %d/%d", max(used, leases), len(r.Slots))
			}
		}
	}
	if r.Drained {
		state += ", drained"
	}
	return state
}

func leaseAge(l protocol.LeaseStatus, now time.Time) string {
	if !l.ReadyAt.IsZero() {
		return shortDur(now.Sub(l.ReadyAt))
	}
	return shortDur(now.Sub(l.CreatedAt))
}

func estimate(l protocol.LeaseStatus) string {
	if l.EstimatedWaitSeconds <= 0 {
		return "-"
	}
	return "~" + shortDur(time.Duration(l.EstimatedWaitSeconds)*time.Second)
}

// shortDur prints a duration as 41s, 12m, or 2h05m.
func shortDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(int(d.Seconds()), 0))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func gb(n int64) string { return fmt.Sprintf("%.0f GB", float64(n)/(1<<30)) }

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// table aligns columns like tabwriter and drops the spaces at line ends.
type table struct {
	*tabwriter.Writer
	buf bytes.Buffer
	w   io.Writer
}

func newTable(w io.Writer) *table {
	t := &table{w: w}
	t.Writer = tabwriter.NewWriter(&t.buf, 0, 4, 2, ' ', 0)
	return t
}

func (t *table) flush() error {
	if err := t.Writer.Flush(); err != nil {
		return err
	}
	for line := range strings.Lines(t.buf.String()) {
		if _, err := io.WriteString(t.w, strings.TrimRight(line, " \n")+"\n"); err != nil {
			return err
		}
	}
	return nil
}

// clean replaces control characters (also newlines and tabs) with spaces:
// tasks, job commands, and messages come from users, and must not add lines
// or columns, or send VT sequences to the terminal.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return ' '
		}
		return r
	}, s)
}

func cleanLease(l *protocol.LeaseStatus) {
	for _, f := range []*string{&l.Task, &l.User, &l.Guest, &l.Repo, &l.Branch, &l.Reason} {
		*f = clean(*f)
	}
}

func cleanRoom(r *protocol.RoomStatus) {
	r.Reason = clean(r.Reason)
	if hw := r.Hardware; hw != nil {
		hw.CPU, hw.OSVersion = clean(hw.CPU), clean(hw.OSVersion)
	}
}

// cleanMachine cleans the text from the configuration (an admin can write
// any string in a tool entry).
func cleanMachine(m *protocol.MachineStatus) {
	m.Key, m.Kind, m.OS = clean(m.Key), clean(m.Kind), clean(m.OS)
	for i := range m.Tools {
		m.Tools[i] = clean(m.Tools[i])
	}
}

// cleanStatus cleans the text from users in a status.
func cleanStatus(st *protocol.Status) {
	st.User = clean(st.User)
	for i := range st.Machines {
		cleanMachine(&st.Machines[i])
	}
	for i := range st.Leases {
		cleanLease(&st.Leases[i])
	}
	for i := range st.Rooms {
		cleanRoom(&st.Rooms[i])
	}
}

// cleanDetails cleans the text from users in room details.
func cleanDetails(d *protocol.RoomDetails) {
	cleanRoom(&d.Room)
	if d.Machine != nil {
		cleanMachine(d.Machine)
	}
	for _, ls := range [][]protocol.LeaseStatus{d.Leases, d.Queue} {
		for i := range ls {
			cleanLease(&ls[i])
		}
	}
	for _, jobs := range d.Jobs {
		for i := range jobs {
			for j := range jobs[i].Command {
				jobs[i].Command[j] = clean(jobs[i].Command[j])
			}
		}
	}
	for i := range d.Events {
		d.Events[i].Message = clean(d.Events[i].Message)
	}
	d.JobsError = clean(d.JobsError)
}
