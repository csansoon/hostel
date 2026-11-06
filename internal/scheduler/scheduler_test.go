// Unit tests are allowed here (AGENTS.md): the scheduler is small, complex, and
// deterministic, and E2E tests cannot reach each order and limit case in
// reasonable time.
package scheduler

import (
	"reflect"
	"testing"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
)

var t0 = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

// haves gives each tool without known options.
func haves(tools ...string) map[string]Have {
	out := map[string]Have{}
	for _, t := range tools {
		out[t] = Have{}
	}
	return out
}

func wants(tools ...string) []Want {
	var out []Want
	for _, t := range tools {
		out = append(out, Want{Name: t})
	}
	return out
}

func static(key string, tools ...string) Machine {
	return Machine{Key: key, Provider: "home", Tools: haves(append([]string{"dev"}, tools...)...),
		PoolPorts: 10, Slots: []string{"a"}, MaxRooms: 1, MaxLease: 2 * time.Hour}
}

func onDemand(key, provider string, maxRooms int) Machine {
	return Machine{Key: key, Provider: provider, OnDemand: true, CanResume: true,
		Tools: haves("dev"), PoolPorts: 10, Slots: []string{"a"}, MaxRooms: maxRooms, MaxLease: 2 * time.Hour}
}

// running is a room that gives every tool of the tests; ready says whether it
// has installed them.
func running(id int64, machine string, ready bool, slotState string) Room {
	return Room{ID: id, Machine: machine, State: protocol.RoomRunning, CreatedAt: t0.Add(time.Duration(id) * time.Minute),
		Tools: haves("dev", "other", "solo", "only-a"),
		Ready: map[string]bool{"dev": ready, "other": ready, "solo": ready, "only-a": ready}, Slots: []Slot{{Name: "a", State: slotState, PoolPorts: 10}}}
}

func lease(id string, prio int, age time.Duration) Lease {
	return Lease{ID: id, Request: Request{Tools: wants("dev")}, Priority: prio, CreatedAt: t0.Add(-age)}
}

func placed(l Lease, pos int) Lease {
	l.ManualPosition = pos
	return l
}

func assigned(out Output) map[string]Assignment {
	m := map[string]Assignment{}
	for _, a := range out.Assignments {
		m[a.LeaseID] = a
	}
	return m
}

func waiting(out Output) map[string]Waiting {
	m := map[string]Waiting{}
	for _, w := range out.Waiting {
		m[w.LeaseID] = w
	}
	return m
}

func TestOrder(t *testing.T) {
	cases := []struct {
		name   string
		leases []Lease
		winner string
	}{
		{"first in, first out", []Lease{lease("b", 1, time.Minute), lease("a", 1, 2*time.Minute)}, "a"},
		{"high passes normal", []Lease{lease("n", 1, 5*time.Minute), lease("h", 2, time.Minute)}, "h"},
		{"no aging: an old low lease waits behind a new normal one", []Lease{lease("l", 0, 5*time.Hour), lease("n", 1, time.Minute)}, "n"},
		{"no aging: an old normal lease waits behind a new high one", []Lease{lease("n", 1, 5*time.Hour), lease("h", 2, time.Minute)}, "h"},
		{"an admin's placed lease goes first", []Lease{lease("h", 2, time.Hour), placed(lease("l", 0, time.Minute), 1)}, "l"},
		{"placed leases keep their order", []Lease{placed(lease("a", 2, time.Hour), 2), placed(lease("b", 0, time.Minute), 1)}, "b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := Schedule(Input{Now: t0, Machines: []Machine{static("home/m")},
				Rooms: []Room{running(1, "home/m", true, protocol.SlotFree)}, Leases: c.leases})
			if len(out.Assignments) != 1 || out.Assignments[0].LeaseID != c.winner {
				t.Fatalf("assignments = %+v, want lease %s", out.Assignments, c.winner)
			}
			if len(out.Waiting) != 1 || out.Waiting[0].Position != 1 {
				t.Fatalf("waiting = %+v, want one lease at position 1", out.Waiting)
			}
		})
	}
}

func TestOptions(t *testing.T) {
	ready := running(1, "p/m1", true, protocol.SlotFree)
	notReady := running(2, "p/m1", false, protocol.SlotFree)
	stopped := running(3, "p/m1", true, protocol.SlotFree)
	stopped.State = protocol.RoomStopped
	m1 := onDemand("p/m1", "p", 5)
	m2 := onDemand("p/m2", "p", 5)

	cases := []struct {
		name   string
		rooms  []Room
		req    Request
		action Action
		room   int64
		mach   string
	}{
		{"ready room first", []Room{notReady, stopped, ready}, Request{}, UseRoom, 1, "p/m1"},
		{"then a room that needs provision", []Room{stopped, notReady}, Request{}, ProvisionRoom, 2, "p/m1"},
		{"then a stopped room", []Room{stopped}, Request{}, ResumeRoom, 3, "p/m1"},
		{"then a new room in configuration order", nil, Request{}, CreateRoom, 0, "p/m1"},
		{"fresh creates a new room", []Room{ready, stopped}, Request{Fresh: true}, CreateRoom, 0, "p/m1"},
		{"option before machine order", []Room{running(4, "p/m2", true, protocol.SlotFree), notReady}, Request{}, UseRoom, 4, "p/m2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.req.Tools = wants("dev")
			out := Schedule(Input{Now: t0, Machines: []Machine{m1, m2}, Providers: []Provider{{Name: "p"}},
				Rooms: c.rooms, Leases: []Lease{{ID: "l", Request: c.req, Priority: 1, CreatedAt: t0}}})
			a, ok := assigned(out)["l"]
			if !ok {
				t.Fatalf("not assigned: %+v", out)
			}
			if a.Action != c.action || a.RoomID != c.room || a.Machine != c.mach || !reflect.DeepEqual(a.Slots, []string{"a"}) {
				t.Fatalf("assignment = %+v, want %s room %d on %s", a, c.action, c.room, c.mach)
			}
		})
	}
}

func TestRoomFilters(t *testing.T) {
	drained := running(1, "home/m", true, protocol.SlotFree)
	drained.Drained = true
	broken := running(1, "home/m", true, protocol.SlotFree)
	broken.State = protocol.RoomBroken
	busy := running(1, "home/m", true, protocol.SlotLeased)
	for name, r := range map[string]Room{"drained": drained, "broken": broken, "busy": busy} {
		t.Run(name, func(t *testing.T) {
			out := Schedule(Input{Now: t0, Machines: []Machine{static("home/m")}, Rooms: []Room{r},
				Leases: []Lease{lease("l", 1, 0)}})
			if len(out.Assignments) != 0 || waiting(out)["l"].Reason != ReasonNoSlot {
				t.Fatalf("got %+v, want the lease to wait with %q", out, ReasonNoSlot)
			}
		})
	}
}

func TestLifetime(t *testing.T) {
	m := onDemand("p/m", "p", 2)
	m.MaxLifetime = 3 * time.Hour
	old := running(1, "p/m", true, protocol.SlotFree)
	old.CreatedAt = t0.Add(-56 * time.Minute) // 2h04m left; a lease needs 2h05m
	young := running(2, "p/m", true, protocol.SlotFree)
	young.CreatedAt = t0.Add(-55 * time.Minute) // 2h05m left
	out := Schedule(Input{Now: t0, Machines: []Machine{m}, Providers: []Provider{{Name: "p"}}, Rooms: []Room{old, young},
		Leases: []Lease{lease("l", 1, 0)}})
	if a := assigned(out)["l"]; a.RoomID != 2 {
		t.Fatalf("assignment = %+v, want room 2 (room 1 has too little lifetime)", a)
	}
}

func TestBlockedMachines(t *testing.T) {
	machines := []Machine{static("home/m1"), static("home/m2")}
	machines[1].Tools = haves("other")
	rooms := []Room{running(1, "home/m1", true, protocol.SlotLeased), running(2, "home/m2", true, protocol.SlotFree)}
	first := lease("first", 1, 3*time.Minute)
	second := lease("second", 1, 2*time.Minute)
	other := lease("other", 1, time.Minute)
	other.Tools = wants("other")
	out := Schedule(Input{Now: t0, Machines: machines, Rooms: rooms, Leases: []Lease{first, second, other}})
	if a := assigned(out)["other"]; a.RoomID != 2 {
		t.Fatalf("other tool: assignment = %+v, want room 2", a)
	}
	w := waiting(out)
	if w["first"].Reason != ReasonNoSlot || w["first"].Position != 1 {
		t.Errorf("first = %+v, want position 1, %q", w["first"], ReasonNoSlot)
	}
	if w["second"].Reason != ReasonEarlierLease || w["second"].Position != 2 || w["second"].Behind != 1 {
		t.Errorf("second = %+v, want position 2, behind 1, %q", w["second"], ReasonEarlierLease)
	}
}

func TestBlockedMachineKeepsLaterLeaseOff(t *testing.T) {
	// "dev" can use m1 or m2; "solo" only m2. Both rooms are busy, so nothing
	// is assigned. When m2 is free, the lease that comes first in the order gets it.
	m1, m2 := static("home/m1"), static("home/m2")
	m2.Tools = haves("dev", "solo")
	rooms := []Room{running(1, "home/m1", true, protocol.SlotLeased), running(2, "home/m2", true, protocol.SlotLeased)}
	dev := lease("dev", 1, 2*time.Minute)
	solo := lease("solo", 2, time.Minute)
	solo.Tools = wants("solo")
	out := Schedule(Input{Now: t0, Machines: []Machine{m1, m2}, Rooms: rooms, Leases: []Lease{dev, solo}})
	if len(out.Assignments) != 0 {
		t.Fatalf("assignments = %+v, want none", out.Assignments)
	}
	// Now m2 is free: the "solo" lease has a higher priority, so it comes first
	// and takes m2. The "dev" lease waits.
	rooms[1].Slots[0].State = protocol.SlotFree
	out = Schedule(Input{Now: t0, Machines: []Machine{m1, m2}, Rooms: rooms, Leases: []Lease{dev, solo}})
	if a := assigned(out)["solo"]; a.RoomID != 2 {
		t.Fatalf("solo: assignment = %+v, want room 2", a)
	}
	// With equal priority the older "dev" lease comes first and takes m2.
	solo.Priority = 1
	out = Schedule(Input{Now: t0, Machines: []Machine{m1, m2}, Rooms: rooms, Leases: []Lease{dev, solo}})
	if a := assigned(out)["dev"]; a.RoomID != 2 {
		t.Fatalf("dev: assignment = %+v, want room 2", a)
	}
	if w := waiting(out)["solo"]; w.Reason != ReasonNoSlot {
		t.Fatalf("solo = %+v, want %q", w, ReasonNoSlot)
	}
}

func TestCandidates(t *testing.T) {
	a := static("home/a", "xcode", "macos-desktop")
	a.Tools["node"] = Have{Options: []string{"22.11.0", "20.18.1"}, Checked: true}
	a.Tools["screen"] = Have{Screen: true, Options: []string{"phone", "tablet"}}
	b := static("home/b", "xcode")
	b.PoolPorts = 1
	b.Tools["node"] = Have{Options: []string{"22"}}
	c := onDemand("p/c", "p", 1)
	machines := []Machine{a, b, c}
	cases := []struct {
		name string
		req  Request
		want []string
	}{
		{"no tools: every machine", Request{}, []string{"home/a", "home/b", "p/c"}},
		{"tool", Request{Tools: wants("dev")}, []string{"home/a", "home/b", "p/c"}},
		{"tool of two machines", Request{Tools: wants("xcode")}, []string{"home/a", "home/b"}},
		{"tools together", Request{Tools: wants("xcode", "macos-desktop")}, []string{"home/a"}},
		{"a version prefix", Request{Tools: []Want{{"node", "22"}}}, []string{"home/a", "home/b"}},
		{"a longer version: inventory, and a configured version that can give it", Request{Tools: []Want{{"node", "22.11"}}}, []string{"home/a", "home/b"}},
		{"a version prefix by parts", Request{Tools: []Want{{"node", "2"}}}, nil},
		{"two versions", Request{Tools: []Want{{"node", "20"}, {"node", "22"}}}, []string{"home/a"}},
		{"a screen option", Request{Tools: []Want{{"screen", "tablet"}}}, []string{"home/a"}},
		{"a screen option matches exactly", Request{Tools: []Want{{"screen", "tab"}}}, nil},
		{"pool ports", Request{Tools: wants("dev"), PoolTunnels: 2}, []string{"home/a", "p/c"}},
		{"fresh", Request{Tools: wants("dev"), Fresh: true}, []string{"p/c"}},
		{"unknown tool", Request{Tools: wants("kvm")}, nil},
	}
	for _, c := range cases {
		if got := Candidates(machines, c.req); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: Candidates = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRoomTools(t *testing.T) {
	// The machine says node 22 (configured); the room's inventory has 22.11.0
	// only, so a lease for node:22.12 cannot use it.
	m := static("home/m")
	m.Tools["node"] = Have{Options: []string{"22"}}
	r := running(1, "home/m", true, protocol.SlotFree)
	r.Tools["node"] = Have{Options: []string{"22.11.0"}, Checked: true}
	r.Ready["node"] = true
	l := lease("l", 1, 0)
	l.Tools = []Want{{"node", "22.12"}}
	out := Schedule(Input{Now: t0, Machines: []Machine{m}, Rooms: []Room{r}, Leases: []Lease{l}})
	if len(out.Assignments) != 0 {
		t.Fatalf("assignments = %+v, want none", out.Assignments)
	}
	l.Tools = []Want{{"node", "22.11"}}
	out = Schedule(Input{Now: t0, Machines: []Machine{m}, Rooms: []Room{r}, Leases: []Lease{l}})
	if a := assigned(out)["l"]; a.RoomID != 1 || a.Action != UseRoom {
		t.Fatalf("assignment = %+v, want room 1", a)
	}
}

func TestExclusive(t *testing.T) {
	m := onDemand("p/m", "p", 1)
	m.Slots = []string{"a", "b"}
	r := running(1, "p/m", true, protocol.SlotFree)
	r.Slots = []Slot{{Name: "a", State: protocol.SlotLeased, PoolPorts: 10}, {Name: "b", State: protocol.SlotFree, PoolPorts: 10}}
	ex := lease("ex", 1, 2*time.Minute)
	ex.Exclusive = true
	normal := lease("normal", 1, time.Minute)
	out := Schedule(Input{Now: t0, Machines: []Machine{m}, Providers: []Provider{{Name: "p"}}, Rooms: []Room{r}, Leases: []Lease{ex, normal}})
	if len(out.Assignments) != 0 {
		t.Fatalf("assignments = %+v, want none (exclusive lease needs all slots; it blocks the machine)", out.Assignments)
	}
	r.Slots[0].State = protocol.SlotFree
	out = Schedule(Input{Now: t0, Machines: []Machine{m}, Providers: []Provider{{Name: "p"}}, Rooms: []Room{r}, Leases: []Lease{ex, normal}})
	if a := assigned(out)["ex"]; !reflect.DeepEqual(a.Slots, []string{"a", "b"}) {
		t.Fatalf("exclusive assignment = %+v, want slots a and b", a)
	}
	if _, ok := assigned(out)["normal"]; ok {
		t.Fatalf("normal lease got a slot of a room that an exclusive lease took")
	}
	out = Schedule(Input{Now: t0, Machines: []Machine{m}, Providers: []Provider{{Name: "p"}}, Leases: []Lease{ex}})
	if a := assigned(out)["ex"]; a.Action != CreateRoom || !reflect.DeepEqual(a.Slots, []string{"a", "b"}) {
		t.Fatalf("exclusive new room = %+v, want create with slots a and b", a)
	}
}

func TestSlotPorts(t *testing.T) {
	m := static("home/m")
	m.Slots = []string{"a", "b"}
	r := running(1, "home/m", true, protocol.SlotFree)
	r.Slots = []Slot{{Name: "a", State: protocol.SlotFree, PoolPorts: 1}, {Name: "b", State: protocol.SlotFree, PoolPorts: 3}}
	l := lease("l", 1, 0)
	l.PoolTunnels = 2
	m.PoolPorts = 3
	out := Schedule(Input{Now: t0, Machines: []Machine{m}, Rooms: []Room{r}, Leases: []Lease{l}})
	if a := assigned(out)["l"]; !reflect.DeepEqual(a.Slots, []string{"b"}) {
		t.Fatalf("assignment = %+v, want slot b (slot a has too few ports)", a)
	}
}

func TestLimits(t *testing.T) {
	cases := []struct {
		name      string
		machines  []Machine
		providers []Provider
		rooms     []Room
		full      map[string]bool
		leases    int
		creates   int
		reason    string
	}{
		{
			name:     "machine max_rooms",
			machines: []Machine{onDemand("p/m", "p", 2)}, providers: []Provider{{Name: "p"}},
			leases: 3, creates: 2, reason: ReasonLimit,
		},
		{
			name:     "machine max_rooms counts existing rooms",
			machines: []Machine{onDemand("p/m", "p", 2)}, providers: []Provider{{Name: "p"}},
			rooms:  []Room{running(1, "p/m", true, protocol.SlotLeased)},
			leases: 3, creates: 1, reason: ReasonLimit,
		},
		{
			name:     "provider max_rooms covers two machines",
			machines: []Machine{onDemand("p/a", "p", 5), onDemand("p/b", "p", 5)}, providers: []Provider{{Name: "p", MaxRooms: 3}},
			leases: 5, creates: 3, reason: ReasonLimit,
		},
		{
			name:      "provider limits entry covers a subset",
			machines:  []Machine{onDemand("p/a", "p", 5), onDemand("p/b", "p", 5), onDemand("p/c", "p", 5)},
			providers: []Provider{{Name: "p", Limits: []Limit{{Machines: []string{"p/a", "p/b"}, MaxRooms: 1}}}},
			// a takes the one place of the a+b limit; c is not in the limit.
			leases: 7, creates: 6, reason: ReasonLimit,
		},
		{
			name:     "full flag",
			machines: []Machine{onDemand("p/m", "p", 5)}, providers: []Provider{{Name: "p"}},
			full:   map[string]bool{"p/m": true},
			leases: 1, creates: 0, reason: ReasonProviderFull,
		},
		{
			name:     "room in creation",
			machines: []Machine{onDemand("p/m", "p", 1)}, providers: []Provider{{Name: "p"}},
			rooms:  []Room{{ID: 1, Machine: "p/m", State: protocol.RoomCreating, CreatedAt: t0, Slots: []Slot{{Name: "a", State: protocol.SlotFree}}}},
			leases: 1, creates: 0, reason: ReasonRoomStarting,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var leases []Lease
			for i := range c.leases {
				leases = append(leases, lease(string(rune('a'+i)), 1, time.Duration(c.leases-i)*time.Minute))
			}
			out := Schedule(Input{Now: t0, Machines: c.machines, Providers: c.providers, Rooms: c.rooms, Full: c.full, Leases: leases})
			creates := 0
			for _, a := range out.Assignments {
				if a.Action == CreateRoom {
					creates++
				}
			}
			if creates != c.creates {
				t.Fatalf("creates = %d, want %d: %+v", creates, c.creates, out.Assignments)
			}
			if len(out.Waiting) == 0 || out.Waiting[0].Reason != c.reason {
				t.Fatalf("waiting = %+v, want first reason %q", out.Waiting, c.reason)
			}
			if len(out.Evict) != 0 {
				t.Fatalf("evict = %v, want none", out.Evict)
			}
		})
	}
}

func TestLimitSubsetDoesNotBlockOtherMachines(t *testing.T) {
	machines := []Machine{onDemand("p/a", "p", 5), onDemand("p/c", "p", 5)}
	providers := []Provider{{Name: "p", Limits: []Limit{{Machines: []string{"p/a"}, MaxRooms: 1}}}}
	out := Schedule(Input{Now: t0, Machines: machines, Providers: providers, Leases: []Lease{lease("x", 1, 2*time.Minute), lease("y", 1, time.Minute)}})
	a := assigned(out)
	if a["x"].Machine != "p/a" || a["y"].Machine != "p/c" {
		t.Fatalf("assignments = %+v, want x on p/a and y on p/c", out.Assignments)
	}
}

func TestEviction(t *testing.T) {
	m := onDemand("p/m", "p", 2)
	providers := []Provider{{Name: "p"}}
	idleOld := running(1, "p/m", true, protocol.SlotFree)
	idleNew := running(2, "p/m", true, protocol.SlotFree)
	busy := running(3, "p/m", true, protocol.SlotLeased)
	stopped := running(4, "p/m", true, protocol.SlotFree)
	stopped.State = protocol.RoomStopped
	stopped.CreatedAt = t0.Add(-time.Hour)
	destroying := running(5, "p/m", true, protocol.SlotFree)
	destroying.State = protocol.RoomDestroying
	fresh := lease("l", 1, 0)
	fresh.Fresh = true

	cases := []struct {
		name   string
		rooms  []Room
		evict  []int64
		reason string
	}{
		{"oldest idle running room", []Room{idleNew, idleOld}, []int64{1}, ReasonEviction},
		{"stopped room is idle", []Room{busy, stopped}, []int64{4}, ReasonEviction},
		{"busy rooms are not evicted", []Room{busy, running(6, "p/m", true, protocol.SlotLeased)}, nil, ReasonLimit},
		{"wait while a room is being destroyed", []Room{idleOld, destroying}, nil, ReasonLimit},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := Schedule(Input{Now: t0, Machines: []Machine{m}, Providers: providers, Rooms: c.rooms, Leases: []Lease{fresh}})
			if len(out.Assignments) != 0 {
				t.Fatalf("assignments = %+v, want none", out.Assignments)
			}
			if !reflect.DeepEqual(out.Evict, c.evict) {
				t.Fatalf("evict = %v, want %v", out.Evict, c.evict)
			}
			if r := waiting(out)["l"].Reason; r != c.reason {
				t.Fatalf("reason = %q, want %q", r, c.reason)
			}
		})
	}
}

func TestEvictionNeedsAllLimits(t *testing.T) {
	// A new room of p/a is blocked by its machine limit and by the provider
	// limit. An idle room of p/b frees the provider limit only, so nothing is
	// evicted; an idle room of p/a frees both.
	a, b := onDemand("p/a", "p", 1), onDemand("p/b", "p", 1)
	a.Tools = haves("only-a")
	providers := []Provider{{Name: "p", MaxRooms: 2}}
	busyA := running(1, "p/a", true, protocol.SlotLeased)
	idleB := running(2, "p/b", true, protocol.SlotFree)
	l := lease("l", 1, 0)
	l.Tools = wants("only-a")
	out := Schedule(Input{Now: t0, Machines: []Machine{a, b}, Providers: providers, Rooms: []Room{busyA, idleB}, Leases: []Lease{l}})
	if len(out.Evict) != 0 {
		t.Fatalf("evict = %v, want none", out.Evict)
	}
	idleA := running(1, "p/a", false, protocol.SlotFree)
	idleA.State = protocol.RoomStopped
	out = Schedule(Input{Now: t0, Machines: []Machine{a, b}, Providers: providers, Rooms: []Room{idleA, idleB}, Leases: []Lease{l}})
	// The stopped room of p/a is resumed (option 3), not evicted.
	if a := assigned(out)["l"]; a.Action != ResumeRoom || a.RoomID != 1 {
		t.Fatalf("assignment = %+v, want resume of room 1", a)
	}
	l.Fresh = true
	out = Schedule(Input{Now: t0, Machines: []Machine{a, b}, Providers: providers, Rooms: []Room{idleA, idleB}, Leases: []Lease{l}})
	if !reflect.DeepEqual(out.Evict, []int64{1}) {
		t.Fatalf("fresh: evict = %v, want [1]", out.Evict)
	}
}

func TestEvictedRoomIsNotUsed(t *testing.T) {
	// The first lease (fresh) evicts the idle room. The second lease must not
	// take a slot of that room in the same pass.
	m := onDemand("p/m", "p", 1)
	idle := running(1, "p/m", true, protocol.SlotFree)
	first := lease("first", 1, 2*time.Minute)
	first.Fresh = true
	second := lease("second", 1, time.Minute)
	out := Schedule(Input{Now: t0, Machines: []Machine{m}, Providers: []Provider{{Name: "p"}}, Rooms: []Room{idle}, Leases: []Lease{first, second}})
	if len(out.Assignments) != 0 || !reflect.DeepEqual(out.Evict, []int64{1}) {
		t.Fatalf("got %+v, want eviction of room 1 and no assignment", out)
	}
}

func TestOneWaitingLine(t *testing.T) {
	// One line for all leases; "behind" counts only the earlier leases that
	// share a machine.
	m := static("home/m")
	m.Tools = haves("dev", "other")
	n := static("home/n")
	n.Tools = haves("solo")
	rooms := []Room{running(1, "home/m", true, protocol.SlotLeased), running(2, "home/n", true, protocol.SlotLeased)}
	a, b, c := lease("a", 1, 3*time.Minute), lease("b", 1, 2*time.Minute), lease("c", 1, time.Minute)
	b.Tools = wants("solo")
	out := Schedule(Input{Now: t0, Machines: []Machine{m, n}, Rooms: rooms, Leases: []Lease{a, b, c}})
	w := waiting(out)
	if w["a"].Position != 1 || w["b"].Position != 2 || w["c"].Position != 3 {
		t.Fatalf("positions = %+v, want a=1, b=2, c=3", out.Waiting)
	}
	if w["a"].Behind != 0 || w["b"].Behind != 0 || w["c"].Behind != 1 {
		t.Fatalf("behind = %+v, want a=0, b=0 (another machine), c=1", out.Waiting)
	}
}

func TestOrderOneLine(t *testing.T) {
	a := func(id string, prio int, age time.Duration, placed int) Lease {
		return Lease{ID: id, Priority: prio, CreatedAt: t0.Add(-age), ManualPosition: placed}
	}
	leases := []Lease{
		a("old", protocol.PriorityNormal, time.Hour, 0),
		a("placed", protocol.PriorityLow, time.Minute, 1),
		a("normal", protocol.PriorityNormal, 30*time.Minute, 0),
		a("high", protocol.PriorityHigh, time.Second, 0),
	}
	Order(leases)
	var got []string
	for _, l := range leases {
		got = append(got, l.ID)
	}
	want := []string{"placed", "high", "old", "normal"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}
