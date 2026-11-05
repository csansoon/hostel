package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
)

// RunDemo starts the app with made-up data and no server ("hostel --demo"):
// rooms get ready, the waiting line moves, leases end. It does not read or
// change the guest configuration.
func RunDemo(version string, stdout io.Writer) error {
	m := &model{version: version, demo: true}
	m.guest, m.c = &config.Guest{Server: "demo.hostel.example"}, newDemoServer()
	m.gen = 1
	m.pages = []page{newMainMenu()}
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	for _, s := range m.keep {
		fmt.Fprintln(stdout, s)
	}
	return err
}

// demoServer answers the app's requests from a small simulated world.
type demoServer struct {
	mu      sync.Mutex
	rev     int
	changed chan struct{}
	rnd     *rand.Rand

	machines []protocol.MachineStatus
	rooms    []protocol.RoomStatus
	leases   []protocol.LeaseStatus
	// readyAt is when a room or a lease in preparation is ready; endAt is
	// when a ready lease ends or a cleaning lease is gone.
	readyAt map[string]time.Time
	endAt   map[string]time.Time
	nextID  int
	nextArr time.Time
}

type demoPlatform struct {
	name, desc string
	devices    []string
	typical    time.Duration
}

var demoPlatforms = []demoPlatform{
	{"ios", "iPhone and iPad simulators", []string{"iphone-17-pro", "ipad-pro-13"}, 24 * time.Minute},
	{"android", "Android emulators", []string{"pixel-9", "pixel-tablet"}, 18 * time.Minute},
	{"macos", "macOS desktop apps", nil, 12 * time.Minute},
	{"windows", "Windows desktop apps", nil, 15 * time.Minute},
	{"linux", "Linux builds and tests", nil, 8 * time.Minute},
}

var (
	demoUsers = []string{"ana", "dani", "lee", "mo", "sam"}
	demoTasks = []string{"login flow", "checkout tests", "iPad layout", "dark mode QA", "release build",
		"push notifications", "onboarding screens", "crash on resume", "settings sync", "offline mode"}
)

func newDemoServer() *demoServer {
	now := time.Now()
	d := &demoServer{changed: make(chan struct{}), rnd: rand.New(rand.NewPCG(7, 11)),
		readyAt: map[string]time.Time{}, endAt: map[string]time.Time{}, nextArr: now.Add(6 * time.Second)}
	d.machines = []protocol.MachineStatus{
		{Key: "home/macbook", Kind: "static", OS: "macos", Tools: []string{"ios", "android", "macos"}, MaxRooms: 1, Connect: true, ConnectVersion: "v0.2.0"},
		{Key: "home/win", Kind: "static", OS: "windows", Tools: []string{"windows"}, MaxRooms: 1, Connect: true, ConnectVersion: "v0.2.0"},
		{Key: "office/mini", Kind: "static", OS: "macos", Tools: []string{"ios", "macos"}, MaxRooms: 1, Connect: true, ConnectVersion: "v0.1.0"},
		{Key: "lab/linux", Kind: "static", OS: "linux", Tools: []string{"linux", "android"}, MaxRooms: 1},
		{Key: "ns/linux", Kind: "namespace", OS: "linux", Tools: []string{"linux", "android"}, MaxRooms: 2},
	}
	hw := func(cpu string, cores int, mem int64, osv string) *protocol.Hardware {
		return &protocol.Hardware{CPU: cpu, Cores: cores, MemoryBytes: mem << 30, OSVersion: osv}
	}
	d.rooms = []protocol.RoomStatus{
		{ID: 1, Name: "home-macbook", Provider: "home", Machine: "macbook", OS: "macos", Arch: "arm64", State: protocol.RoomRunning,
			Tools: []string{"ios", "android", "macos"}, Hardware: hw("Apple M3 Pro", 12, 36, "macOS 26.1"),
			Slots: []protocol.SlotStatus{{Name: "slot-1", State: protocol.SlotFree}, {Name: "slot-2", State: protocol.SlotFree}}},
		{ID: 2, Name: "home-win", Provider: "home", Machine: "win", OS: "windows", Arch: "amd64", State: protocol.RoomRunning,
			Tools: []string{"windows"}, Hardware: hw("Intel Xeon", 4, 16, "Windows 11"),
			Slots: []protocol.SlotStatus{{Name: "slot-1", State: protocol.SlotFree}}},
		{ID: 3, Name: "office-mini", Provider: "office", Machine: "mini", OS: "macos", Arch: "arm64", State: protocol.RoomRunning,
			Tools: []string{"ios", "macos"}, Hardware: hw("Apple M2", 8, 16, "macOS 26.0"),
			Slots: []protocol.SlotStatus{{Name: "slot-1", State: protocol.SlotFree}}},
		{ID: 4, Name: "lab-linux", Provider: "lab", Machine: "linux", OS: "linux", Arch: "amd64", State: protocol.RoomBroken,
			Reason: "unreachable", LastSeen: now.Add(-3 * time.Hour), Tools: []string{"linux", "android"},
			Slots: []protocol.SlotStatus{{Name: "slot-1", State: protocol.SlotBroken}}},
	}
	d.nextID = 10
	// A busy start: two guests in rooms, one getting ready, two waiting.
	d.place(d.newLease("ana", "ios", "login flow", "normal", now.Add(-21*time.Minute)), 1, "slot-1", protocol.LeaseReady, now.Add(-18*time.Minute))
	d.place(d.newLease("dani", "windows", "release build", "normal", now.Add(-9*time.Minute)), 2, "slot-1", protocol.LeaseReady, now.Add(-7*time.Minute))
	d.place(d.newLease("lee", "ios", "iPad layout", "high", now.Add(-40*time.Second)), 3, "slot-1", protocol.LeasePreparing, time.Time{})
	d.readyAt[d.leases[2].ID] = now.Add(3 * time.Second)
	d.queue(d.newLease("mo", "windows", "settings sync", "normal", now.Add(-4*time.Minute)))
	d.queue(d.newLease("sam", "windows", "crash on resume", "low", now.Add(-1*time.Minute)))
	d.endAt[d.leases[0].ID] = now.Add(40 * time.Second)
	d.endAt[d.leases[1].ID] = now.Add(9 * time.Second)
	d.positions(now)
	go d.run()
	return d
}

func (d *demoServer) newLease(user, platform, task, prio string, created time.Time) *protocol.LeaseStatus {
	d.nextID++
	repo := "github.com/example/app"
	return &protocol.LeaseStatus{ID: "lease-" + strconv.Itoa(d.nextID), Platform: platform, Task: task, Priority: prio,
		User: user, Guest: user + "-laptop", Repo: repo, Branch: strings.ReplaceAll(task, " ", "-"), CreatedAt: created}
}

func (d *demoServer) place(l *protocol.LeaseStatus, roomID int64, slot, state string, ready time.Time) {
	r := d.room(roomID)
	l.State, l.RoomID, l.Slot, l.Provider, l.Machine, l.ReadyAt = state, roomID, slot, r.Provider, r.Machine, ready
	for i := range r.Slots {
		if r.Slots[i].Name == slot {
			r.Slots[i].State, r.Slots[i].LeaseID = protocol.SlotLeased, l.ID
		}
	}
	d.leases = append(d.leases, *l)
}

func (d *demoServer) queue(l *protocol.LeaseStatus) {
	l.State = protocol.LeaseQueued
	d.leases = append(d.leases, *l)
}

func (d *demoServer) room(id int64) *protocol.RoomStatus {
	for i := range d.rooms {
		if d.rooms[i].ID == id {
			return &d.rooms[i]
		}
	}
	return nil
}

// run moves the world once a second.
func (d *demoServer) run() {
	for range time.Tick(time.Second) {
		d.mu.Lock()
		if d.step(time.Now()) {
			d.bump()
		}
		d.mu.Unlock()
	}
}

func (d *demoServer) bump() {
	d.rev++
	close(d.changed)
	d.changed = make(chan struct{})
}

func (d *demoServer) step(now time.Time) bool {
	changed := false
	for i := range d.rooms {
		r := &d.rooms[i]
		if t, ok := d.readyAt[r.Name]; ok && now.After(t) {
			delete(d.readyAt, r.Name)
			r.State = protocol.RoomRunning
			changed = true
		}
	}
	var keep []protocol.LeaseStatus
	for _, l := range d.leases {
		switch l.State {
		case protocol.LeaseAssigned, protocol.LeasePreparing:
			r := d.room(l.RoomID)
			if t, ok := d.readyAt[l.ID]; ok && now.After(t) && r.State == protocol.RoomRunning {
				delete(d.readyAt, l.ID)
				l.State, l.ReadyAt = protocol.LeaseReady, now
				d.endAt[l.ID] = now.Add(time.Duration(25+d.rnd.IntN(50)) * time.Second)
				changed = true
			} else if r.State == protocol.RoomRunning && l.State == protocol.LeaseAssigned {
				l.State = protocol.LeasePreparing
				changed = true
			}
		case protocol.LeaseReady:
			if now.After(d.endAt[l.ID]) {
				l.State = protocol.LeaseCleaning
				d.endAt[l.ID] = now.Add(3 * time.Second)
				changed = true
			}
		case protocol.LeaseCleaning:
			if now.After(d.endAt[l.ID]) {
				d.free(l)
				changed = true
				continue
			}
		}
		keep = append(keep, l)
	}
	d.leases = keep
	if now.After(d.nextArr) && waiting(protocol.Status{Leases: d.leases}) < 4 {
		p := demoPlatforms[d.rnd.IntN(len(demoPlatforms))]
		prio := []string{"normal", "normal", "normal", "high", "low"}[d.rnd.IntN(5)]
		d.queue(d.newLease(demoUsers[d.rnd.IntN(len(demoUsers))], p.name, demoTasks[d.rnd.IntN(len(demoTasks))], prio, now))
		d.nextArr = now.Add(time.Duration(8+d.rnd.IntN(14)) * time.Second)
		changed = true
	}
	if d.schedule(now) {
		changed = true
	}
	d.positions(now)
	return changed
}

// free gives the lease's slot back; an on-demand room without leases goes.
func (d *demoServer) free(l protocol.LeaseStatus) {
	r := d.room(l.RoomID)
	if r == nil {
		return
	}
	for i := range r.Slots {
		if r.Slots[i].LeaseID == l.ID {
			r.Slots[i] = protocol.SlotStatus{Name: r.Slots[i].Name, State: protocol.SlotFree}
		}
	}
	if r.Provider == "ns" {
		d.rooms = slices.DeleteFunc(d.rooms, func(x protocol.RoomStatus) bool { return x.ID == r.ID })
	}
}

// schedule puts queued leases on free slots, first by priority, then by age;
// an on-demand machine makes a new room.
func (d *demoServer) schedule(now time.Time) bool {
	changed := false
	for _, l := range d.ordered() {
		i := slices.IndexFunc(d.leases, func(x protocol.LeaseStatus) bool { return x.ID == l.ID })
		lease := &d.leases[i]
		placed := false
		for ri := range d.rooms {
			r := &d.rooms[ri]
			if !freeSlot(*r, l.Platform) {
				continue
			}
			for si := range r.Slots {
				if r.Slots[si].State == protocol.SlotFree {
					r.Slots[si].State, r.Slots[si].LeaseID = protocol.SlotLeased, l.ID
					lease.State, lease.RoomID, lease.Slot, lease.Provider, lease.Machine = protocol.LeasePreparing, r.ID, r.Slots[si].Name, r.Provider, r.Machine
					d.readyAt[l.ID] = now.Add(time.Duration(4+d.rnd.IntN(5)) * time.Second)
					placed = true
					break
				}
			}
			break
		}
		if !placed {
			ns := d.machines[4]
			if slices.Contains(ns.Tools, l.Platform) && len(machineRooms(protocol.Status{Rooms: d.rooms}, ns.Key)) < ns.MaxRooms {
				d.nextID++
				name := "ns-linux-" + strconv.Itoa(d.nextID)
				d.rooms = append(d.rooms, protocol.RoomStatus{ID: int64(d.nextID), Name: name, Provider: "ns", Machine: "linux", OS: "linux", Arch: "amd64",
					State: protocol.RoomProvisioning, Tools: ns.Tools, Deadline: now.Add(2 * time.Hour),
					Hardware: &protocol.Hardware{CPU: "AMD EPYC", Cores: 8, MemoryBytes: 16 << 30, OSVersion: "Ubuntu 24.04"},
					Slots:    []protocol.SlotStatus{{Name: "slot-1", State: protocol.SlotLeased, LeaseID: l.ID}}})
				d.readyAt[name] = now.Add(8 * time.Second)
				d.readyAt[l.ID] = now.Add(11 * time.Second)
				lease.State, lease.RoomID, lease.Slot, lease.Provider, lease.Machine = protocol.LeaseAssigned, int64(d.nextID), "slot-1", "ns", "linux"
				placed = true
			}
		}
		changed = changed || placed
	}
	return changed
}

// ordered returns the queued leases in queue order.
func (d *demoServer) ordered() []protocol.LeaseStatus {
	var q []protocol.LeaseStatus
	for _, l := range d.leases {
		if l.State == protocol.LeaseQueued {
			q = append(q, l)
		}
	}
	rank := map[string]int{"high": 0, "normal": 1, "low": 2}
	slices.SortStableFunc(q, func(a, b protocol.LeaseStatus) int {
		if rank[a.Priority] != rank[b.Priority] {
			return rank[a.Priority] - rank[b.Priority]
		}
		return a.CreatedAt.Compare(b.CreatedAt)
	})
	return q
}

// positions numbers each platform's queue and estimates the wait.
func (d *demoServer) positions(now time.Time) {
	n := map[string]int{}
	for _, q := range d.ordered() {
		n[q.Platform]++
		for i := range d.leases {
			if d.leases[i].ID == q.ID {
				d.leases[i].Position = n[q.Platform]
				d.leases[i].EstimatedWaitSeconds = d.wait(q.Platform, now) + (n[q.Platform]-1)*300
				d.leases[i].Reason = "every room for " + q.Platform + " is in use"
			}
		}
	}
}

// wait is the time until the first room for a platform is free.
func (d *demoServer) wait(platform string, now time.Time) int {
	best := -1
	for _, l := range d.leases {
		if l.State == protocol.LeaseQueued || !slices.Contains(d.room(l.RoomID).Tools, platform) {
			continue
		}
		s := int(d.endAt[l.ID].Sub(now).Seconds()) + 120
		if best < 0 || s < best {
			best = s
		}
	}
	return max(best, 60)
}

func (d *demoServer) status() protocol.Status {
	return protocol.Status{Revision: strconv.Itoa(d.rev), ServerID: "demo-7f3a", User: "sam", Role: protocol.RoleAdmin,
		Machines: slices.Clone(d.machines), Rooms: slices.Clone(d.rooms), Leases: slices.Clone(d.leases)}
}

func (d *demoServer) platforms() []protocol.Platform {
	st := d.status()
	var out []protocol.Platform
	for _, p := range demoPlatforms {
		pl := protocol.Platform{Name: p.name, Kind: "device", Description: p.desc, Devices: p.devices, MaxDevices: len(p.devices),
			TypicalLeaseSeconds: int(p.typical.Seconds())}
		for _, ms := range d.machines {
			if slices.Contains(ms.Tools, p.name) {
				pl.Machines = append(pl.Machines, ms.Key)
			}
		}
		for _, r := range d.rooms {
			if freeSlot(r, p.name) {
				pl.FreeSlots++
			}
		}
		pl.Queue = len(queued(st, p.name))
		if pl.Queue > 0 {
			pl.EstimatedWaitSeconds = d.wait(p.name, time.Now())
		}
		out = append(out, pl)
	}
	return out
}

// Do answers one request of the app like the server would.
func (d *demoServer) Do(ctx context.Context, method, path string, in, out any) error {
	route, query, _ := strings.Cut(path, "?")
	if method == "GET" && route == "/v1/status" && query != "" {
		// The long poll: wait for a change of the revision.
		d.mu.Lock()
		ch, since := d.changed, ""
		for _, kv := range strings.Split(query, "&") {
			if v, ok := strings.CutPrefix(kv, "since="); ok {
				since = v
			}
		}
		same := since == strconv.Itoa(d.rev)
		d.mu.Unlock()
		if same {
			select {
			case <-ch:
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(30 * time.Second):
			}
		}
	}
	// Changes to people and repairs are not part of the demo world: say so
	// instead of a success that changes nothing.
	if method == "PATCH" || method == "DELETE" || strings.HasSuffix(route, "/repair") {
		return errors.New("not available in the demo")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var v any
	now := time.Now()
	parts := strings.Split(strings.TrimPrefix(route, "/v1/"), "/")
	switch {
	case method == "GET" && route == "/v1/status":
		v = d.status()
	case method == "GET" && route == "/v1/platforms":
		v = d.platforms()
	case method == "GET" && route == "/v1/info":
		v = protocol.Info{Version: "v0.2.0"}
	case method == "GET" && route == "/v1/me":
		v = protocol.Me{ServerID: "demo-7f3a", User: "sam", Role: protocol.RoleAdmin, Computer: "sam-laptop"}
	case method == "GET" && route == "/v1/users":
		var us []protocol.User
		for i, u := range demoUsers {
			role := protocol.RoleMember
			if u == "sam" {
				role = protocol.RoleAdmin
			}
			us = append(us, protocol.User{Name: u, Role: role, Computers: []string{u + "-laptop"}, CreatedAt: now.AddDate(0, 0, -30+i*4)})
		}
		v = us
	case method == "GET" && parts[0] == "rooms":
		v = d.details(strings.Join(parts[1:], "/"), now)
	case method == "POST" && route == "/v1/invites":
		v = protocol.InviteResponse{Code: "hostel1-demo-not-a-real-code", ExpiresAt: now.Add(24 * time.Hour)}
	case method == "POST" && route == "/v1/machines/invites":
		v = protocol.MachineInviteResponse{Machine: "home/new", Code: "demo", Unix: "curl -fsSL https://demo.hostel.example/v1/install.sh | sh -s -- demo",
			Windows: "irm https://demo.hostel.example/v1/install.ps1 | iex", ExpiresAt: now.Add(time.Hour)}
	case method == "POST" && parts[0] == "leases" && len(parts) == 3:
		d.leaseAction(parts[1], parts[2], in)
		d.positions(now)
		d.bump()
	case method == "POST" && parts[0] == "rooms" && len(parts) == 3:
		id, _ := strconv.ParseInt(parts[1], 10, 64)
		if r := d.room(id); r != nil {
			switch parts[2] {
			case "drain":
				r.Drained = true
			case "undrain":
				r.Drained = false
			}
			d.bump()
		}
	}
	if v == nil || out == nil {
		return nil
	}
	// Through JSON, as from a server: the app gets its own copies.
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func (d *demoServer) leaseAction(id, action string, in any) {
	i := slices.IndexFunc(d.leases, func(l protocol.LeaseStatus) bool { return l.ID == id })
	if i < 0 {
		return
	}
	l := &d.leases[i]
	switch action {
	case "release":
		if l.State == protocol.LeaseQueued {
			d.leases = slices.Delete(d.leases, i, i+1)
			return
		}
		l.State = protocol.LeaseCleaning
		d.endAt[l.ID] = time.Now().Add(3 * time.Second)
	case "priority":
		if r, ok := in.(protocol.PriorityRequest); ok {
			l.Priority = r.Priority
		}
	case "move":
		if r, ok := in.(protocol.MoveRequest); ok {
			if j := slices.IndexFunc(d.leases, func(x protocol.LeaseStatus) bool { return x.ID == r.Before }); j >= 0 {
				l.Priority, l.CreatedAt = d.leases[j].Priority, d.leases[j].CreatedAt.Add(-time.Millisecond)
			}
		}
	}
}

func (d *demoServer) details(name string, now time.Time) protocol.RoomDetails {
	i := slices.IndexFunc(d.rooms, func(r protocol.RoomStatus) bool { return r.Name == name })
	if i < 0 {
		return protocol.RoomDetails{}
	}
	r := d.rooms[i]
	out := protocol.RoomDetails{Room: r, Leases: roomLeases(d.status(), r.ID), Jobs: map[string][]protocol.JobInfo{}}
	for _, l := range out.Leases {
		if l.State == protocol.LeaseReady {
			out.Jobs[l.ID] = []protocol.JobInfo{{ID: "job-1", Command: []string{"xcodebuild", "test", "-scheme", "App"}, State: "running", StartedAt: now.Add(-90 * time.Second)}}
		}
	}
	if r.State == protocol.RoomRunning && r.Hardware != nil {
		used := 0.35 + 0.4*d.rnd.Float64()
		out.Usage = &protocol.Usage{CPUPercent: 10 + 70*d.rnd.Float64(), MemoryUsedBytes: int64(used * float64(r.Hardware.MemoryBytes)), MemoryTotalBytes: r.Hardware.MemoryBytes}
	}
	return out
}
