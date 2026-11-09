package app

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/csansoon/hostel/internal/protocol"
)

// templateMachines returns the machines of a machine (an on-demand machine can have
// several).
func templateMachines(st protocol.Status, key string) []protocol.MachineStatus {
	var out []protocol.MachineStatus
	for _, r := range st.Machines {
		if r.Provider+"/"+r.Template == key {
			out = append(out, r)
		}
	}
	return out
}

// machineLeases returns the leases on a machine that are not queued.
func machineLeases(st protocol.Status, id int64) []protocol.LeaseStatus {
	var out []protocol.LeaseStatus
	for _, l := range st.Leases {
		if l.MachineID == id && l.State != protocol.LeaseQueued {
			out = append(out, l)
		}
	}
	return out
}

// freeRoom reports a running machine that takes a new lease for the tool now.
func freeRoom(r protocol.MachineStatus, tool string) bool {
	if r.State != protocol.MachineRunning || r.Drained || (tool != "" && !machineHas(r, tool)) {
		return false
	}
	return slices.ContainsFunc(r.Rooms, func(s protocol.RoomStatus) bool { return s.State == protocol.RoomFree })
}

// machineHas reports whether a machine's inventory has a tool.
func machineHas(r protocol.MachineStatus, tool string) bool {
	return slices.ContainsFunc(r.Tools, func(t protocol.InventoryTool) bool { return t.Name == tool })
}

// templateFree reports whether a machine can take a lease for the tool now:
// a free room, or machine for a new on-demand machine.
func templateFree(st protocol.Status, m protocol.TemplateStatus, tool string) bool {
	machines := templateMachines(st, m.Key)
	if slices.ContainsFunc(machines, func(r protocol.MachineStatus) bool { return freeRoom(r, tool) }) {
		return true
	}
	return m.Kind != "static" && len(machines) < m.MaxMachines
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
func byState(st protocol.Status, templates []protocol.TemplateStatus, now time.Time) []protocol.TemplateStatus {
	out := slices.Clone(templates)
	slices.SortStableFunc(out, func(a, b protocol.TemplateStatus) int {
		return templateState(st, a, now).rank() - templateState(st, b, now).rank()
	})
	return out
}

// offlineMachine returns the machine of a static machine that does not answer
// (broken, reason "unreachable"; the server checks it again).
func offlineMachine(machines []protocol.MachineStatus) (protocol.MachineStatus, bool) {
	for _, r := range machines {
		if r.State == protocol.MachineBroken && r.Reason == "unreachable" {
			return r, true
		}
	}
	return protocol.MachineStatus{}, false
}

// online reports whether a machine can take leases at all: an on-demand
// machine, or a static machine with a machine that is not offline (a static
// machine without a machine has not connected or is not prepared yet).
func online(st protocol.Status, m protocol.TemplateStatus) bool {
	if m.Kind != "static" {
		return true
	}
	machines := templateMachines(st, m.Key)
	_, off := offlineMachine(machines)
	return len(machines) > 0 && !off
}

// templateState is the state of a machine: Vacant (a new lease can start on
// it now; the same rule as the free machines of the Lobby), Occupied, No new
// leases, Getting ready, Broken, …
func templateState(st protocol.Status, m protocol.TemplateStatus, now time.Time) state {
	machines := templateMachines(st, m.Key)
	var leases []protocol.LeaseStatus
	for _, r := range machines {
		leases = append(leases, machineLeases(st, r.ID)...)
	}
	guests := func() string {
		s := leaseLine(leases[0], now)
		if len(leases) > 1 {
			s += fmt.Sprintf(" (+%d)", len(leases)-1)
		}
		return s
	}
	if m.Kind == "static" && len(machines) == 0 {
		if m.Connect {
			return state{"Not connected", colMuted, "the machine has not connected to the server yet"}
		}
		return state{"Not ready", colMuted, "the server has not prepared the machine yet"}
	}
	if r, ok := offlineMachine(machines); ok && len(leases) == 0 {
		seen := "not seen yet"
		if !r.LastSeen.IsZero() {
			seen = "last seen " + ago(now.Sub(r.LastSeen)) + " ago"
		}
		return state{"Offline", colMuted, seen + " · it comes back by itself when the machine answers"}
	}
	var s state
	switch {
	case templateFree(st, m, ""):
		s = state{"Vacant", colGreen, "ready for a new lease"}
		if len(leases) > 0 {
			s.detail = guests() + " · machine for one more"
		}
	case len(leases) > 0:
		s = state{"Occupied", colBlue, guests()}
	case slices.ContainsFunc(machines, func(r protocol.MachineStatus) bool { return r.State == protocol.MachineBroken }):
		i := slices.IndexFunc(machines, func(r protocol.MachineStatus) bool { return r.State == protocol.MachineBroken })
		s = state{"Broken", colRed, clean(machines[i].Reason)}
	case slices.ContainsFunc(machines, func(r protocol.MachineStatus) bool { return r.Drained }):
		s = state{"No new leases", colMuted, "an admin stopped new leases on this machine"}
	case slices.ContainsFunc(machines, func(r protocol.MachineStatus) bool {
		return r.State == protocol.MachineProvisioning || r.State == protocol.MachineCreating
	}):
		s = state{"Getting ready", colYellow, "the server prepares the machine"}
	default:
		s = state{strings.ToUpper(machines[0].State[:1]) + machines[0].State[1:], colMuted, ""}
	}
	if m.Kind != "static" {
		s.detail = strings.TrimPrefix(s.detail+fmt.Sprintf(" · on demand, %d of %d machines", len(machines), m.MaxMachines), " · ")
	}
	if s.word == "Occupied" && slices.ContainsFunc(machines, func(r protocol.MachineStatus) bool { return r.Drained }) {
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
