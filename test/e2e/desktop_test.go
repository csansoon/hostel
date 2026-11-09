//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// TestDesktop (M7): "desktop" runs a lease command through the desktop program
// ("machine desktop serve"; on a Windows machine it runs in the desktop session, which
// SSH cannot reach). The command gets the lease environment, its output, and
// its exit code. Only the desktop lease of the room may use it. A child that
// keeps the output open does not block the command, and a command ends when
// its caller ends. A detached command ends with the lease, also when it cleared
// HOSTEL_LEASE, and the desktop program serves the next lease.
func TestDesktop(t *testing.T) {
	t.Parallel()
	f := newFixture(t, &fixtureOpts{templateTools: []string{"fake-screen", "fake-desktop"}})
	lease := f.acquire("fake-desktop")
	if r := f.e.hostel("run", "--", "desktop true"); r.code != 69 || !strings.Contains(r.stdout, "no desktop session") {
		t.Fatalf("desktop without the desktop program: %s, want exit code 69", r)
	}
	if _, err := docker("exec", "-d", "-u", "hostel", f.c.name, "/home/hostel/hostel/bin/hostel", "helper", "desktop", "serve"); err != nil {
		t.Fatal(err)
	}
	f.e.waitFor("the desktop program", time.Minute, func() bool {
		return f.c.sh("test -s /home/hostel/hostel/desktop.json && echo yes || echo no") == "yes"
	})

	r := f.e.hostel("run", "--", `desktop sh -c 'echo "$HOSTEL_LEASE $(pwd)"; echo to-stderr >&2; exit 3'`)
	// A job's output has stdout and stderr together.
	if r.code != 3 || !strings.Contains(r.stdout, lease+" /home/hostel/hostel/rooms/a/work\n") || !strings.Contains(r.stdout, "to-stderr") {
		t.Fatalf("desktop command: %s, want exit code 3, the lease ID, and the work directory", r)
	}
	if r := f.e.hostel("run", "--", "HOSTEL_LEASE=other desktop true"); r.code != 127 || !strings.Contains(r.stdout, "not the lease of room a") {
		t.Fatalf("desktop with another lease ID: %s, want a refusal", r)
	}
	start := time.Now()
	if r := f.e.mustHostel("run", "--", "desktop sh -c 'sleep 3003 & echo bg'"); !strings.Contains(r.stdout, "bg") || time.Since(start) > 30*time.Second {
		t.Fatalf("desktop command with a background child: %s after %s", r, time.Since(start))
	}
	// The caller ends after 3 s; the command must end too.
	f.e.hostel("run", "--", "timeout 3 desktop sleep 3004")
	f.e.waitFor("the desktop command ends with its caller", 30*time.Second, func() bool {
		return f.c.sh("pgrep -fx 'sleep 3004' | wc -l") == "0"
	})
	if r := f.e.hostel("run", "--", "desktop no-such-command"); r.code != 127 || !strings.Contains(r.stdout, "command not found") {
		t.Fatalf("desktop with a missing command: %s, want exit code 127", r)
	}
	if r := f.e.mustHostel("run", "--", "desktop --detach sleep 3001"); !strings.Contains(r.stdout, "started on the desktop") {
		t.Fatalf("desktop --detach: %s", r)
	}
	f.e.mustHostel("run", "--", "desktop --detach env -u HOSTEL_LEASE sleep 3002")
	running := func() string { return f.c.sh("pgrep -fx 'sleep 300[123]' | wc -l") }
	if got := running(); got != "3" {
		t.Fatalf("detached desktop commands and the background child: %s running, want 3", got)
	}
	f.e.mustHostel("release")
	f.waitRoomFree()
	f.e.waitFor("the detached desktop commands end with the lease", 30*time.Second, func() bool { return running() == "0" })

	f.acquire("fake-desktop")
	if r := f.e.mustHostel("run", "--", "desktop echo next"); r.stdout != "next\n" {
		t.Fatalf("desktop in the next lease: %s", r)
	}
	f.e.mustHostel("release")
	f.waitRoomFree()
	// A lease that does not hold the whole machine must not use the desktop.
	f.acquire()
	if r := f.e.hostel("run", "--", "desktop true"); r.code != 127 || !strings.Contains(r.stdout, "the desktop needs a lease with") {
		t.Fatalf("desktop in a lease without the desktop: %s, want a refusal", r)
	}
	f.e.mustHostel("release")
}
