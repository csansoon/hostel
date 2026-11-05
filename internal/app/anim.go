package app

import (
	"os"
	"slices"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/csansoon/hostel/internal/protocol"
)

// The motion of the app: a changed state glows for a moment, and states that
// wait turn. Pages do not move (owner decision, 2026-11-05).
// HOSTEL_NO_ANIMATION=1 turns the motion off.
const (
	glowTime = 1200 * time.Millisecond
	frameGap = 100 * time.Millisecond
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
	return tea.Tick(frameGap, func(time.Time) tea.Msg { return frameMsg{} })
}

func (m *model) onFrame() tea.Cmd {
	m.anim.ticking = false
	return m.frame()
}

// moving reports a glow, or a state that turns (a lease that gets ready or
// waits, a room that starts).
func (m *model) moving() bool {
	for _, t := range m.anim.glow {
		if time.Since(t) < glowTime {
			return true
		}
	}
	if slices.ContainsFunc(m.st.Leases, func(l protocol.LeaseStatus) bool { return l.State != protocol.LeaseReady }) {
		return true
	}
	return slices.ContainsFunc(m.st.Rooms, func(r protocol.RoomStatus) bool {
		return r.State == protocol.RoomProvisioning || r.State == protocol.RoomCreating
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
