//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
	"golang.org/x/crypto/ssh"
)

// TestDirtySlotStaysDirty: a lease leaves a dirty slot on an unreachable room
// and gets a slot on another room; its later steps do not touch the dirty
// slot, which is cleaned when its room is back.
func TestDirtySlotStaysDirty(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.init()
	first := e.startStaticRoom("first")
	second := e.startStaticRoom("second")
	e.useTools("fake-sdk", "fake-screen")
	e.writeConfig(serverHeader() + `
[providers.home]
kind = "static"

[providers.home.machines.first]
address = "` + first.address() + `"
os = "linux"
tools = ["fake-screen"]

[providers.home.machines.second]
address = "` + second.address() + `"
os = "linux"
tools = ["fake-screen"]
`)
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	s.waitRoom("home/first", protocol.RoomRunning, roomReady)
	s.waitRoom("home/second", protocol.RoomRunning, roomReady)
	e.useGuestToken()
	// Machines are taken in configuration order: the holder gets "first", so
	// the tested lease starts on "second".
	f := &fixture{e: e, s: s, c: second}
	e.newRepoNamed("holder", defaultRepoFiles())
	if r := e.mustHostel("acquire", "fake-screen", "--task", "holder"); !strings.Contains(r.stderr, "on home/first") {
		t.Fatalf("holder: %s", r)
	}
	e.newRepo(defaultRepoFiles())
	f.marker("slow-start", true)
	if r := e.hostel("acquire", "fake-screen", "--task", "moves", "--wait", "3s"); r.code != 75 || !strings.Contains(r.stderr, "preparing on home/second") {
		t.Fatalf("acquire: %s", r)
	}
	// The lease began (the server sets began_at before start runs), so the
	// slot becomes dirty when the room is gone.
	start := "for p in /proc/[0-9]*; do tr '\\0' ' ' < $p/cmdline 2>/dev/null; echo; done | grep -c '^sleep 20 $' || true"
	f.e.waitFor("the slow start", time.Minute, func() bool { return second.sh(start) == "1" })
	second.stop()
	f.e.waitFor("dirty slot", time.Minute, func() bool { return s.room("home/second").Slots[0].State == protocol.SlotDirty })
	// The holder releases; the lease moves to the first room.
	f.useRepo("holder")
	e.mustHostel("release")
	f.useRepo("repo")
	if r := e.mustHostel("acquire", "fake-screen", "--task", "moves"); !strings.Contains(r.stderr, "on home/first") {
		t.Fatalf("the lease is not on home/first: %s", r)
	}
	e.mustHostel("release")
	f.e.waitFor("first slot free", time.Minute, func() bool { return s.room("home/first").Slots[0].State == protocol.SlotFree })
	if st := s.room("home/second").Slots[0].State; st != protocol.SlotDirty {
		t.Fatalf("the dirty slot is %s after the lease ended on another room", st)
	}
	second.start()
	f.marker("slow-start", false)
	f.e.waitFor("room back with a clean slot", 2*time.Minute, func() bool {
		r := s.room("home/second")
		return r.State == protocol.RoomRunning && r.Slots[0].State == protocol.SlotFree
	})
	// The cleanup of the dirty slot ran: the slot directory is gone. The stop
	// ran too: the start of fake-screen did not finish and can leave processes.
	if got := second.sh("test -e /home/hostel/hostel/slots/a && echo exists || echo removed"); got != "removed" {
		t.Fatalf("the slot directory of the dirty slot: %s", got)
	}
	if got := second.sh("grep -c '^stop linux' " + cacheDir + "/fake-screen.log || true"); got != "1" {
		t.Fatalf("stops on the dirty room: %s, want 1", got)
	}
}

// TestTwoSlotsKeepNoGuestKey: on a room with two slots, a key that lease A
// adds does not go into the base of lease B, so A's cleanup removes it.
func TestTwoSlotsKeepNoGuestKey(t *testing.T) {
	t.Parallel()
	f := newFixture(t, &fixtureOpts{machineKeys: "slots = [{ name = \"a\" }, { name = \"b\" }]\n"})
	base := f.c.sh("cat /home/hostel/.ssh/authorized_keys")
	f.acquire()
	f.e.mustHostel("run", "--", "echo 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGuestAddedKeyGuestAddedKeyGuestAddedK lease-a' >> ~/.ssh/authorized_keys")
	f.e.newRepoNamed("second", defaultRepoFiles())
	f.acquire()
	f.useRepo("repo")
	f.e.mustHostel("release")
	f.useRepo("second")
	f.e.mustHostel("release")
	f.e.waitFor("both slots free", time.Minute, func() bool {
		for _, sl := range f.s.room("home/box").Slots {
			if sl.State != protocol.SlotFree {
				return false
			}
		}
		return true
	})
	if got := f.c.sh("cat /home/hostel/.ssh/authorized_keys"); got != base {
		t.Fatalf("authorized_keys after both leases:\n%s\nwant:\n%s", got, base)
	}
}

// TestBrokenCloudRoomDestroyed: an on-demand room that a failed cleanup broke
// is destroyed (its broken slot keeps the ended lease's ID).
func TestBrokenCloudRoomDestroyed(t *testing.T) {
	t.Parallel()
	f := newExternalFixture(t, "", `warm_running = "10m"`, nil, smallMachine)
	f.acquire()
	room := f.cloudRooms()[0].Name
	f.roomSh(room, "touch "+cacheDir+"/fail-stop")
	f.e.mustHostel("release")
	f.waitNoCloudRooms()
	// A machine without a room keeps the size that its last room measured.
	if r := f.e.mustHostel("status", "cloud/small"); !regexp.MustCompile(`(?m)^cloud/small · on demand, up to \d+ rooms? · linux · \d+ cores, \d+ GB$`).MatchString(r.stdout) {
		t.Fatalf("status of a machine whose room is gone:\n%s", r.stdout)
	}
}

// TestTunnelsBeforeCreation: a lease whose tunnels cannot be placed fails
// before a room is created for it.
func TestTunnelsBeforeCreation(t *testing.T) {
	t.Parallel()
	f := newExternalFixture(t, "", "", nil, smallMachine)
	var cfg strings.Builder
	cfg.WriteString("[tunnels.fixed]\nlocal_port = 3000\nroom_port = 7500\n")
	for i := range 10 {
		fmt.Fprintf(&cfg, "[tunnels.p%d]\nlocal_port = 3000\n", i)
	}
	f.e.newRepoNamed("tunnels", map[string]string{"hostel.toml": cfg.String()})
	r := f.e.hostel("acquire", "fake-screen", "--task", "x", "--wait", "30s")
	if r.code != 1 || !strings.Contains(r.stderr, "no free pool port") {
		t.Fatalf("acquire: %s", r)
	}
	if rooms := f.e.fakeRooms(); len(rooms) != 0 {
		t.Fatalf("provider rooms: %v, want none", rooms)
	}
	if rooms := f.cloudRooms(); len(rooms) != 0 {
		t.Fatalf("rooms: %+v, want none", rooms)
	}
}

// TestDuplicateFixedPorts: two tunnels cannot have the same fixed room port.
func TestDuplicateFixedPorts(t *testing.T) {
	t.Parallel()
	f := newFixture(t, &fixtureOpts{files: map[string]string{
		"hostel.toml": "[tunnels.a]\nlocal_port = 3000\nroom_port = 7600\n[tunnels.b]\nlocal_port = 3001\nroom_port = 7600\n",
	}})
	r := f.e.hostel("acquire", "fake-screen", "--task", "x")
	if r.code != 1 || !strings.Contains(r.stderr, "room port 7600 is also the room port of tunnel a") {
		t.Fatalf("acquire: %s", r)
	}
	f.e.newRepoNamed("plain", defaultRepoFiles())
	id := f.acquire()
	body := `[{"name":"a","room_port":7600},{"name":"b","room_port":7600}]`
	if code := f.s.put("/v1/leases/"+id+"/tunnels", f.e.vars["HOSTEL_TOKEN"], body); code != 400 {
		t.Fatalf("PUT tunnels with a duplicate fixed port: HTTP %d, want 400", code)
	}
	create, err := json.Marshal(protocol.CreateLeaseRequest{Tools: []string{"fake-screen"}, Task: "x",
		PublicKey: string(ssh.MarshalAuthorizedKey(f.leaseKey().PublicKey())),
		Tunnels:   []protocol.TunnelRequest{{Name: "a", RoomPort: 7600}, {Name: "b", RoomPort: 7600}}})
	if err != nil {
		t.Fatal(err)
	}
	if code := f.s.send(http.MethodPost, "/v1/leases", f.e.vars["HOSTEL_TOKEN"], strings.NewReader(string(create)), nil); code != 400 {
		t.Fatalf("create a lease with a duplicate fixed port: HTTP %d, want 400", code)
	}
}

// TestTunnelChangeWakesPolls and closes forwards of removed ports: a tunnel
// change raises the lease revision, and the gateway closes a remote forward
// whose port the lease no longer registers, also if the guest does not cancel it.
func TestTunnelChange(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	id := f.acquire()
	guest := f.e.vars["HOSTEL_TOKEN"]
	before := f.lease(id).Revision
	if code := f.s.put("/v1/leases/"+id+"/tunnels", guest, `[{"name":"web","room_port":7600}]`); code != 200 {
		t.Fatalf("PUT tunnels: HTTP %d", code)
	}
	if after := f.lease(id).Revision; after <= before {
		t.Fatalf("revision %d → %d after a tunnel change", before, after)
	}
	ports, err := sshDial(f.s.gateway, id+".ports", f.leaseKey())
	if err != nil {
		t.Fatal(err)
	}
	defer ports.Close()
	ln, err := ports.Listen("tcp", "localhost:7600")
	if err != nil {
		t.Fatalf("remote forward of a registered port: %v", err)
	}
	defer ln.Close()
	if got := f.work("nc -z 127.0.0.1 7600 && echo open || echo closed"); got != "open" {
		t.Fatalf("room port 7600 with the forward: %s", got)
	}
	if code := f.s.put("/v1/leases/"+id+"/tunnels", guest, `[]`); code != 200 {
		t.Fatalf("PUT tunnels: HTTP %d", code)
	}
	f.e.waitFor("forward closed on the room", time.Minute, func() bool {
		return f.work("nc -z 127.0.0.1 7600 && echo open || echo closed") == "closed"
	})
}
