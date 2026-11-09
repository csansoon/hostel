// Package scheduler orders the queue and selects a room or a provider action for
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

// lifetimeMargin is the extra machine lifetime that a lease needs after max_lease.
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
	// PoolTunnels is the number of tunnels without a fixed machine port.
	PoolTunnels int
	Fresh       bool
	// Exclusive leases take all rooms of a machine.
	Exclusive bool
}

// Have is what a machine or a machine gives of one tool.
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

// Template is one machine of the configuration.
type Template struct {
	Key       string // "<provider>/<machine>"
	Provider  string
	OnDemand  bool
	CanResume bool
	// Tools are the machine's tools: the inventories of its machines where
	// they have one, else the configuration.
	Tools map[string]Have
	// PoolPorts is the smallest number of pool ports of its rooms.
	PoolPorts int
	// Rooms are the room names of a new machine.
	Rooms       []string
	MaxMachines int
	MaxLease    time.Duration
	MaxLifetime time.Duration // 0: no limit
}

// Provider has the limits of a provider.
type Provider struct {
	Name        string
	MaxMachines int // 0: no limit
	Limits      []Limit
}

// Limit counts the machines of a subset of the machines of a provider.
type Limit struct {
	Templates   []string // machine keys
	MaxMachines int
}

// Machine is a machine that is not destroyed.
type Machine struct {
	ID        int64
	Template  string
	State     string
	Drained   bool
	CreatedAt time.Time
	// Tools are what the machine gives: its inventory, else its machine's
	// configuration.
	Tools map[string]Have
	// Ready lists the tools that the machine has installed and checked with
	// the current scripts, with their needs.
	Ready map[string]bool
	Rooms []Room
}

type Room struct {
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
	Templates []Template // configuration order
	Providers []Provider
	Machines  []Machine
	Leases    []Lease
	// Full has the machines with a provider "limit reached" flag.
	Full map[string]bool
}

// Action is what a worker does for an assignment.
type Action string

const (
	// UseMachine: a free room in a running machine that is ready for the lease.
	UseMachine Action = "use"
	// ProvisionMachine: a free room in a running machine; the install scripts of
	// the lease's tools run first.
	ProvisionMachine Action = "provision"
	// ResumeMachine: a stopped machine.
	ResumeMachine Action = "resume"
	// CreateMachine: a new machine of Machine; MachineID is 0.
	CreateMachine Action = "create"
)

type Assignment struct {
	LeaseID   string
	Action    Action
	Template  string
	MachineID int64
	// Rooms are the reserved rooms; the first is the lease's room.
	Rooms []string
}

// Reasons why a lease waits (R7).
const (
	ReasonNoRoom          = "no free room"
	ReasonEarlierLease    = "an earlier lease waits for the same machines"
	ReasonMachineStarting = "a machine is starting"
	ReasonLimit           = "machine limit reached"
	ReasonProviderFull    = "provider limit reached"
	ReasonEviction        = "an idle machine is being removed to make space"
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
	// Evict lists idle machines to destroy, so that a waiting lease can get a new machine.
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
func (m *Template) Serves(r Request) bool {
	return gives(m.Tools, r) && m.PoolPorts >= r.PoolTunnels && (!r.Fresh || m.OnDemand)
}

// ready reports whether the machine has installed and checked each requested
// tool.
func (r *machine) ready(req Request) bool {
	for _, w := range req.Tools {
		if !r.Ready[w.Name] {
			return false
		}
	}
	return true
}

// Candidates returns the keys of the machines that can satisfy the request, in
// configuration order.
func Candidates(templates []Template, r Request) []string {
	var keys []string
	for i := range templates {
		if templates[i].Serves(r) {
			keys = append(keys, templates[i].Key)
		}
	}
	return keys
}

type pass struct {
	in        Input
	templates map[string]*Template
	providers map[string]*Provider
	machines  []*machine
	blocked   map[string]bool
}

type machine struct {
	Machine
	template *Template
	evicted  bool
	busy     map[string]bool // rooms taken in this pass
}

// Schedule runs one scheduler pass.
func Schedule(in Input) Output {
	p := &pass{in: in, templates: map[string]*Template{}, providers: map[string]*Provider{}, blocked: map[string]bool{}}
	for i := range in.Templates {
		p.templates[in.Templates[i].Key] = &in.Templates[i]
	}
	for i := range in.Providers {
		p.providers[in.Providers[i].Name] = &in.Providers[i]
	}
	for _, r := range in.Machines {
		m := p.templates[r.Template]
		if m == nil {
			continue // a machine of a machine that left the configuration
		}
		p.machines = append(p.machines, &machine{Machine: r, template: m, busy: map[string]bool{}})
	}
	sort.SliceStable(p.machines, func(i, j int) bool { return p.machines[i].ID < p.machines[j].ID })

	leases := slices.Clone(in.Leases)
	Order(leases)

	var out Output
	var waiting []waitingLease
	for _, l := range leases {
		all := Candidates(in.Templates, l.Request)
		var free []*Template
		for _, k := range all {
			if !p.blocked[k] {
				free = append(free, p.templates[k])
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
			if slices.ContainsFunc(all, func(k string) bool { return slices.Contains(w.templates, k) }) {
				behind++
			}
		}
		waiting = append(waiting, waitingLease{all})
		out.Waiting = append(out.Waiting, Waiting{LeaseID: l.ID, Position: len(waiting), Behind: behind, Reason: reason})
	}
	return out
}

type waitingLease struct{ templates []string }

func (p *pass) assign(l Lease, templates []*Template) (Assignment, bool) {
	if !l.Fresh {
		for _, ready := range []bool{true, false} {
			for _, m := range templates {
				for _, r := range p.machinesOf(m.Key) {
					if r.evicted || r.State != protocol.MachineRunning || r.Drained || !gives(r.Tools, l.Request) || r.ready(l.Request) != ready || !p.lifetimeOK(r) {
						continue
					}
					if rooms := r.take(l.Request); rooms != nil {
						action := UseMachine
						if !ready {
							action = ProvisionMachine
						}
						return Assignment{LeaseID: l.ID, Action: action, Template: m.Key, MachineID: r.ID, Rooms: rooms}, true
					}
				}
			}
		}
		for _, m := range templates {
			if !m.CanResume {
				continue
			}
			for _, r := range p.machinesOf(m.Key) {
				if r.evicted || r.State != protocol.MachineStopped || r.Drained || !gives(r.Tools, l.Request) || !p.lifetimeOK(r) {
					continue
				}
				if rooms := r.take(l.Request); rooms != nil {
					return Assignment{LeaseID: l.ID, Action: ResumeMachine, Template: m.Key, MachineID: r.ID, Rooms: rooms}, true
				}
			}
		}
	}
	for _, m := range templates {
		if !m.OnDemand || p.in.Full[m.Key] || len(p.blockingLimits(m)) > 0 {
			continue
		}
		rooms := m.Rooms[:1]
		if l.Exclusive {
			rooms = slices.Clone(m.Rooms)
		}
		// The new machine counts against the limits for the rest of the pass.
		nr := &machine{Machine: Machine{State: protocol.MachineCreating, Template: m.Key, CreatedAt: p.in.Now}, template: m, busy: map[string]bool{}}
		p.machines = append(p.machines, nr)
		return Assignment{LeaseID: l.ID, Action: CreateMachine, Template: m.Key, Rooms: rooms}, true
	}
	return Assignment{}, false
}

// take reserves the rooms for a lease, or returns nil.
func (r *machine) take(req Request) []string {
	var free []string
	for _, s := range r.Rooms {
		if s.State == protocol.RoomFree && !r.busy[s.Name] {
			free = append(free, s.Name)
		}
	}
	if req.Exclusive {
		if len(free) != len(r.Rooms) || len(free) == 0 || r.roomPorts(free[0]) < req.PoolTunnels {
			return nil
		}
	} else {
		i := slices.IndexFunc(free, func(n string) bool { return r.roomPorts(n) >= req.PoolTunnels })
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

func (r *machine) roomPorts(name string) int {
	for _, s := range r.Rooms {
		if s.Name == name {
			return s.PoolPorts
		}
	}
	return 0
}

// idle reports whether the machine has no lease: running with all rooms free, or stopped.
func (r *machine) idle() bool {
	if r.evicted {
		return false
	}
	switch r.State {
	case protocol.MachineStopped:
		return true
	case protocol.MachineRunning:
		for _, s := range r.Rooms {
			if s.State != protocol.RoomFree || r.busy[s.Name] {
				return false
			}
		}
		return true
	}
	return false
}

func (p *pass) lifetimeOK(r *machine) bool {
	lt := r.template.MaxLifetime
	return lt == 0 || r.CreatedAt.Add(lt).Sub(p.in.Now) >= r.template.MaxLease+lifetimeMargin
}

func (p *pass) machinesOf(template string) []*machine {
	var out []*machine
	for _, r := range p.machines {
		if r.Template == template {
			out = append(out, r)
		}
	}
	return out
}

// scope is a limit with the machines that it counts.
type scope struct {
	templates []string
	max       int
}

func (s scope) has(template string) bool { return slices.Contains(s.templates, template) }

func (p *pass) count(s scope, without map[*machine]bool) int {
	n := 0
	for _, r := range p.machines {
		if s.has(r.Template) && !without[r] {
			n++
		}
	}
	return n
}

// scopes returns the machine, provider, and provider limits that apply to m.
func (p *pass) scopes(m *Template) []scope {
	out := []scope{{templates: []string{m.Key}, max: m.MaxMachines}}
	prov := p.providers[m.Provider]
	if prov == nil {
		return out
	}
	if prov.MaxMachines > 0 {
		var all []string
		for _, mm := range p.in.Templates {
			if mm.Provider == m.Provider {
				all = append(all, mm.Key)
			}
		}
		out = append(out, scope{templates: all, max: prov.MaxMachines})
	}
	for _, l := range prov.Limits {
		if slices.Contains(l.Templates, m.Key) {
			out = append(out, scope{templates: l.Templates, max: l.MaxMachines})
		}
	}
	return out
}

func (p *pass) blockingLimits(m *Template) []scope {
	return p.blockingWithout(m, nil)
}

func (p *pass) blockingWithout(m *Template, without map[*machine]bool) []scope {
	var out []scope
	for _, s := range p.scopes(m) {
		if p.count(s, without) >= s.max {
			out = append(out, s)
		}
	}
	return out
}

// evict selects idle machines to destroy so that a new machine of one of the machines
// fits in all its limits. It selects nothing while a machine in a blocking limit is
// already being destroyed: that machine frees the space soon.
func (p *pass) evict(templates []*Template) ([]int64, bool) {
	for _, m := range templates {
		if !m.OnDemand || p.in.Full[m.Key] {
			continue
		}
		chosen := map[*machine]bool{}
		for {
			blocking := p.blockingWithout(m, chosen)
			if len(blocking) == 0 {
				break
			}
			s := blocking[0]
			var oldest *machine
			for _, r := range p.machines {
				if !s.has(r.Template) || chosen[r] {
					continue
				}
				if r.State == protocol.MachineDestroying || r.evicted {
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
		for _, r := range p.machines {
			if chosen[r] {
				r.evicted = true
				ids = append(ids, r.ID)
			}
		}
		return ids, true
	}
	return nil, false
}

func (p *pass) reason(templates []*Template, someBlocked bool) string {
	limit, full := false, false
	for _, m := range templates {
		for _, r := range p.machinesOf(m.Key) {
			switch r.State {
			case protocol.MachineCreating, protocol.MachineProvisioning, protocol.MachineResuming:
				// Only a starting machine with a free room can take this lease, for
				// example after an early cancel.
				if slices.ContainsFunc(r.Rooms, func(s Room) bool { return s.State == protocol.RoomFree && !r.busy[s.Name] }) {
					return ReasonMachineStarting
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
	return ReasonNoRoom
}
