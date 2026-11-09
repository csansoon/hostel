//go:build e2e

package e2e

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
)

const cacheDir = "/home/hostel/hostel/cache"

// marker creates or removes a test marker in the machine cache (see the
// fake-screen scripts).
func (f *fixture) marker(name string, on bool) {
	if on {
		f.c.sh("touch " + cacheDir + "/" + name)
	} else {
		f.c.sh("rm -f " + cacheDir + "/" + name)
	}
}

func (f *fixture) machineID() string {
	return fmt.Sprint(f.s.machine("home/box").ID)
}

// admin runs a command with the admin token.
func (f *fixture) admin(args ...string) result {
	f.e.t.Helper()
	guest := f.e.vars["HOSTEL_TOKEN"]
	f.e.vars["HOSTEL_TOKEN"] = f.s.token
	defer func() { f.e.vars["HOSTEL_TOKEN"] = guest }()
	return f.e.mustHostel(args...)
}

// TestFailures is scenario 20: a failed start of a screen tool returns the
// lease to the waiting line and marks the machine broken; a cleanup failure marks
// the machine and the room broken; admin repair makes a static machine usable again.
func TestFailures(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.marker("fail-start", true)
	r := f.e.hostel("acquire", "fake-screen", "--task", "start fails", "--wait", "40s")
	if r.code != 75 || !strings.Contains(r.stderr, "queued") || !strings.Contains(r.stderr, "machine broken: tool fake-screen: start script exited with code 1") {
		t.Fatalf("acquire with a failing start: %s", r)
	}
	if machine := f.s.machine("home/box"); machine.State != protocol.MachineBroken || !strings.Contains(machine.Reason, "start script exited with code 1") {
		t.Fatalf("machine after the start failure: %+v", machine)
	}
	f.marker("fail-start", false)
	f.admin("admin", "machine", "repair", f.machineID())
	f.acquire()

	// A cleanup failure: the machine and the room are broken; a new lease waits.
	f.marker("fail-stop", true)
	f.e.mustHostel("release")
	f.e.waitFor("machine broken by the cleanup", time.Minute, func() bool {
		machine := f.s.machine("home/box")
		return machine.State == protocol.MachineBroken && strings.Contains(machine.Reason, "stop failed on purpose")
	})
	if room := f.s.machine("home/box").Rooms[0]; room.State != protocol.RoomBroken {
		t.Fatalf("room after the cleanup failure: %+v", room)
	}
	f.e.newRepoNamed("next", defaultRepoFiles())
	if r := f.e.hostel("acquire", "fake-screen", "--task", "next", "--wait", "3s"); r.code != 75 {
		t.Fatalf("a new lease on a broken machine: %s", r)
	}
	f.marker("fail-stop", false)
	f.admin("admin", "machine", "repair", f.machineID())
	f.e.mustHostel("acquire", "fake-screen", "--task", "next")
	// Two successful stops: the cleanup after the failed start (the tool can
	// leave processes), and the repair after the failed stop (a failing stop
	// writes no log line).
	if got := f.c.sh("grep -c '^stop linux' " + cacheDir + "/fake-screen.log"); got != "2" {
		t.Fatalf("successful stops: %s, want 2", got)
	}
}

// TestStaticOffline is scenario 25 (offline after start): the host goes offline
// during preparing; the machine is broken (unreachable), the lease is queued, and
// the machine becomes usable by itself when the host is back.
func TestStaticOffline(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.marker("slow-start", true)
	f.acquireUntil(protocol.LeasePreparing)
	f.c.stop()
	f.e.waitFor("machine unreachable", time.Minute, func() bool {
		machine := f.s.machine("home/box")
		return machine.State == protocol.MachineBroken && machine.Reason == "unreachable"
	})
	if l := f.s.status().Leases; len(l) != 1 || l[0].State != protocol.LeaseQueued {
		t.Fatalf("lease after the host went offline: %+v", l)
	}
	f.c.start()
	f.marker("slow-start", false)
	f.e.mustHostel("acquire", "fake-screen", "--task", "offline")
}

// TestRestart is scenario 17: after kill -9 in each lease state, the lease
// continues correctly and no second machine is created.
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
		f.e.mustHostel("acquire", "fake-screen", "--task", "wait")
	})
	t.Run("preparing", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, nil)
		f.marker("slow-start", true)
		f.acquireUntil(protocol.LeasePreparing)
		f.s.kill()
		f.marker("slow-start", false)
		f.s = f.s.restart()
		f.acquire()
	})
	t.Run("cleaning", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, nil)
		id := f.acquire()
		f.marker("slow-stop", true)
		f.e.mustHostel("release")
		f.e.waitFor("lease cleaning", time.Minute, func() bool { return f.lease(id).State == protocol.LeaseCleaning })
		f.s.kill()
		f.marker("slow-stop", false)
		f.s = f.s.restart()
		f.waitRoomFree()
		if l := f.lease(id); l.State != protocol.LeaseReleased {
			t.Fatalf("lease after the cleanup: %s", l.State)
		}
		if f.roomExists() {
			t.Fatal("the room directory exists after the cleanup")
		}
	})
	t.Run("provisioning during the provider call", func(t *testing.T) {
		t.Parallel()
		f := newExternalFixture(t, "", "", map[string]string{"FAKE_DELAY": "20s"}, smallTemplate)
		f.acquireUntil(protocol.LeaseProvisioning)
		f.s = f.s.restartAfterKill()
		f.acquire()
		if machines := f.e.fakeMachines(); len(machines) != 1 {
			t.Fatalf("provider machines after the restart: %v, want one", machines)
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
	r := f.e.hostel("acquire", "fake-screen", "--task", "wait", "--wait", "5s")
	if r.code != 75 || !strings.Contains(r.stderr, "lease "+id+" queued") {
		t.Fatalf("the queued lease after the restart: %s", r)
	}
}

// TestPollNetworkError: a waiting acquire survives a server that stops during
// its long poll and is down for a while; it retries its poll and continues the
// same lease. A short acquire during the outage ends at its --wait.
func TestPollNetworkError(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.acquire()
	f.e.newRepoNamed("waits", defaultRepoFiles())
	id := f.queued()
	var stderr syncBuffer
	cmd := exec.Command(hostelBin, "acquire", "fake-screen", "--task", "wait", "--wait", "110s")
	cmd.Env, cmd.Dir, cmd.Stderr = f.e.environ(), f.e.cwd, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { cmd.Process.Kill() })
	// The first poll answers at once; the second waits for a change.
	f.e.waitFor("the guest polls", time.Minute, func() bool { return strings.Contains(stderr.String(), "lease "+id+" queued") })
	f.s.stop()
	f.e.waitFor("the guest retries", time.Minute, func() bool { return strings.Contains(stderr.String(), "; retrying") })
	// While the server is down, a short acquire still ends at its --wait:
	// failed polls count too.
	start := time.Now()
	if r := f.e.hostel("acquire", "fake-screen", "--task", "wait", "--wait", "3s"); r.code != 75 || time.Since(start) > 20*time.Second {
		t.Fatalf("acquire --wait 3s while the server is down: %s after %s", r, time.Since(start))
	}
	f.s = f.s.restart()
	f.useRepo("repo")
	f.e.mustHostel("release")
	select {
	case err := <-done:
		if err != nil || strings.Contains(stderr.String(), "created") {
			t.Fatalf("acquire of lease %s after the outage: %v\n%s", id, err, stderr.String())
		}
	case <-time.After(2 * time.Minute):
		t.Fatalf("acquire did not finish:\n%s", stderr.String())
	}
	if l := f.lease(id); l.State != protocol.LeaseReady {
		t.Fatalf("lease %s: %s", id, l.State)
	}
}
