package app

import (
	"hash/fnv"
	"math/rand/v2"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// The street on the main menu is an easter egg (owner decision, 2026-11-05):
// the hostel between two buildings, a lamp, a tree, and cars that pass. The
// share of lit hostel windows follows the share of occupied machines, so it
// works for one machine and for fifty; the app does not explain it.
//
// In the picture: w is a hostel window, n a window of a neighbour, L the
// lamp, T the tree, t its trunk. The last line is the road.
var streetArt = []string{
	"            ┌───HOSTEL───┐           ",
	"  ╭L  ┌───┐ │ w  w  w  w │ ┌──┐      ",
	"  │   │n n│ │ w  w  w  w │ │nn│  TTT ",
	"  │   │n n│ │ w  ┌──┐  w │ │nn│   t  ",
	"──┴───┴───┴─┴────┘  └────┴─┴──┴───┴──",
}

const (
	streetWidth  = 37
	hostelWins   = 10
	neighbourWin = 8
	// carSpeed is in cells per second. The time before each car is random,
	// up to carMaxGap: a car is a rare, strange thing (owner decision,
	// 2026-11-05).
	carSpeed  = 20.0
	carMaxGap = 6 * time.Minute
)

var (
	colLight  = lipgloss.Color("229") // a lit window, the lamp
	colSoft   = lipgloss.Color("186") // a lit window of a neighbour
	colWindow = lipgloss.Color("237") // a dark window
	carShapes = []string{"▗▅▅▖", "▄▆▄"}
	carColors = []lipgloss.Color{"174", "109", "250", "180"}
)

type car struct {
	shape string
	color lipgloss.Color
	start time.Time
	// from is the first cell at start; dir is 1 (to the right) or -1.
	from float64
	dir  float64
}

func (c car) x(now time.Time) int {
	return int(c.from + c.dir*carSpeed*now.Sub(c.start).Seconds())
}

func (c car) gone(now time.Time) bool {
	x := c.x(now)
	return x > streetWidth || x < -len([]rune(c.shape))
}

type street struct {
	cars []car
	// lit are the windows of the neighbours that are lit (decoration).
	lit []bool
}

// carMsg brings the next car.
type carMsg struct{}

func nextCar() tea.Cmd {
	d := time.Duration(rand.Int64N(int64(carMaxGap)))
	return tea.Tick(d, func(time.Time) tea.Msg { return carMsg{} })
}

// onCar adds a car when the street is on the screen, and sometimes turns a
// neighbour's light on or off.
func (m *model) onCar() tea.Cmd {
	if animOff() {
		return nil
	}
	if _, ok := m.top().(*mainMenu); ok && len(m.street.cars) == 0 {
		c := car{shape: carShapes[rand.IntN(len(carShapes))], color: carColors[rand.IntN(len(carColors))], start: time.Now(), dir: 1}
		c.from = -float64(len([]rune(c.shape)))
		if rand.IntN(2) == 0 {
			c.from, c.dir = streetWidth, -1
		}
		m.street.cars = append(m.street.cars, c)
		if len(m.street.lit) > 0 && rand.IntN(3) == 0 {
			i := rand.IntN(len(m.street.lit))
			m.street.lit[i] = !m.street.lit[i]
		}
	}
	return tea.Batch(nextCar(), m.frame())
}

// dropCars forgets the cars that left the street.
func (s *street) dropCars(now time.Time) {
	var keep []car
	for _, c := range s.cars {
		if !c.gone(now) {
			keep = append(keep, c)
		}
	}
	s.cars = keep
}

// windowState is what a hostel window shows.
type windowState int

const (
	windowDark   windowState = iota // a vacant machine
	windowLit                       // an occupied machine
	windowReady                     // a machine that gets ready (it flickers)
	windowClosed                    // an offline or broken machine
)

// windows returns the state of each hostel window for the machines'
// states. Which window lights is a fixed order for each server, so that a
// change lights or darkens one window at a time.
func windows(serverID string, words []string) []windowState {
	out := make([]windowState, hostelWins)
	if len(words) == 0 {
		return out
	}
	n := map[windowState]int{}
	for _, w := range words {
		switch w {
		case "Occupied":
			n[windowLit]++
		case "Getting ready":
			n[windowReady]++
		case "Vacant":
		default:
			n[windowClosed]++
		}
	}
	// share is the number of windows for count machines: at least one when
	// count > 0, and all of them when every machine has that state.
	share := func(count int) int {
		if count == 0 {
			return 0
		}
		return max(1, (count*hostelWins+len(words)/2)/len(words))
	}
	order := seeded(serverID).Perm(hostelWins)
	// Lit and flickering windows from the start of the order, closed ones
	// from the end: a change of one kind does not move the others.
	i := 0
	for _, k := range []windowState{windowLit, windowReady} {
		for c := share(n[k]); c > 0 && i < hostelWins; c-- {
			out[order[i]] = k
			i++
		}
	}
	for c, j := share(n[windowClosed]), hostelWins-1; c > 0 && j >= i; c, j = c-1, j-1 {
		out[order[j]] = windowClosed
	}
	return out
}

func seeded(serverID string) *rand.Rand {
	h := fnv.New64a()
	h.Write([]byte(serverID))
	return rand.New(rand.NewPCG(h.Sum64(), 1))
}

func (w windowState) draw() string {
	switch w {
	case windowLit:
		return fg(colLight).Render("■")
	case windowReady:
		return fg(lipgloss.Color(spin("229", "237"))).Render("■")
	case windowClosed:
		return dimStyle.Render("▒")
	}
	return fg(colWindow).Render("■")
}

// draw returns the street, with the left margin.
func (s *street) draw(m *model) string {
	now := time.Now()
	var words []string
	for _, ms := range m.st.Machines {
		words = append(words, machineState(m.st, ms, now).word)
	}
	ws := windows(m.st.ServerID, words)
	if s.lit == nil {
		r := seeded(m.st.ServerID + "/street")
		s.lit = make([]bool, neighbourWin)
		for i := range s.lit {
			s.lit[i] = r.IntN(3) == 0
		}
	}
	var b strings.Builder
	hw, nw := 0, 0
	for _, line := range streetArt {
		b.WriteString(margin + " ")
		rest := line
		for len(rest) > 0 {
			if strings.HasPrefix(rest, "HOSTEL") {
				b.WriteString(accentStyle.Bold(true).Render("HOSTEL"))
				rest = rest[len("HOSTEL"):]
				continue
			}
			if strings.HasPrefix(rest, "TTT") {
				b.WriteString(fg(lipgloss.Color("65")).Render("▟█▙"))
				rest = rest[3:]
				continue
			}
			r := []rune(rest)[0]
			rest = rest[len(string(r)):]
			switch r {
			case 'w':
				b.WriteString(ws[hw].draw())
				hw++
			case 'n':
				c := colWindow
				if s.lit[nw] {
					c = colSoft
				}
				b.WriteString(fg(c).Render("■"))
				nw++
			case 'L':
				b.WriteString(fg(colLight).Render("●"))
			case 't':
				b.WriteString(fg(lipgloss.Color("94")).Render("│"))
			case ' ':
				b.WriteString(" ")
			default:
				b.WriteString(dimStyle.Render(string(r)))
			}
		}
		b.WriteString("\n")
	}
	b.WriteString(margin + " " + s.road(now))
	return b.String()
}

// road is the last line: the cars at their place now.
func (s *street) road(now time.Time) string {
	cells := make([]string, streetWidth)
	for i := range cells {
		cells[i] = " "
	}
	for _, c := range s.cars {
		x := c.x(now)
		for i, r := range []rune(c.shape) {
			if p := x + i; p >= 0 && p < streetWidth {
				cells[p] = fg(c.color).Render(string(r))
			}
		}
	}
	return strings.Join(cells, "")
}
