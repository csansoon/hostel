package app

import (
	"os"
	"slices"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/csansoon/hostel/internal/protocol"
)

// The motion of the app: a changed state glows for a moment, states that
// wait turn, and cars pass on the main menu's street. Pages do not move (owner decision, 2026-11-05).
// HOSTEL_NO_ANIMATION=1 turns the motion off.
const (
	glowTime = 1200 * time.Millisecond
	// frameGap is the step of the turning glyphs (spin).
	frameGap = 150 * time.Millisecond
	// carFrame is the frame gap while a car drives: smoother.
	carFrame = 50 * time.Millisecond
)

type animState struct {
	// ticking: a frame message is on its way.
	ticking bool
	// glow is when each key changed; prev is its last value.
	glow map[string]time.Time
	prev map[string]string
}

type frameMsg struct{}

func animOff() bool { return os.Getenv("HOSTEL_NO_ANIMATION") == "1" }

// frame asks for the next frame while something moves.
func (m *model) frame() tea.Cmd {
	if animOff() || m.anim.ticking || !m.moving() {
		return nil
	}
	m.anim.ticking = true
	d := frameGap
	if len(m.street.cars) > 0 {
		d = carFrame
	}
	return tea.Tick(d, func(time.Time) tea.Msg { return frameMsg{} })
}

func (m *model) onFrame() tea.Cmd {
	m.anim.ticking = false
	m.street.dropCars(time.Now())
	return m.frame()
}

// moving reports a car, a glow, or a state whose glyph turns on the current
// page (a lease that waits or gets ready, a room that starts).
func (m *model) moving() bool {
	if len(m.street.cars) > 0 {
		return true
	}
	for _, t := range m.anim.glow {
		if time.Since(t) < glowTime {
			return true
		}
	}
	if m.help {
		return false
	}
	// Each page checks only the states whose glyphs turn on it.
	switch p := m.top().(type) {
	case *mainMenu:
		return m.queued("") || m.starting("")
	case *lobby:
		return m.queued("")
	case *toolPage:
		t, _ := p.tool(m)
		return m.queued(p.name) || slices.ContainsFunc(t.Machines, func(tm protocol.ToolMachine) bool { return m.starting(tm.Key) })
	case *rooms, *roomAdmin:
		return m.starting("")
	case *machinePage:
		var leases []protocol.LeaseStatus
		for _, r := range machineRooms(m.st, p.machineKey) {
			leases = append(leases, roomLeases(m.st, r.ID)...)
		}
		return m.starting(p.machineKey) || slices.ContainsFunc(leases, func(l protocol.LeaseStatus) bool { return l.State != protocol.LeaseReady })
	}
	return false
}

// queued reports a queued lease for a tool ("" for any).
func (m *model) queued(tool string) bool {
	return slices.ContainsFunc(m.st.Leases, func(l protocol.LeaseStatus) bool {
		return l.State == protocol.LeaseQueued && (tool == "" || asks(l, tool))
	})
}

// starting reports a room that starts on the machine key ("" for any): the
// machine is Getting ready.
func (m *model) starting(key string) bool {
	return slices.ContainsFunc(m.st.Rooms, func(r protocol.RoomStatus) bool {
		return (r.State == protocol.RoomProvisioning || r.State == protocol.RoomCreating) && (key == "" || r.Provider+"/"+r.Machine == key)
	})
}

// note keeps a value; a change (not the first value) makes the key glow.
func (m *model) note(key, value string) {
	if m.anim.prev == nil {
		m.anim.prev, m.anim.glow = map[string]string{}, map[string]time.Time{}
	}
	if old, ok := m.anim.prev[key]; ok && old != value {
		m.anim.glow[key] = time.Now()
	}
	m.anim.prev[key] = value
}

func (m *model) glowing(key string) bool {
	t, ok := m.anim.glow[key]
	return ok && !animOff() && time.Since(t) < glowTime
}

// spin is the current frame of a turning glyph.
func spin(frames ...string) string {
	if animOff() {
		return frames[0]
	}
	return frames[int(time.Now().UnixMilli()/150)%len(frames)]
}
