// Package scheduler orders the queue and selects a slot or a provider action for
// each queued lease (design, section 5). It is a pure function: it reads its
// input and does no I/O. Workers do the actions after the pass.
package scheduler

import (
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
)

// lifetimeMargin is the extra room lifetime that a lease needs after max_lease.
const lifetimeMargin = 5 * time.Minute

// Want is one requested tool: "name" or "name:option".
type Want struct {
	Name   string
	Option string
}

// Request is what a lease asks for.
type Request struct {
	// Tools are the requested tools; none: any machine.
	Tools []Want
	// PoolTunnels is the number of tunnels without a fixed room port.
	PoolTunnels int
	Fresh       bool
	// Exclusive leases take all slots of a room.
	Exclusive bool
}

// Have is what a machine or a room gives of one tool.
type Have struct {
	Screen bool
	// Options are the versions of a software tool, or the options of a
	// screen tool; empty when they are not known.
	Options []string
	// Checked: Options come from the inventory (what check reported), not
	// from the configuration.
	Checked bool
}

// Matches reports whether the tool satisfies a requested option (design,
// section 3). A screen option matches exactly. A version matches the whole
// version or its first dot-separated parts; a configured version (before the
// inventory) also matches a more exact request ("22" can install 22.11).
func (h Have) Matches(option string) bool {
	if option == "" || len(h.Options) == 0 {
		return true
	}
	if h.Screen {
		return slices.Contains(h.Options, option)
	}
	for _, have := range h.Options {
		if VersionMatches(option, have) || (!h.Checked && (have == "latest" || VersionMatches(have, option))) {
			return true
		}
	}
	return false
}

// VersionMatches reports whether want is the whole version have or its first
// dot-separated parts: "22" matches "22.11.0", not "2.2".
func VersionMatches(want, have string) bool {
	return have == want || strings.HasPrefix(have, want+".")
}

// gives reports whether tools satisfy every tool of the request.
func gives(tools map[string]Have, r Request) bool {
	for _, w := range r.Tools {
		h, ok := tools[w.Name]
		if !ok || !h.Matches(w.Option) {
			return false
		}
	}
	return true
}

// Machine is one machine of the configuration.
type Machine struct {
	Key       string // "<provider>/<machine>"
	Provider  string
	OnDemand  bool
	CanResume bool
	// Tools are the machine's tools: the inventories of its rooms where
	// they have one, else the configuration.
	Tools map[string]Have
	// PoolPorts is the smallest number of pool ports of its slots.
	PoolPorts int
	// Slots are the slot names of a new room.
	Slots       []string
	MaxRooms    int
	MaxLease    time.Duration
	MaxLifetime time.Duration // 0: no limit
}

// Provider has the limits of a provider.
type Provider struct {
	Name     string
	MaxRooms int // 0: no limit
	Limits   []Limit
}

// Limit counts the rooms of a subset of the machines of a provider.
type Limit struct {
	Machines []string // machine keys
	MaxRooms int
}

// Room is a room that is not destroyed.
type Room struct {
	ID        int64
	Machine   string
	State     string
	Drained   bool
	CreatedAt time.Time
	// Tools are what the room gives: its inventory, else its machine's
	// configuration.
	Tools map[string]Have
	// Ready lists the tools that the room has installed and checked with
	// the current scripts, with their needs.
	Ready map[string]bool
	Slots []Slot
}

type Slot struct {
	Name      string
	State     string
	PoolPorts int
}

// Lease is a queued lease.
type Lease struct {
	ID string
	Request
	Priority int
	// ManualPosition > 0: an admin placed the lease; it goes before the
	// others, in this order.
	ManualPosition int
	CreatedAt      time.Time
}

type Input struct {
	Now       time.Time
	Machines  []Machine // configuration order
	Providers []Provider
	Rooms     []Room
	Leases    []Lease
	// Full has the machines with a provider "limit reached" flag.
	Full map[string]bool
}

// Action is what a worker does for an assignment.
type Action string

const (
	// UseRoom: a free slot in a running room that is ready for the lease.
	UseRoom Action = "use"
	// ProvisionRoom: a free slot in a running room; the install scripts of
	// the lease's tools run first.
	ProvisionRoom Action = "provision"
	// ResumeRoom: a stopped room.
	ResumeRoom Action = "resume"
	// CreateRoom: a new room of Machine; RoomID is 0.
	CreateRoom Action = "create"
)

type Assignment struct {
	LeaseID string
	Action  Action
	Machine string
	RoomID  int64
	// Slots are the reserved slots; the first is the lease's slot.
	Slots []string
}

// Reasons why a lease waits (R7).
const (
	ReasonNoSlot       = "no free slot"
	ReasonEarlierLease = "an earlier lease waits for the same machines"
	ReasonRoomStarting = "a room is starting"
	ReasonLimit        = "room limit reached"
	ReasonProviderFull = "provider limit reached"
	ReasonEviction     = "an idle room is being removed to make space"
)

type Waiting struct {
	LeaseID string
	// Position is 1 for the first lease of the waiting line.
	Position int
	// Behind counts the waiting leases before it that share one of its
	// candidate machines.
	Behind int
	Reason string
}

type Output struct {
	Assignments []Assignment
	// Evict lists idle rooms to destroy, so that a waiting lease can get a new room.
	Evict   []int64
	Waiting []Waiting
}

// Order sorts queued leases into the one waiting line (design, section 5):
// leases that an admin placed first (by their manual position), then the
// priority lane (high first), then the request time; there is no aging.
func Order(leases []Lease) {
	sort.SliceStable(leases, func(i, j int) bool {
		a, b := leases[i], leases[j]
		if (a.ManualPosition > 0) != (b.ManualPosition > 0) {
			return a.ManualPosition > 0
		}
		if a.ManualPosition != b.ManualPosition {
			return a.ManualPosition < b.ManualPosition
		}
		return before(a, b)
	})
}

// before is the order without placements: lane, then request time.
func before(a, b Lease) bool {
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.ID < b.ID
}

// Serves reports whether the machine can ever satisfy the request.
func (m *Machine) Serves(r Request) bool {
	return gives(m.Tools, r) && m.PoolPorts >= r.PoolTunnels && (!r.Fresh || m.OnDemand)
}

// ready reports whether the room has installed and checked each requested
// tool.
func (r *room) ready(req Request) bool {
	for _, w := range req.Tools {
		if !r.Ready[w.Name] {
			return false
		}
	}
	return true
}

// Candidates returns the keys of the machines that can satisfy the request, in
// configuration order.
func Candidates(machines []Machine, r Request) []string {
	var keys []string
	for i := range machines {
		if machines[i].Serves(r) {
			keys = append(keys, machines[i].Key)
		}
	}
	return keys
}

type pass struct {
	in        Input
	machines  map[string]*Machine
	providers map[string]*Provider
	rooms     []*room
	blocked   map[string]bool
}

type room struct {
	Room
	machine *Machine
	evicted bool
	busy    map[string]bool // slots taken in this pass
}

// Schedule runs one scheduler pass.
func Schedule(in Input) Output {
	p := &pass{in: in, machines: map[string]*Machine{}, providers: map[string]*Provider{}, blocked: map[string]bool{}}
	for i := range in.Machines {
		p.machines[in.Machines[i].Key] = &in.Machines[i]
	}
	for i := range in.Providers {
		p.providers[in.Providers[i].Name] = &in.Providers[i]
	}
	for _, r := range in.Rooms {
		m := p.machines[r.Machine]
		if m == nil {
			continue // a room of a machine that left the configuration
		}
		p.rooms = append(p.rooms, &room{Room: r, machine: m, busy: map[string]bool{}})
	}
	sort.SliceStable(p.rooms, func(i, j int) bool { return p.rooms[i].ID < p.rooms[j].ID })

	leases := slices.Clone(in.Leases)
	Order(leases)

	var out Output
	var waiting []waitingLease
	for _, l := range leases {
		all := Candidates(in.Machines, l.Request)
		var free []*Machine
		for _, k := range all {
			if !p.blocked[k] {
				free = append(free, p.machines[k])
			}
		}
		if a, ok := p.assign(l, free); ok {
			out.Assignments = append(out.Assignments, a)
			continue
		}
		reason := p.reason(free, len(free) < len(all))
		if evict, ok := p.evict(free); ok {
			out.Evict = append(out.Evict, evict...)
			reason = ReasonEviction
		}
		for _, k := range all {
			p.blocked[k] = true
		}
		behind := 0
		for _, w := range waiting {
			if slices.ContainsFunc(all, func(k string) bool { return slices.Contains(w.machines, k) }) {
				behind++
			}
		}
		waiting = append(waiting, waitingLease{all})
		out.Waiting = append(out.Waiting, Waiting{LeaseID: l.ID, Position: len(waiting), Behind: behind, Reason: reason})
	}
	return out
}

type waitingLease struct{ machines []string }

func (p *pass) assign(l Lease, machines []*Machine) (Assignment, bool) {
	if !l.Fresh {
		for _, ready := range []bool{true, false} {
			for _, m := range machines {
				for _, r := range p.roomsOf(m.Key) {
					if r.evicted || r.State != protocol.RoomRunning || r.Drained || !gives(r.Tools, l.Request) || r.ready(l.Request) != ready || !p.lifetimeOK(r) {
						continue
					}
					if slots := r.take(l.Request); slots != nil {
						action := UseRoom
						if !ready {
							action = ProvisionRoom
						}
						return Assignment{LeaseID: l.ID, Action: action, Machine: m.Key, RoomID: r.ID, Slots: slots}, true
					}
				}
			}
		}
		for _, m := range machines {
			if !m.CanResume {
				continue
			}
			for _, r := range p.roomsOf(m.Key) {
				if r.evicted || r.State != protocol.RoomStopped || r.Drained || !gives(r.Tools, l.Request) || !p.lifetimeOK(r) {
					continue
				}
				if slots := r.take(l.Request); slots != nil {
					return Assignment{LeaseID: l.ID, Action: ResumeRoom, Machine: m.Key, RoomID: r.ID, Slots: slots}, true
				}
			}
		}
	}
	for _, m := range machines {
		if !m.OnDemand || p.in.Full[m.Key] || len(p.blockingLimits(m)) > 0 {
			continue
		}
		slots := m.Slots[:1]
		if l.Exclusive {
			slots = slices.Clone(m.Slots)
		}
		// The new room counts against the limits for the rest of the pass.
		nr := &room{Room: Room{State: protocol.RoomCreating, Machine: m.Key, CreatedAt: p.in.Now}, machine: m, busy: map[string]bool{}}
		p.rooms = append(p.rooms, nr)
		return Assignment{LeaseID: l.ID, Action: CreateRoom, Machine: m.Key, Slots: slots}, true
	}
	return Assignment{}, false
}

// take reserves the slots for a lease, or returns nil.
func (r *room) take(req Request) []string {
	var free []string
	for _, s := range r.Slots {
		if s.State == protocol.SlotFree && !r.busy[s.Name] {
			free = append(free, s.Name)
		}
	}
	if req.Exclusive {
		if len(free) != len(r.Slots) || len(free) == 0 || r.slotPorts(free[0]) < req.PoolTunnels {
			return nil
		}
	} else {
		i := slices.IndexFunc(free, func(n string) bool { return r.slotPorts(n) >= req.PoolTunnels })
		if i < 0 {
			return nil
		}
		free = free[i : i+1]
	}
	for _, n := range free {
		r.busy[n] = true
	}
	return free
}

func (r *room) slotPorts(name string) int {
	for _, s := range r.Slots {
		if s.Name == name {
			return s.PoolPorts
		}
	}
	return 0
}

// idle reports whether the room has no lease: running with all slots free, or stopped.
func (r *room) idle() bool {
	if r.evicted {
		return false
	}
	switch r.State {
	case protocol.RoomStopped:
		return true
	case protocol.RoomRunning:
		for _, s := range r.Slots {
			if s.State != protocol.SlotFree || r.busy[s.Name] {
				return false
			}
		}
		return true
	}
	return false
}

func (p *pass) lifetimeOK(r *room) bool {
	lt := r.machine.MaxLifetime
	return lt == 0 || r.CreatedAt.Add(lt).Sub(p.in.Now) >= r.machine.MaxLease+lifetimeMargin
}

func (p *pass) roomsOf(machine string) []*room {
	var out []*room
	for _, r := range p.rooms {
		if r.Machine == machine {
			out = append(out, r)
		}
	}
	return out
}

// scope is a limit with the machines that it counts.
type scope struct {
	machines []string
	max      int
}

func (s scope) has(machine string) bool { return slices.Contains(s.machines, machine) }

func (p *pass) count(s scope, without map[*room]bool) int {
	n := 0
	for _, r := range p.rooms {
		if s.has(r.Machine) && !without[r] {
			n++
		}
	}
	return n
}

// scopes returns the machine, provider, and provider limits that apply to m.
func (p *pass) scopes(m *Machine) []scope {
	out := []scope{{machines: []string{m.Key}, max: m.MaxRooms}}
	prov := p.providers[m.Provider]
	if prov == nil {
		return out
	}
	if prov.MaxRooms > 0 {
		var all []string
		for _, mm := range p.in.Machines {
			if mm.Provider == m.Provider {
				all = append(all, mm.Key)
			}
		}
		out = append(out, scope{machines: all, max: prov.MaxRooms})
	}
	for _, l := range prov.Limits {
		if slices.Contains(l.Machines, m.Key) {
			out = append(out, scope{machines: l.Machines, max: l.MaxRooms})
		}
	}
	return out
}

func (p *pass) blockingLimits(m *Machine) []scope {
	return p.blockingWithout(m, nil)
}

func (p *pass) blockingWithout(m *Machine, without map[*room]bool) []scope {
	var out []scope
	for _, s := range p.scopes(m) {
		if p.count(s, without) >= s.max {
			out = append(out, s)
		}
	}
	return out
}

// evict selects idle rooms to destroy so that a new room of one of the machines
// fits in all its limits. It selects nothing while a room in a blocking limit is
// already being destroyed: that room frees the space soon.
func (p *pass) evict(machines []*Machine) ([]int64, bool) {
	for _, m := range machines {
		if !m.OnDemand || p.in.Full[m.Key] {
			continue
		}
		chosen := map[*room]bool{}
		for {
			blocking := p.blockingWithout(m, chosen)
			if len(blocking) == 0 {
				break
			}
			s := blocking[0]
			var oldest *room
			for _, r := range p.rooms {
				if !s.has(r.Machine) || chosen[r] {
					continue
				}
				if r.State == protocol.RoomDestroying || r.evicted {
					return nil, false
				}
				if r.idle() && (oldest == nil || r.CreatedAt.Before(oldest.CreatedAt)) {
					oldest = r
				}
			}
			if oldest == nil {
				chosen = nil
				break
			}
			chosen[oldest] = true
		}
		if len(chosen) == 0 {
			continue
		}
		var ids []int64
		for _, r := range p.rooms {
			if chosen[r] {
				r.evicted = true
				ids = append(ids, r.ID)
			}
		}
		return ids, true
	}
	return nil, false
}

func (p *pass) reason(machines []*Machine, someBlocked bool) string {
	limit, full := false, false
	for _, m := range machines {
		for _, r := range p.roomsOf(m.Key) {
			switch r.State {
			case protocol.RoomCreating, protocol.RoomProvisioning, protocol.RoomResuming:
				// Only a starting room with a free slot can take this lease, for
				// example after an early cancel.
				if slices.ContainsFunc(r.Slots, func(s Slot) bool { return s.State == protocol.SlotFree && !r.busy[s.Name] }) {
					return ReasonRoomStarting
				}
			}
		}
		if m.OnDemand {
			full = full || p.in.Full[m.Key]
			limit = limit || len(p.blockingLimits(m)) > 0
		}
	}
	switch {
	case full:
		return ReasonProviderFull
	case limit:
		return ReasonLimit
	case someBlocked:
		return ReasonEarlierLease
	}
	return ReasonNoSlot
}
