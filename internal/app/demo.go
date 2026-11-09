package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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
// machines get ready, the waiting line moves, leases end. It does not read or
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

	templates []protocol.TemplateStatus
	machines  []protocol.MachineStatus
	leases    []protocol.LeaseStatus
	// readyAt is when a machine or a lease in preparation is ready; endAt is
	// when a ready lease ends or a cleaning lease is gone.
	readyAt map[string]time.Time
	endAt   map[string]time.Time
	nextID  int
	nextArr time.Time
}

// demoTool is a tool of the demo world; options are those of a screen tool
// (the versions of a software tool are in the inventories).
type demoTool struct {
	name, desc, guide string
	screen, whole     bool
	options           []string
	mise              bool
	typical           time.Duration
}

var demoTools = []demoTool{
	{name: "android-emulator", desc: "Android emulator: one clean emulator for each lease", screen: true, options: []string{"pixel-9", "pixel-tablet"}, typical: 18 * time.Minute,
		guide: "Android emulators: each lease gets new, clean emulators (one for each option, for example pixel-9 or pixel-tablet). The SDK comes with it."},
	{name: "ios-simulator", desc: "iOS simulators: new, clean simulators for each lease", screen: true, options: []string{"iphone-17-pro", "ipad-pro-13"}, typical: 24 * time.Minute,
		guide: "iOS simulators: each lease gets new, clean simulators (one for each option, for example iphone-17-pro or ipad-pro-13). Xcode comes with it."},
	{name: "macos-desktop", desc: "The macOS desktop, for Mac apps", screen: true, whole: true, typical: 12 * time.Minute,
		guide: "The macOS desktop, for Mac apps (also Mac Catalyst apps). The lease takes the whole machine."},
	{name: "windows-desktop", desc: "The Windows desktop, for Windows apps", screen: true, whole: true, typical: 15 * time.Minute,
		guide: "The Windows desktop, for Windows apps. The lease takes the whole machine."},
	{name: "android-sdk", desc: "Android SDK: build tools, platform tools, and the emulator binaries", typical: 9 * time.Minute,
		guide: "The Android SDK, for builds. Java comes from the jdk tool."},
	{name: "dotnet", mise: true, typical: 10 * time.Minute},
	{name: "jdk", mise: true, typical: 9 * time.Minute},
	{name: "node", mise: true, typical: 7 * time.Minute},
	{name: "xcode", desc: "Xcode, for builds of Apple apps", typical: 14 * time.Minute,
		guide: "Xcode, for builds of Apple apps. Ask for ios-simulator or macos-desktop only when you must see the app."},
}

// demoVersions are the options that the inventories report.
var demoVersions = map[string][]string{
	"android-sdk": {"36"}, "jdk": {"21.0.5"}, "xcode": {"26.1"}, "dotnet": {"9.0.100"}, "node": {"22.11.0"},
	"android-emulator": {"pixel-9", "pixel-tablet"}, "ios-simulator": {"iphone-17-pro", "ipad-pro-13"},
}

// demoRequests are the tools that the demo leases ask for.
var demoRequests = [][]string{
	{"ios-simulator:iphone-17-pro"}, {"ios-simulator:ipad-pro-13"}, {"android-emulator:pixel-9"}, {"android-sdk", "jdk"},
	{"xcode"}, {"xcode", "macos-desktop"}, {"dotnet", "windows-desktop"}, {"node:22"},
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
	android := []string{"android-sdk", "android-emulator", "jdk:21", "node:22"}
	d.templates = []protocol.TemplateStatus{
		{Key: "home/macbook", Kind: "static", OS: "macos", Tools: []string{"xcode", "ios-simulator", "android-sdk", "android-emulator", "jdk:21", "macos-desktop"},
			MaxMachines: 1, Connect: true, ConnectVersion: "v0.3.0"},
		{Key: "home/win", Kind: "static", OS: "windows", Tools: []string{"windows-desktop", "dotnet:9"}, MaxMachines: 1, Connect: true, ConnectVersion: "v0.3.0"},
		{Key: "office/mini", Kind: "static", OS: "macos", Tools: []string{"xcode", "ios-simulator", "macos-desktop"}, MaxMachines: 1, Connect: true, ConnectVersion: "v0.2.0"},
		{Key: "lab/linux", Kind: "static", OS: "linux", Tools: android, MaxMachines: 1},
		{Key: "ns/linux", Kind: "namespace", OS: "linux", Tools: android, MaxMachines: 2, Hardware: &protocol.Hardware{Cores: 8, MemoryBytes: 16 << 30}},
	}
	hw := func(cpu string, cores int, mem int64, osv string) *protocol.Hardware {
		return &protocol.Hardware{CPU: cpu, Cores: cores, MemoryBytes: mem << 30, OSVersion: osv}
	}
	d.machines = []protocol.MachineStatus{
		{ID: 1, Name: "home-macbook", Provider: "home", Template: "macbook", OS: "macos", Arch: "arm64", State: protocol.MachineRunning,
			Tools: inventory(d.templates[0].Tools...), Hardware: hw("Apple M3 Pro", 12, 36, "macOS 26.1"),
			Rooms: []protocol.RoomStatus{{Name: "room-1", State: protocol.RoomFree}, {Name: "room-2", State: protocol.RoomFree}}},
		{ID: 2, Name: "home-win", Provider: "home", Template: "win", OS: "windows", Arch: "amd64", State: protocol.MachineRunning,
			Tools: inventory(d.templates[1].Tools...), Hardware: hw("Intel Xeon", 4, 16, "Windows 11"),
			Rooms: []protocol.RoomStatus{{Name: "room-1", State: protocol.RoomFree}}},
		{ID: 3, Name: "office-mini", Provider: "office", Template: "mini", OS: "macos", Arch: "arm64", State: protocol.MachineRunning,
			Tools: inventory(d.templates[2].Tools...), Hardware: hw("Apple M2", 8, 16, "macOS 26.0"),
			Rooms: []protocol.RoomStatus{{Name: "room-1", State: protocol.RoomFree}}},
		{ID: 4, Name: "lab-linux", Provider: "lab", Template: "linux", OS: "linux", Arch: "amd64", State: protocol.MachineBroken,
			Reason: "unreachable", LastSeen: now.Add(-3 * time.Hour), Tools: inventory(android...),
			Rooms: []protocol.RoomStatus{{Name: "room-1", State: protocol.RoomBroken}}},
	}
	d.nextID = 10
	// A busy start: two guests in machines, one getting ready, two waiting.
	d.place(d.newLease("ana", []string{"ios-simulator:iphone-17-pro"}, "login flow", "normal", now.Add(-21*time.Minute)), 1, "room-1", protocol.LeaseReady, now.Add(-18*time.Minute))
	d.place(d.newLease("dani", []string{"dotnet", "windows-desktop"}, "release build", "normal", now.Add(-9*time.Minute)), 2, "room-1", protocol.LeaseReady, now.Add(-7*time.Minute))
	d.place(d.newLease("lee", []string{"ios-simulator:ipad-pro-13"}, "iPad layout", "high", now.Add(-40*time.Second)), 3, "room-1", protocol.LeasePreparing, time.Time{})
	d.readyAt[d.leases[2].ID] = now.Add(3 * time.Second)
	d.queue(d.newLease("mo", []string{"dotnet", "windows-desktop"}, "settings sync", "normal", now.Add(-4*time.Minute)))
	d.queue(d.newLease("sam", []string{"windows-desktop"}, "crash on resume", "low", now.Add(-1*time.Minute)))
	d.endAt[d.leases[0].ID] = now.Add(40 * time.Second)
	d.endAt[d.leases[1].ID] = now.Add(9 * time.Second)
	d.positions(now)
	go d.run()
	return d
}

// inventory is a machine inventory of these tool entries, in name order.
func inventory(entries ...string) []protocol.InventoryTool {
	var out []protocol.InventoryTool
	for _, e := range entries {
		name, _, _ := strings.Cut(e, ":")
		i := slices.IndexFunc(demoTools, func(t demoTool) bool { return t.name == name })
		out = append(out, protocol.InventoryTool{Name: name, Options: demoVersions[name], State: "ready", Screen: i >= 0 && demoTools[i].screen})
	}
	slices.SortFunc(out, func(a, b protocol.InventoryTool) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func (d *demoServer) newLease(user string, tools []string, task, prio string, created time.Time) *protocol.LeaseStatus {
	d.nextID++
	repo := "github.com/example/app"
	return &protocol.LeaseStatus{ID: "lease-" + strconv.Itoa(d.nextID), Tools: tools, Task: task, Priority: prio,
		User: user, Guest: user + "-laptop", Repo: repo, Branch: strings.ReplaceAll(task, " ", "-"), CreatedAt: created}
}

func (d *demoServer) place(l *protocol.LeaseStatus, machineID int64, room, state string, ready time.Time) {
	r := d.machine(machineID)
	l.State, l.MachineID, l.Room, l.Provider, l.Template, l.ReadyAt = state, machineID, room, r.Provider, r.Template, ready
	for i := range r.Rooms {
		if r.Rooms[i].Name == room {
			r.Rooms[i].State, r.Rooms[i].LeaseID = protocol.RoomLeased, l.ID
		}
	}
	d.leases = append(d.leases, *l)
}

func (d *demoServer) queue(l *protocol.LeaseStatus) {
	l.State = protocol.LeaseQueued
	d.leases = append(d.leases, *l)
}

func (d *demoServer) machine(id int64) *protocol.MachineStatus {
	for i := range d.machines {
		if d.machines[i].ID == id {
			return &d.machines[i]
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
	for i := range d.machines {
		r := &d.machines[i]
		if t, ok := d.readyAt[r.Name]; ok && now.After(t) {
			delete(d.readyAt, r.Name)
			r.State = protocol.MachineRunning
			changed = true
		}
	}
	var keep []protocol.LeaseStatus
	for _, l := range d.leases {
		switch l.State {
		case protocol.LeaseAssigned, protocol.LeasePreparing:
			r := d.machine(l.MachineID)
			if t, ok := d.readyAt[l.ID]; ok && now.After(t) && r.State == protocol.MachineRunning {
				delete(d.readyAt, l.ID)
				l.State, l.ReadyAt = protocol.LeaseReady, now
				d.endAt[l.ID] = now.Add(time.Duration(25+d.rnd.IntN(50)) * time.Second)
				changed = true
			} else if r.State == protocol.MachineRunning && l.State == protocol.LeaseAssigned {
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
		tools := demoRequests[d.rnd.IntN(len(demoRequests))]
		prio := []string{"normal", "normal", "normal", "high", "low"}[d.rnd.IntN(5)]
		d.queue(d.newLease(demoUsers[d.rnd.IntN(len(demoUsers))], tools, demoTasks[d.rnd.IntN(len(demoTasks))], prio, now))
		d.nextArr = now.Add(time.Duration(8+d.rnd.IntN(14)) * time.Second)
		changed = true
	}
	if d.schedule(now) {
		changed = true
	}
	d.positions(now)
	return changed
}

// free gives the lease's room back; an on-demand machine without leases goes.
func (d *demoServer) free(l protocol.LeaseStatus) {
	r := d.machine(l.MachineID)
	if r == nil {
		return
	}
	for i := range r.Rooms {
		if r.Rooms[i].LeaseID == l.ID {
			r.Rooms[i] = protocol.RoomStatus{Name: r.Rooms[i].Name, State: protocol.RoomFree}
		}
	}
	if r.Provider == "ns" {
		d.machines = slices.DeleteFunc(d.machines, func(x protocol.MachineStatus) bool { return x.ID == r.ID })
	}
}

// toolNames are the names of tool entries ("jdk:21" is jdk).
func toolNames(entries []string) []string {
	var out []string
	for _, e := range entries {
		name, _, _ := strings.Cut(e, ":")
		out = append(out, name)
	}
	return out
}

// wholeMachine reports a lease with a tool that takes the whole machine.
func wholeMachine(tools []string) bool {
	return slices.ContainsFunc(demoTools, func(t demoTool) bool { return t.whole && slices.Contains(toolNames(tools), t.name) })
}

// fits reports whether a running machine can take a lease for the tools now:
// one free room, or all rooms for a lease that takes the whole machine.
func fits(r protocol.MachineStatus, tools []string) bool {
	for _, t := range toolNames(tools) {
		if !freeRoom(r, t) {
			return false
		}
	}
	if !freeRoom(r, "") {
		return false
	}
	return !wholeMachine(tools) || !slices.ContainsFunc(r.Rooms, func(s protocol.RoomStatus) bool { return s.State != protocol.RoomFree })
}

// schedule puts queued leases on free rooms, first by priority, then by age;
// an on-demand machine makes a new machine.
func (d *demoServer) schedule(now time.Time) bool {
	changed := false
	for _, l := range d.ordered() {
		i := slices.IndexFunc(d.leases, func(x protocol.LeaseStatus) bool { return x.ID == l.ID })
		lease := &d.leases[i]
		placed := false
		for ri := range d.machines {
			r := &d.machines[ri]
			if !fits(*r, l.Tools) {
				continue
			}
			for si := range r.Rooms {
				if r.Rooms[si].State != protocol.RoomFree {
					continue
				}
				if !placed {
					lease.State, lease.MachineID, lease.Room, lease.Provider, lease.Template = protocol.LeasePreparing, r.ID, r.Rooms[si].Name, r.Provider, r.Template
					d.readyAt[l.ID] = now.Add(time.Duration(4+d.rnd.IntN(5)) * time.Second)
				} else if !wholeMachine(l.Tools) {
					break
				}
				r.Rooms[si].State, r.Rooms[si].LeaseID = protocol.RoomLeased, l.ID
				placed = true
			}
			break
		}
		if !placed {
			ns := d.templates[4]
			if !slices.ContainsFunc(toolNames(l.Tools), func(t string) bool { return !slices.Contains(toolNames(ns.Tools), t) }) &&
				len(templateMachines(protocol.Status{Machines: d.machines}, ns.Key)) < ns.MaxMachines {
				d.nextID++
				name := "ns-linux-" + strconv.Itoa(d.nextID)
				d.machines = append(d.machines, protocol.MachineStatus{ID: int64(d.nextID), Name: name, Provider: "ns", Template: "linux", OS: "linux", Arch: "amd64",
					State: protocol.MachineProvisioning, Tools: inventory(ns.Tools...), Deadline: now.Add(2 * time.Hour),
					Hardware: &protocol.Hardware{CPU: "AMD EPYC", Cores: 8, MemoryBytes: 16 << 30, OSVersion: "Ubuntu 24.04"},
					Rooms:    []protocol.RoomStatus{{Name: "room-1", State: protocol.RoomLeased, LeaseID: l.ID}}})
				d.readyAt[name] = now.Add(8 * time.Second)
				d.readyAt[l.ID] = now.Add(11 * time.Second)
				lease.State, lease.MachineID, lease.Room, lease.Provider, lease.Template = protocol.LeaseAssigned, int64(d.nextID), "room-1", "ns", "linux"
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

// positions numbers the waiting line (one for all leases) and estimates the
// wait: the first free machine with the tools, and 5 minutes for each earlier
// lease that asks for one of the same tools.
func (d *demoServer) positions(now time.Time) {
	q := d.ordered()
	for pos, ql := range q {
		behind := 0
		for _, earlier := range q[:pos] {
			if slices.ContainsFunc(toolNames(earlier.Tools), func(t string) bool { return slices.Contains(toolNames(ql.Tools), t) }) {
				behind++
			}
		}
		for i := range d.leases {
			if d.leases[i].ID == ql.ID {
				d.leases[i].Position = pos + 1
				d.leases[i].EstimatedWaitSeconds = d.wait(ql.Tools, now) + behind*300
				d.leases[i].Reason = "every machine with " + strings.Join(toolNames(ql.Tools), " and ") + " is in use"
			}
		}
	}
}

// wait is the time until the first machine with the tools is free.
func (d *demoServer) wait(tools []string, now time.Time) int {
	best := -1
	for _, l := range d.leases {
		if l.State == protocol.LeaseQueued || slices.ContainsFunc(toolNames(tools), func(t string) bool { return !machineHas(*d.machine(l.MachineID), t) }) {
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
		Providers: []protocol.ProviderStatus{{Name: "home", Label: "Home", Kind: "static"}, {Name: "office", Label: "Office", Kind: "static"},
			{Name: "lab", Label: "Lab", Kind: "static"}, {Name: "ns", Label: "Namespace", Kind: "namespace", MaxMachines: 2}},
		Templates: slices.Clone(d.templates), Machines: slices.Clone(d.machines), Leases: slices.Clone(d.leases)}
}

// tools is GET /v1/tools of the demo world: screen tools first.
func (d *demoServer) tools() []protocol.Tool {
	st := d.status()
	var out []protocol.Tool
	for _, dt := range demoTools {
		t := protocol.Tool{Name: dt.name, Screen: dt.screen, Mise: dt.mise, Description: dt.desc, Guide: dt.guide, WholeMachine: dt.whole,
			Options: dt.options, TypicalLeaseSeconds: int(dt.typical.Seconds())}
		if len(dt.options) > 1 {
			t.MaxOptions = len(dt.options)
		}
		if !dt.screen {
			t.Options = demoVersions[dt.name]
		}
		for _, ms := range d.templates {
			if slices.Contains(toolNames(ms.Tools), dt.name) {
				t.Templates = append(t.Templates, protocol.ToolTemplate{Key: ms.Key, OS: ms.OS, Options: demoVersions[dt.name], OnDemand: ms.Kind != "static", Online: true})
			}
		}
		for _, r := range d.machines {
			if freeRoom(r, dt.name) {
				t.FreeRooms++
			}
		}
		t.Waiting = len(queued(st, dt.name))
		if t.Waiting > 0 {
			t.EstimatedWaitSeconds = d.wait([]string{dt.name}, time.Now())
		}
		out = append(out, t)
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
	case method == "GET" && route == "/v1/tools":
		v = protocol.Tools{Tools: d.tools()}
	case method == "GET" && route == "/v1/info":
		v = protocol.Info{Version: "v0.3.0"}
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
	case method == "GET" && parts[0] == "usage":
		u := map[string]protocol.Usage{}
		for _, r := range d.machines {
			if r.State == protocol.MachineRunning {
				wave := math.Sin(float64(now.Unix())/20+float64(r.ID)) + 1
				u[r.Name] = protocol.Usage{CPUPercent: 15 + 38*wave, MemoryUsedBytes: int64((3 + 3*wave) * (1 << 30)), MemoryTotalBytes: 16 << 30}
			}
		}
		v = protocol.UsageList{Machines: u}
	case method == "GET" && parts[0] == "machines":
		v = d.details(strings.Join(parts[1:], "/"), now)
	case method == "POST" && route == "/v1/invites":
		v = protocol.InviteResponse{Code: "hostel1-demo-not-a-real-code", ExpiresAt: now.Add(24 * time.Hour)}
	case method == "POST" && route == "/v1/machines/invites":
		v = protocol.MachineInviteResponse{Template: "home/new", Code: "demo", Unix: "curl -fsSL https://demo.hostel.example/v1/install.sh | sh -s -- demo",
			Windows: "irm https://demo.hostel.example/v1/install.ps1 | iex", ExpiresAt: now.Add(time.Hour)}
	case method == "POST" && parts[0] == "leases" && len(parts) == 3:
		d.leaseAction(parts[1], parts[2], in)
		d.positions(now)
		d.bump()
	case method == "POST" && parts[0] == "machines" && len(parts) == 3:
		id, _ := strconv.ParseInt(parts[1], 10, 64)
		if r := d.machine(id); r != nil {
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

func (d *demoServer) details(name string, now time.Time) protocol.MachineDetails {
	i := slices.IndexFunc(d.machines, func(r protocol.MachineStatus) bool { return r.Name == name })
	if i < 0 {
		return protocol.MachineDetails{}
	}
	r := d.machines[i]
	out := protocol.MachineDetails{Machine: r, Leases: machineLeases(d.status(), r.ID), Jobs: map[string][]protocol.JobInfo{}}
	for _, l := range out.Leases {
		if l.State == protocol.LeaseReady {
			out.Jobs[l.ID] = []protocol.JobInfo{{ID: "job-1", Command: []string{"xcodebuild", "test", "-scheme", "App"}, State: "running", StartedAt: now.Add(-90 * time.Second)}}
		}
	}
	if r.State == protocol.MachineRunning && r.Hardware != nil {
		used := 0.35 + 0.4*d.rnd.Float64()
		out.Usage = &protocol.Usage{CPUPercent: 10 + 70*d.rnd.Float64(), MemoryUsedBytes: int64(used * float64(r.Hardware.MemoryBytes)), MemoryTotalBytes: r.Hardware.MemoryBytes}
	}
	return out
}
