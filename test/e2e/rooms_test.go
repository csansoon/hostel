//go:build e2e

package e2e

import (
	"io"
	"net"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
)

// readForward connects to a local forward and returns what the machine server
// sends ("" if the gateway refused the forward).
func readForward(t *testing.T, local string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", local, 2*time.Second)
	if err != nil {
		return ""
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	b, _ := io.ReadAll(c)
	return string(b)
}

// TestRoomUsers is the M6 room scenario: two leases at the same time on one
// machine, each room with its own OS user. Each lease runs as its room's user in
// its own home, cannot read the other's files, and can forward only to its own
// room's ports. The cleanup of one lease leaves the other's job running. An
// exclusive lease (a whole_machine tool) waits until both rooms are free and
// then takes both.
func TestRoomUsers(t *testing.T) {
	t.Parallel()
	f := newFixture(t, &fixtureOpts{roomUsers: []string{"hostel-a", "hostel-b"}, templateTools: []string{"fake-screen", "fake-desktop"},
		templateKeys: `rooms = [{ name = "a", user = "hostel-a" }, { name = "b", user = "hostel-b" }]
`})
	f.acquire()
	if r := f.e.mustHostel("run", "--", "id -un; echo $HOSTEL_WORK_DIR"); r.stdout != "hostel-a\n/home/hostel-a/hostel/rooms/a/work\n" {
		t.Fatalf("first lease: %q, want user hostel-a in its own home", r.stdout)
	}
	f.e.mustHostel("run", "--detach", "--", "while true; do echo hello-a | nc -N -l 127.0.0.1 7500; done")
	fa := strings.TrimSpace(f.e.mustHostel("forward", "7500").stdout)
	f.e.waitFor("the first lease's server on its room port", time.Minute, func() bool { return readForward(t, fa) == "hello-a\n" })
	f.e.mustHostel("run", "--detach", "--", "sleep 3001")

	f.e.newRepoNamed("second", defaultRepoFiles())
	f.acquire()
	if r := f.e.mustHostel("run", "--", "id -un; echo $HOSTEL_WORK_DIR"); r.stdout != "hostel-b\n/home/hostel-b/hostel/rooms/b/work\n" {
		t.Fatalf("second lease: %q, want user hostel-b in its own home", r.stdout)
	}
	if r := f.e.hostel("run", "--", "cat /home/hostel-a/hostel/rooms/a/work/a.txt"); r.code == 0 {
		t.Fatalf("the second lease read the first lease's files: %s", r)
	}
	f.e.mustHostel("run", "--detach", "--", "while true; do echo hello-b | nc -N -l 127.0.0.1 7510; done")
	own := strings.TrimSpace(f.e.mustHostel("forward", "7510").stdout)
	f.e.waitFor("forward to the own room's port", time.Minute, func() bool { return readForward(t, own) == "hello-b\n" })
	other := strings.TrimSpace(f.e.mustHostel("forward", "7500").stdout)
	if got := readForward(t, other); got != "" {
		t.Fatalf("forward to a port of the other room: %q, want a refusal", got)
	}
	if r := f.e.hostel("tunnel", "add", "x", "--local-port", "18080", "--machine-port", "7500"); r.code == 0 || !strings.Contains(r.stderr, "belongs to room a") {
		t.Fatalf("tunnel on a port of the other room: %s", r)
	}

	f.e.mustHostel("release")
	f.e.waitFor("room b free", time.Minute, func() bool {
		for _, sl := range f.s.machine("home/box").Rooms {
			if sl.Name == "b" {
				return sl.State == protocol.RoomFree && sl.LeaseID == ""
			}
		}
		return false
	})
	f.useRepo("repo")
	if r := f.e.mustHostel("jobs"); !strings.Contains(r.stdout, "running") || !strings.Contains(r.stdout, "sleep 3001") {
		t.Fatalf("the first lease's job after the second lease's cleanup:\n%s", r.stdout)
	}

	// An exclusive lease waits for the busy room, then takes both rooms.
	f.e.newRepoNamed("third", defaultRepoFiles())
	if r := f.e.hostel("acquire", "fake-desktop", "--task", "exclusive", "--wait", "2s"); r.code != 75 {
		t.Fatalf("exclusive lease while room a is busy: %s, want exit code 75", r)
	}
	f.useRepo("repo")
	f.e.mustHostel("release")
	f.useRepo("third")
	var lease string
	f.e.waitFor("the exclusive lease is ready", 2*time.Minute, func() bool {
		r := f.e.hostel("acquire", "fake-desktop", "--task", "exclusive", "--wait", "10s")
		if m := leaseIDRE.FindStringSubmatch(r.stdout); r.code == 0 && m != nil {
			lease = m[1]
			return true
		}
		return false
	})
	for _, sl := range f.s.machine("home/box").Rooms {
		if sl.LeaseID != lease {
			t.Fatalf("room %s after the exclusive lease is ready: lease %q, want %s", sl.Name, sl.LeaseID, lease)
		}
	}
	// The status counts both rooms as used.
	if r := f.e.mustHostel("status"); !regexp.MustCompile(`(?m)^home/box\s+\S.*\s+in use 2/2\s+guest\s+exclusive\s`).MatchString(r.stdout) {
		t.Fatalf("status with the exclusive lease, want \"in use 2/2\":\n%s", r.stdout)
	}
	// It holds both rooms, so it may forward to any port.
	f.e.mustHostel("run", "--detach", "--", "while true; do echo hello-x | nc -N -l 127.0.0.1 7510; done")
	fx := strings.TrimSpace(f.e.mustHostel("forward", "7510").stdout)
	f.e.waitFor("forward of the exclusive lease", time.Minute, func() bool { return readForward(t, fx) == "hello-x\n" })
	f.e.mustHostel("release")
}

// TestExclusiveScripts: the tool scripts of an exclusive lease (a
// whole_machine tool) get HOSTEL_EXCLUSIVE=1, so they may reset the desktop;
// those of a normal lease get it empty (M7).
func TestExclusiveScripts(t *testing.T) {
	t.Parallel()
	f := newFixture(t, &fixtureOpts{templateTools: []string{"fake-screen", "fake-desktop"}})
	desk := f.acquire("fake-screen", "fake-desktop")
	f.e.mustHostel("release")
	f.waitRoomFree()
	normal := f.acquire()
	f.e.mustHostel("release")
	f.waitRoomFree()
	log := f.c.sh("cat " + cacheDir + "/fake-screen.log")
	for _, want := range []string{
		"start linux " + desk + " option-a exclusive=1", "stop linux " + desk + " option-a exclusive=1",
		"start linux " + normal + " option-a exclusive=\n", "stop linux " + normal + " option-a exclusive=\n",
	} {
		if !strings.Contains(log+"\n", want) {
			t.Fatalf("fake-screen.log has no %q:\n%s", want, log)
		}
	}
	if log := f.c.sh("cat " + cacheDir + "/fake-desktop.log"); !strings.Contains(log, "start linux "+desk+" exclusive=1") || strings.Contains(log, normal) {
		t.Fatalf("fake-desktop.log: %q, want a start for the desktop lease only", log)
	}
}
