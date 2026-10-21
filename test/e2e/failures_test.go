//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
)

const cacheDir = "/home/hostel/hostel/cache"

// marker creates or removes a test marker in the room cache (see the
// fakedevice scripts).
func (f *fixture) marker(name string, on bool) {
	if on {
		f.c.sh("touch " + cacheDir + "/" + name)
	} else {
		f.c.sh("rm -f " + cacheDir + "/" + name)
	}
}

func (f *fixture) roomID() string {
	return fmt.Sprint(f.s.room("home/box").ID)
}

// admin runs a command with the admin token.
func (f *fixture) admin(args ...string) result {
	f.e.t.Helper()
	guest := f.e.vars["HOSTEL_TOKEN"]
	f.e.vars["HOSTEL_TOKEN"] = f.s.token
	defer func() { f.e.vars["HOSTEL_TOKEN"] = guest }()
	return f.e.mustHostel(args...)
}

// TestFailures is scenario 20: a prepare failure returns the lease to the
// queue and marks the room broken; a cleanup failure marks the room and the
// slot broken; admin repair makes a static room usable again.
func TestFailures(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.marker("fail-prepare", true)
	r := f.e.hostel("acquire", "fakedevice", "--task", "prepare fails", "--wait", "8s")
	if r.code != 75 || !strings.Contains(r.stderr, "queued") || !strings.Contains(r.stderr, "room broken: platform fakedevice: prepare script exited with code 1") {
		t.Fatalf("acquire with a failing prepare: %s", r)
	}
	if room := f.s.room("home/box"); room.State != protocol.RoomBroken || !strings.Contains(room.Reason, "prepare script exited with code 1") {
		t.Fatalf("room after the prepare failure: %+v", room)
	}
	f.marker("fail-prepare", false)
	f.admin("admin", "room", "repair", f.roomID())
	f.acquire()

	// A cleanup failure: the room and the slot are broken; a new lease waits.
	f.marker("fail-reset", true)
	f.e.mustHostel("release")
	f.e.waitFor("room broken by the cleanup", time.Minute, func() bool {
		room := f.s.room("home/box")
		return room.State == protocol.RoomBroken && strings.Contains(room.Reason, "reset failed on purpose")
	})
	if slot := f.s.room("home/box").Slots[0]; slot.State != protocol.SlotBroken {
		t.Fatalf("slot after the cleanup failure: %+v", slot)
	}
	f.e.newRepoNamed("next", defaultRepoFiles())
	if r := f.e.hostel("acquire", "fakedevice", "--task", "next", "--wait", "3s"); r.code != 75 {
		t.Fatalf("a new lease on a broken room: %s", r)
	}
	f.marker("fail-reset", false)
	f.admin("admin", "room", "repair", f.roomID())
	f.e.mustHostel("acquire", "fakedevice", "--task", "next")
	if got := f.c.sh("grep -c '^reset linux' " + cacheDir + "/fakedevice.log"); got != "1" {
		t.Fatalf("successful resets: %s, want 1 (the repair)", got)
	}
}

// TestStaticOffline is scenario 25 (offline after start): the host goes offline
// during preparing; the room is broken (unreachable), the lease is queued, and
// the room becomes usable by itself when the host is back.
func TestStaticOffline(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.marker("slow-prepare", true)
	if r := f.e.hostel("acquire", "fakedevice", "--task", "offline", "--wait", "3s"); r.code != 75 || !strings.Contains(r.stderr, "preparing") {
		t.Fatalf("acquire with a slow prepare: %s", r)
	}
	f.c.stop()
	f.e.waitFor("room unreachable", time.Minute, func() bool {
		room := f.s.room("home/box")
		return room.State == protocol.RoomBroken && room.Reason == "unreachable"
	})
	if l := f.s.status().Leases; len(l) != 1 || l[0].State != protocol.LeaseQueued {
		t.Fatalf("lease after the host went offline: %+v", l)
	}
	f.c.start()
	f.marker("slow-prepare", false)
	f.e.mustHostel("acquire", "fakedevice", "--task", "offline")
}

// TestRestart is scenario 17: after kill -9 in each lease state, the lease
// continues correctly and no second room is created.
func TestRestart(t *testing.T) {
	t.Parallel()
	t.Run("queued and ready", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, nil)
		f.acquire()
		f.e.newRepoNamed("waits", defaultRepoFiles())
		id := f.queued()
		f.s = f.s.restartAfterKill()
		if l := f.lease(id); l.State != protocol.LeaseQueued {
			t.Fatalf("queued lease after the restart: %s", l.State)
		}
		f.useRepo("repo")
		f.e.mustHostel("run", "--", "true")
		f.e.mustHostel("release")
		f.useRepo("waits")
		f.e.mustHostel("acquire", "fakedevice", "--task", "wait")
	})
	t.Run("preparing", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, nil)
		f.marker("slow-prepare", true)
		if r := f.e.hostel("acquire", "fakedevice", "--task", "x", "--wait", "3s"); r.code != 75 || !strings.Contains(r.stderr, "preparing") {
			t.Fatalf("acquire: %s", r)
		}
		f.s.kill()
		f.marker("slow-prepare", false)
		f.s = f.s.restart()
		f.acquire()
	})
	t.Run("cleaning", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, nil)
		id := f.acquire()
		f.marker("slow-reset", true)
		f.e.mustHostel("release")
		f.e.waitFor("lease cleaning", time.Minute, func() bool { return f.lease(id).State == protocol.LeaseCleaning })
		f.s.kill()
		f.marker("slow-reset", false)
		f.s = f.s.restart()
		f.waitSlotFree()
		if l := f.lease(id); l.State != protocol.LeaseReleased {
			t.Fatalf("lease after the cleanup: %s", l.State)
		}
		if f.slotExists() {
			t.Fatal("the slot directory exists after the cleanup")
		}
	})
	t.Run("provisioning during the provider call", func(t *testing.T) {
		t.Parallel()
		f := newExternalFixture(t, "", "", map[string]string{"FAKE_DELAY": "8s"}, smallMachine)
		if r := f.e.hostel("acquire", "fakedevice", "--task", "x", "--wait", "2s"); r.code != 75 || !strings.Contains(r.stderr, "provisioning") {
			t.Fatalf("acquire: %s", r)
		}
		f.s = f.s.restartAfterKill()
		f.acquire()
		if rooms := f.e.fakeRooms(); len(rooms) != 1 {
			t.Fatalf("provider rooms after the restart: %v, want one", rooms)
		}
	})
}

// TestRestartQueue is scenario 27: the server is down longer than
// poll_timeout; after the restart, no queued lease is cancelled at once.
func TestRestartQueue(t *testing.T) {
	t.Parallel()
	f := newFixture(t, &fixtureOpts{serverKeys: "poll_timeout = \"3s\"\n"})
	f.acquire()
	f.e.newRepoNamed("waits", defaultRepoFiles())
	id := f.queued()
	f.s.stop()
	down := time.Now()
	f.e.waitFor("server down longer than poll_timeout", time.Minute, func() bool { return time.Since(down) > 5*time.Second })
	f.s = f.s.restart()
	r := f.e.hostel("acquire", "fakedevice", "--task", "wait", "--wait", "1s")
	if r.code != 75 || !strings.Contains(r.stderr, "lease "+id+" queued") {
		t.Fatalf("the queued lease after the restart: %s", r)
	}
}
