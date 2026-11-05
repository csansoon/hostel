package app

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/csansoon/hostel/internal/protocol"
)

// machineRooms returns the rooms of a machine (an on-demand machine can have
// several).
func machineRooms(st protocol.Status, key string) []protocol.RoomStatus {
	var out []protocol.RoomStatus
	for _, r := range st.Rooms {
		if r.Provider+"/"+r.Machine == key {
			out = append(out, r)
		}
	}
	return out
}

// roomLeases returns the leases on a room that are not queued.
func roomLeases(st protocol.Status, id int64) []protocol.LeaseStatus {
	var out []protocol.LeaseStatus
	for _, l := range st.Leases {
		if l.RoomID == id && l.State != protocol.LeaseQueued {
			out = append(out, l)
		}
	}
	return out
}

// freeSlot reports a running room that takes a new lease for the tool now.
func freeSlot(r protocol.RoomStatus, tool string) bool {
	if r.State != protocol.RoomRunning || r.Drained || (tool != "" && !roomHas(r, tool)) {
		return false
	}
	return slices.ContainsFunc(r.Slots, func(s protocol.SlotStatus) bool { return s.State == protocol.SlotFree })
}

// roomHas reports whether a room's inventory has a tool.
func roomHas(r protocol.RoomStatus, tool string) bool {
	return slices.ContainsFunc(r.Tools, func(t protocol.InventoryTool) bool { return t.Name == tool })
}

// machineFree reports whether a machine can take a lease for the tool now:
// a free slot, or room for a new on-demand room.
func machineFree(st protocol.Status, m protocol.MachineStatus, tool string) bool {
	rooms := machineRooms(st, m.Key)
	if slices.ContainsFunc(rooms, func(r protocol.RoomStatus) bool { return freeSlot(r, tool) }) {
		return true
	}
	return m.Kind != "static" && len(rooms) < m.MaxRooms
}

// leaseLine is a lease in plain words: Ana · "iOS snapshots" · 12m.
func leaseLine(l protocol.LeaseStatus, now time.Time) string {
	s := fmt.Sprintf("%s · %q · %s", clean(l.User), clean(l.Task), ago(now.Sub(leaseSince(l))))
	if l.State != protocol.LeaseReady {
		s += " (" + plainLeaseState(l.State) + ")"
	}
	return s
}

// leaseSince is the check in time of a lease: ready, else created.
func leaseSince(l protocol.LeaseStatus) time.Time {
	if !l.ReadyAt.IsZero() {
		return l.ReadyAt
	}
	return l.CreatedAt
}

func plainLeaseState(state string) string {
	switch state {
	case protocol.LeaseAssigned, protocol.LeaseProvisioning, protocol.LeasePreparing:
		return "getting ready"
	case protocol.LeaseCleaning:
		return "cleaning up"
	}
	return state
}

// state is a machine state: a themed word with its color, and the detail in
// plain words.
type state struct {
	word   string
	color  lipgloss.Color
	detail string
}

// dot is the state's icon in its color: ● vacant (green), ◉ occupied
// (blue), ◐ getting ready (it turns), ◌ no new leases, ✕ broken, ○ offline or not
// connected.
func (s state) dot() string {
	mark := "●"
	switch s.word {
	case "Occupied":
		mark = "◉"
	case "Getting ready":
		mark = spin("◐", "◓", "◑", "◒")
	case "No new leases":
		mark = "◌"
	case "Broken":
		mark = "✕"
	case "Offline", "Not connected", "Not ready":
		mark = "○"
	}
	return fg(s.color).Render(mark)
}

// rank orders the states in lists: the machines that can work first, the
// offline ones last.
func (s state) rank() int {
	switch s.word {
	case "Vacant":
		return 0
	case "Occupied":
		return 1
	case "Getting ready":
		return 2
	case "No new leases":
		return 3
	case "Broken":
		return 4
	case "Offline":
		return 6
	case "Not connected", "Not ready":
		return 7
	}
	return 5
}

// byState returns the machines in state order (stable: the configuration
// order inside a state).
func byState(st protocol.Status, machines []protocol.MachineStatus, now time.Time) []protocol.MachineStatus {
	out := slices.Clone(machines)
	slices.SortStableFunc(out, func(a, b protocol.MachineStatus) int {
		return machineState(st, a, now).rank() - machineState(st, b, now).rank()
	})
	return out
}

// offlineRoom returns the room of a static machine that does not answer
// (broken, reason "unreachable"; the server checks it again).
func offlineRoom(rooms []protocol.RoomStatus) (protocol.RoomStatus, bool) {
	for _, r := range rooms {
		if r.State == protocol.RoomBroken && r.Reason == "unreachable" {
			return r, true
		}
	}
	return protocol.RoomStatus{}, false
}

// online reports whether a machine can take leases at all: an on-demand
// machine, or a static machine with a room that is not offline (a static
// machine without a room has not connected or is not prepared yet).
func online(st protocol.Status, m protocol.MachineStatus) bool {
	if m.Kind != "static" {
		return true
	}
	rooms := machineRooms(st, m.Key)
	_, off := offlineRoom(rooms)
	return len(rooms) > 0 && !off
}

// machineState is the state of a machine: Vacant (a new lease can start on
// it now; the same rule as the free machines of the Lobby), Occupied, No new
// leases, Getting ready, Broken, …
func machineState(st protocol.Status, m protocol.MachineStatus, now time.Time) state {
	rooms := machineRooms(st, m.Key)
	var leases []protocol.LeaseStatus
	for _, r := range rooms {
		leases = append(leases, roomLeases(st, r.ID)...)
	}
	guests := func() string {
		s := leaseLine(leases[0], now)
		if len(leases) > 1 {
			s += fmt.Sprintf(" (+%d)", len(leases)-1)
		}
		return s
	}
	if m.Kind == "static" && len(rooms) == 0 {
		if m.Connect {
			return state{"Not connected", colMuted, "the machine has not connected to the server yet"}
		}
		return state{"Not ready", colMuted, "the server has not prepared the machine yet"}
	}
	if r, ok := offlineRoom(rooms); ok && len(leases) == 0 {
		seen := "not seen yet"
		if !r.LastSeen.IsZero() {
			seen = "last seen " + ago(now.Sub(r.LastSeen)) + " ago"
		}
		return state{"Offline", colMuted, seen + " · it comes back by itself when the machine answers"}
	}
	var s state
	switch {
	case machineFree(st, m, ""):
		s = state{"Vacant", colGreen, "ready for a new lease"}
		if len(leases) > 0 {
			s.detail = guests() + " · room for one more"
		}
	case len(leases) > 0:
		s = state{"Occupied", colBlue, guests()}
	case slices.ContainsFunc(rooms, func(r protocol.RoomStatus) bool { return r.State == protocol.RoomBroken }):
		i := slices.IndexFunc(rooms, func(r protocol.RoomStatus) bool { return r.State == protocol.RoomBroken })
		s = state{"Broken", colRed, clean(rooms[i].Reason)}
	case slices.ContainsFunc(rooms, func(r protocol.RoomStatus) bool { return r.Drained }):
		s = state{"No new leases", colMuted, "an admin stopped new leases on this machine"}
	case slices.ContainsFunc(rooms, func(r protocol.RoomStatus) bool {
		return r.State == protocol.RoomProvisioning || r.State == protocol.RoomCreating
	}):
		s = state{"Getting ready", colYellow, "the server prepares the machine"}
	default:
		s = state{strings.ToUpper(rooms[0].State[:1]) + rooms[0].State[1:], colMuted, ""}
	}
	if m.Kind != "static" {
		s.detail = strings.TrimPrefix(s.detail+fmt.Sprintf(" · on demand, %d of %d rooms", len(rooms), m.MaxRooms), " · ")
	}
	if s.word == "Occupied" && slices.ContainsFunc(rooms, func(r protocol.RoomStatus) bool { return r.Drained }) {
		s.detail += " · no new leases"
	}
	return s
}

// ago prints a duration as 41s, 12m, or 2h05m.
func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(int(d.Seconds()), 0))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// word is the state's word in its color; a state that just changed glows
// (the word on its color) for a moment.
func (m *model) word(key string, s state) string {
	if m.glowing(key) {
		return lipgloss.NewStyle().Background(s.color).Foreground(colInk).Render(s.word)
	}
	return fg(s.color).Render(s.word)
}
