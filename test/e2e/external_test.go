//go:build e2e

package e2e

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
)

const smallMachine = `
[providers.cloud.machines.small]
os = "linux"
spec = { image = "room" }
platforms = ["fakedevice"]
max_rooms = 2
`

// newExternalFixture starts a server with the fake external provider "cloud".
func newExternalFixture(t *testing.T, serverKeys, providerKeys string, vars map[string]string, machines string) *fixture {
	t.Helper()
	e := newEnv(t)
	e.init()
	e.usePlatform("fakedevice")
	e.writeConfig(serverHeader() + serverKeys + e.externalProvider("cloud", providerKeys, vars, machines))
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	e.useGuestToken()
	e.newRepo(defaultRepoFiles())
	return &fixture{e: e, s: s}
}

// cloudRooms returns the rooms of the cloud provider that are not destroyed.
func (f *fixture) cloudRooms() []protocol.RoomStatus {
	var out []protocol.RoomStatus
	for _, r := range f.s.status().Rooms {
		if r.Provider == "cloud" {
			out = append(out, r)
		}
	}
	return out
}

// roomSh runs a shell command in a fake provider room.
func (f *fixture) roomSh(room, cmd string) string {
	f.e.t.Helper()
	out, err := docker("exec", "-u", "hostel", room, "sh", "-c", cmd)
	if err != nil {
		f.e.t.Fatal(err)
	}
	return out
}

func (f *fixture) waitNoCloudRooms() {
	f.e.t.Helper()
	f.e.waitFor("cloud rooms destroyed", 2*time.Minute, func() bool {
		return len(f.cloudRooms()) == 0 && len(f.e.fakeRooms()) == 0
	})
}

// TestExternalProvider is scenarios 18 and 22 (on-demand): creation on demand,
// a platform provision only at the first use, warm reuse, provision again after
// a script change, and the access through a proxy command.
func TestExternalProvider(t *testing.T) {
	t.Parallel()
	f := newExternalFixture(t, "", `warm_running = "10m"`, map[string]string{"FAKE_PROXY": "1"}, smallMachine)
	if rooms := f.cloudRooms(); len(rooms) != 0 {
		t.Fatalf("rooms before a lease: %+v", rooms)
	}
	f.acquire()
	rooms := f.cloudRooms()
	if len(rooms) != 1 || rooms[0].State != protocol.RoomRunning || !strings.HasPrefix(rooms[0].Name, "hostel-e2e") {
		t.Fatalf("rooms after the first lease: %+v", rooms)
	}
	room := rooms[0].Name
	// The provider deadline covers a full lease: max_lease (2h) + warm_running
	// (10m) + 10m at least.
	if d := time.Until(rooms[0].Deadline); d < 2*time.Hour+19*time.Minute {
		t.Fatalf("provider deadline in %s, want at least 2h19m", d)
	}
	log := "/home/hostel/hostel/cache/fakedevice.log"
	if got := f.roomSh(room, "grep -c '^provision linux' "+log); got != "1" {
		t.Fatalf("provision ran %s times, want 1", got)
	}
	f.e.mustHostel("release")
	f.e.waitFor("warm room", time.Minute, func() bool {
		r := f.cloudRooms()
		return len(r) == 1 && r[0].Slots[0].State == protocol.SlotFree
	})
	// Warm reuse: the same room, no second provision.
	f.acquire()
	if rooms := f.cloudRooms(); len(rooms) != 1 || rooms[0].Name != room {
		t.Fatalf("rooms after the second lease: %+v", rooms)
	}
	if got := f.roomSh(room, "grep -c '^provision linux' "+log); got != "1" {
		t.Fatalf("provision ran %s times after warm reuse, want 1", got)
	}
	f.e.mustHostel("release")
	f.waitSlotFreeOn(room)
	// Changed platform scripts: provision runs again when the next lease uses the room.
	f.e.writePlatform("fakedevice", map[string]string{"provision.linux.sh": "#!/bin/sh\necho \"provision linux changed\" >> \"$HOSTEL_CACHE_DIR/fakedevice.log\"\n"})
	f.e.pushPlatform("fakedevice")
	f.acquire()
	if got := f.roomSh(room, "grep -c '^provision linux changed' "+log); got != "1" {
		t.Fatalf("provision after a script change ran %s times, want 1", got)
	}
	if n := len(f.e.fakeRooms()); n != 1 {
		t.Fatalf("%d provider rooms, want 1", n)
	}
}

func (f *fixture) waitSlotFreeOn(room string) {
	f.e.t.Helper()
	f.e.waitFor("slot of "+room+" free", time.Minute, func() bool {
		for _, r := range f.s.status().Rooms {
			if r.Name == room {
				return r.Slots[0].State == protocol.SlotFree && r.Slots[0].LeaseID == ""
			}
		}
		return false
	})
}

// TestExternalMacOS is scenario 22 (script selection by OS): an external machine
// with os = "macos" runs the .macos.sh scripts in a Linux container.
func TestExternalMacOS(t *testing.T) {
	t.Parallel()
	f := newExternalFixture(t, "", "", nil, strings.Replace(smallMachine, `os = "linux"`, `os = "macos"`, 1))
	f.acquire()
	r := f.e.mustHostel("run", "--", "echo $FAKE_DEVICE_OS $HOSTEL_ROOM_OS $(uname -s)")
	if r.stdout != "macos macos Linux\n" {
		t.Fatalf("scripts by OS: %q", r.stdout)
	}
	room := f.cloudRooms()[0].Name
	if got := f.roomSh(room, "cat /home/hostel/hostel/cache/fakedevice.log"); !strings.Contains(got, "provision macos") || strings.Contains(got, "linux") {
		t.Fatalf("platform scripts: %q", got)
	}
}

// TestWarmDestroy is scenario 18: a room of a provider that cannot stop is
// destroyed after warm_running.
func TestWarmDestroy(t *testing.T) {
	t.Parallel()
	f := newExternalFixture(t, "", `warm_running = "2s"`, nil, smallMachine)
	f.acquire()
	f.e.mustHostel("release")
	f.waitNoCloudRooms()
}

// TestRemoveOnDemandMachine (M9): an on-demand machine with a room cannot be
// removed (the provider destroys a room through its machine's settings); the
// admin destroys the free room, and then removes the machine.
func TestRemoveOnDemandMachine(t *testing.T) {
	t.Parallel()
	f := newExternalFixture(t, "", `warm_running = "10m"`, nil, smallMachine)
	f.acquire()
	room := f.cloudRooms()[0].Name
	f.e.mustHostel("release")
	f.waitSlotFreeOn(room)
	if r := f.e.adminHostel("config", "rooms", "remove", "cloud/small"); r.code != 1 || !strings.Contains(r.stderr, "destroy it first (hostel config rooms destroy "+room) {
		t.Fatalf("remove an on-demand machine with a room: %s", r)
	}
	if r := f.e.adminHostel("config", "rooms", "destroy", room); r.code != 0 {
		t.Fatalf("destroy the room: %s", r)
	}
	f.waitNoCloudRooms()
	if r := f.e.adminHostel("config", "rooms", "remove", "cloud/small"); r.code != 0 {
		t.Fatalf("remove the machine without rooms: %s", r)
	}
}

// TestStopResume: a room of a provider that can stop is stopped after
// warm_running and resumed for the next lease.
func TestStopResume(t *testing.T) {
	t.Parallel()
	f := newExternalFixture(t, "", `warm_running = "2s"`, map[string]string{"FAKE_CAN_STOP": "1"}, smallMachine)
	f.acquire()
	room := f.cloudRooms()[0].Name
	f.e.mustHostel("release")
	f.e.waitFor("room stopped", time.Minute, func() bool {
		r := f.cloudRooms()
		return len(r) == 1 && r[0].State == protocol.RoomStopped
	})
	if state, _ := docker("inspect", "-f", "{{.State.Status}}", room); state != "exited" {
		t.Fatalf("container state %q, want exited", state)
	}
	f.acquire()
	if r := f.cloudRooms(); len(r) != 1 || r[0].Name != room || r[0].State != protocol.RoomRunning {
		t.Fatalf("rooms after resume: %+v", r)
	}
}

// TestLimitRetry is scenarios 18 and 19: a provider limit error keeps the lease
// queued, and the retry after limit_retry works; reconcile does not touch a
// room without the prefix.
func TestLimitRetry(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	foreign := "foreign-" + e.runID
	if _, err := docker("run", "-d", "--name", foreign, "--label", dockerLabel+"="+e.runID, "--label", "hostel-fake=1", roomImage); err != nil {
		t.Fatal(err)
	}
	e.init()
	e.usePlatform("fakedevice")
	e.writeConfig(serverHeader() + "limit_retry = \"3s\"\n" + e.externalProvider("cloud", "", map[string]string{"FAKE_LIMIT": "1"}, smallMachine))
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	e.useGuestToken()
	e.newRepo(defaultRepoFiles())
	f := &fixture{e: e, s: s}
	r := e.hostel("acquire", "fakedevice", "--task", "limited", "--wait", "6s")
	if r.code != 75 || !strings.Contains(r.stderr, "provider limit reached") {
		t.Fatalf("acquire at the provider limit: %s", r)
	}
	if !slices.Contains(e.fakeRooms(), foreign) {
		t.Fatal("reconcile removed a room without the prefix")
	}
	if _, err := docker("rm", "-f", foreign); err != nil {
		t.Fatal(err)
	}
	f.acquire()
}

// TestProviderFailures is scenario 18: three failed provider attempts fail the
// lease with the provider message; fewer failures are retried.
func TestProviderFailures(t *testing.T) {
	t.Parallel()
	t.Run("retried", func(t *testing.T) {
		t.Parallel()
		f := newExternalFixture(t, "", "", map[string]string{"FAKE_FAIL": "2"}, smallMachine)
		f.acquire()
	})
	t.Run("three failures", func(t *testing.T) {
		t.Parallel()
		f := newExternalFixture(t, "", "", map[string]string{"FAKE_FAIL": "3"}, smallMachine)
		r := f.e.hostel("acquire", "fakedevice", "--task", "fails", "--wait", "60s")
		if r.code != 1 || !strings.Contains(r.stderr, "injected failure 3 of 3") {
			t.Fatalf("acquire with three provider failures: %s", r)
		}
		if n := len(f.e.fakeRooms()); n != 0 {
			t.Fatalf("%d provider rooms after the failures", n)
		}
	})
}

// TestFresh is scenarios 18 and 23: --fresh gets a new room and evicts an idle
// warm room at the machine limit.
func TestFresh(t *testing.T) {
	t.Parallel()
	f := newExternalFixture(t, "", `warm_running = "10m"`, nil, strings.Replace(smallMachine, "max_rooms = 2", "max_rooms = 1", 1))
	f.acquire()
	first := f.cloudRooms()[0].Name
	f.e.mustHostel("release")
	f.waitSlotFreeOn(first)
	f.acquire("--fresh")
	rooms := f.cloudRooms()
	if len(rooms) != 1 || rooms[0].Name == first {
		t.Fatalf("rooms after --fresh: %+v (first room %s)", rooms, first)
	}
	if slices.Contains(f.e.fakeRooms(), first) {
		t.Fatal("the evicted room still exists at the provider")
	}
}

// TestLimits is scenario 23.
func TestLimits(t *testing.T) {
	t.Parallel()
	two := `
[providers.cloud.machines.a]
os = "linux"
spec = { image = "room" }
platforms = ["fakedevice"]
max_rooms = 2

[providers.cloud.machines.b]
os = "linux"
spec = { image = "room" }
platforms = ["fakedevice"]
max_rooms = 2
`
	t.Run("provider max_rooms", func(t *testing.T) {
		t.Parallel()
		f := newExternalFixture(t, "", "max_rooms = 2", nil, two)
		f.acquire()
		f.e.newRepoNamed("second", defaultRepoFiles())
		f.acquire()
		f.e.newRepoNamed("third", defaultRepoFiles())
		r := f.e.hostel("acquire", "fakedevice", "--task", "third", "--wait", "3s")
		if r.code != 75 || !strings.Contains(r.stderr, "room limit reached") {
			t.Fatalf("third lease: %s", r)
		}
		if n := len(f.e.fakeRooms()); n != 2 {
			t.Fatalf("%d rooms, want 2", n)
		}
	})
	t.Run("limits entry", func(t *testing.T) {
		t.Parallel()
		f := newExternalFixture(t, "", `limits = [{ machines = ["a"], max_rooms = 1 }]`, nil, two)
		f.acquire()
		f.e.newRepoNamed("second", defaultRepoFiles())
		r := f.e.mustHostel("acquire", "fakedevice", "--task", "second")
		if !strings.Contains(r.stderr, "on cloud/b") {
			t.Fatalf("the second lease is not on machine b: %s", r)
		}
	})
	t.Run("evict after a limit error", func(t *testing.T) {
		t.Parallel()
		f := newExternalFixture(t, "", `warm_running = "10m"`, map[string]string{"FAKE_LIMIT": "1"}, smallMachine)
		f.acquire()
		first := f.cloudRooms()[0].Name
		f.e.mustHostel("release")
		f.waitSlotFreeOn(first)
		// The machine allows 2 rooms, so --fresh asks for a new one; the
		// provider refuses (its own limit), and the server evicts the idle room.
		f.acquire("--fresh", "--wait", "90s")
		if rooms := f.e.fakeRooms(); len(rooms) != 1 || rooms[0] == first {
			t.Fatalf("provider rooms after the eviction: %v (first %s)", rooms, first)
		}
	})
}

// TestReconcile is scenario 19.
func TestReconcile(t *testing.T) {
	t.Parallel()
	f := newExternalFixture(t, "", "", nil, smallMachine)
	id := f.acquire()
	// A room with this server's prefix that the database does not know.
	var sid string
	for _, r := range f.cloudRooms() {
		sid = strings.Split(r.Name, "-")[1]
	}
	unknown := fmt.Sprintf("hostel-%s-999", sid)
	if _, err := docker("run", "-d", "--name", unknown, "--label", dockerLabel+"="+f.e.runID, "--label", "hostel-fake=1", roomImage); err != nil {
		t.Fatal(err)
	}
	f.e.waitFor("unknown room destroyed", time.Minute, func() bool { return !slices.Contains(f.e.fakeRooms(), unknown) })
	// The provider loses the room of a ready lease.
	if _, err := docker("rm", "-f", f.cloudRooms()[0].Name); err != nil {
		t.Fatal(err)
	}
	f.e.waitFor("lease failed", time.Minute, func() bool {
		var l protocol.Lease
		f.s.get("/v1/leases/"+id, f.s.token, &l)
		return l.State == protocol.LeaseFailed && strings.Contains(l.EndReason, "room lost")
	})
	if rooms := f.cloudRooms(); len(rooms) != 0 {
		t.Fatalf("rooms after the loss: %+v", rooms)
	}
}

// TestEarlyCancel is scenario 26: a lease that is cancelled while its room is in
// creation frees its slot without a helper call; the room becomes warm.
func TestEarlyCancel(t *testing.T) {
	t.Parallel()
	// The room creation takes 20 s, so the release comes during creation also on
	// a slow machine.
	f := newExternalFixture(t, "", `warm_running = "10m"`, map[string]string{"FAKE_DELAY": "20s"}, smallMachine)
	f.acquireUntil(protocol.LeaseProvisioning)
	f.e.mustHostel("release")
	f.e.waitFor("warm room", 2*time.Minute, func() bool {
		rooms := f.cloudRooms()
		return len(rooms) == 1 && rooms[0].State == protocol.RoomRunning && rooms[0].Slots[0].State == protocol.SlotFree
	})
	room := f.cloudRooms()[0].Name
	if got := f.roomSh(room, "test -e /home/hostel/hostel/slots && echo used || echo unused"); got != "unused" {
		t.Fatalf("the cancelled lease used the room: %s", got)
	}
}
