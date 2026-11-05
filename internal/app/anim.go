package app

import (
	"os"
	"slices"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/csansoon/hostel/internal/protocol"
)

// The motion of the app: pages slide in and out, a changed state glows for a
// moment, and states that wait turn. HOSTEL_NO_ANIMATION=1 turns it off.
const (
	slideTime = 200 * time.Millisecond
	glowTime  = 1200 * time.Millisecond
	slowFrame = 100 * time.Millisecond
	fastFrame = 16 * time.Millisecond
)

type animState struct {
	slide *slideAnim
	// last is the body of the last frame: the page that slides out.
	last []string
	// ticking: a frame message is on its way.
	ticking bool
	// glow is when each key changed; prev is its last value.
	glow map[string]time.Time
	prev map[string]string
}

type slideAnim struct {
	from  []string
	dir   int // 1: the new page comes from the right; -1: from the left
	start time.Time
}

type frameMsg struct{}

func animOff() bool { return os.Getenv("HOSTEL_NO_ANIMATION") == "1" }

// frame asks for the next frame while something moves.
func (m *model) frame() tea.Cmd {
	if animOff() || m.anim.ticking || !m.moving() {
		return nil
	}
	m.anim.ticking = true
	d := slowFrame
	if m.anim.slide != nil {
		d = fastFrame
	}
	return tea.Tick(d, func(time.Time) tea.Msg { return frameMsg{} })
}

func (m *model) onFrame() tea.Cmd {
	m.anim.ticking = false
	if s := m.anim.slide; s != nil && time.Since(s.start) >= slideTime {
		m.anim.slide = nil
	}
	return m.frame()
}

// moving reports a slide, a glow, or a state that turns (a lease that gets
// ready or waits, a room that starts).
func (m *model) moving() bool {
	if m.anim.slide != nil {
		return true
	}
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

func (m *model) startSlide(dir int) tea.Cmd {
	if animOff() || m.anim.last == nil {
		return nil
	}
	m.anim.slide = &slideAnim{from: m.anim.last, dir: dir, start: time.Now()}
	return m.frame()
}

// slid draws the body during a slide: the old page leaves on one side while
// the new one comes in.
func (m *model) slid(lines []string, w int) []string {
	s := m.anim.slide
	t := min(float64(time.Since(s.start))/float64(slideTime), 1)
	p := 1 - (1-t)*(1-t)*(1-t)
	x := int(float64(w)*(1-p) + 0.5)
	out := make([]string, len(lines))
	for i, nl := range lines {
		ol := ""
		if i < len(s.from) {
			ol = s.from[i]
		}
		nl, ol = cell(nl, w), cell(ol, w)
		if s.dir > 0 {
			out[i] = cell(ansi.Cut(ol, w-x, w), x) + "\x1b[0m" + ansi.Cut(nl, 0, w-x)
		} else {
			out[i] = cell(ansi.Cut(nl, x, w), w-x) + "\x1b[0m" + ansi.Cut(ol, 0, x)
		}
	}
	return out
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
