//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
)

// TestIdleExpiry is scenario 9: idle expiry after idle_timeout; an open cmd
// connection keeps the lease active; a long run that ends does not cause an
// immediate expiry.
func TestIdleExpiry(t *testing.T) {
	t.Parallel()
	f := newFixture(t, &fixtureOpts{serverKeys: "idle_timeout = \"4s\"\n"})
	f.acquire()
	// The shell connection stays open longer than the idle timeout.
	f.e.mustHostel("ssh", "--", "sleep 8")
	f.e.mustHostel("run", "--", "true")
	// A run that streams for longer than the idle timeout.
	f.e.mustHostel("run", "--", "sleep 8")
	f.e.mustHostel("run", "--", "true")
	// Without activity, the lease expires.
	f.waitSlotFree()
	r := f.e.hostel("run", "--", "true")
	if r.code != 1 || !strings.Contains(r.stderr, "ended (expired: idle") {
		t.Fatalf("run after the idle expiry: %s", r)
	}
}

// TestMaxLease is scenario 10: max_lease ends an active lease.
func TestMaxLease(t *testing.T) {
	t.Parallel()
	f := newFixture(t, &fixtureOpts{machineKeys: "max_lease = \"10s\"\n"})
	// The lease is ready before acquire returns (acquire then syncs and runs
	// the setup job), so the time before acquire is a safe lower bound.
	start := time.Now()
	f.acquire()
	var last result
	f.e.waitFor("lease ended by max_lease", time.Minute, func() bool {
		last = f.e.hostel("run", "--", "true")
		return last.code != 0
	})
	if !strings.Contains(last.stderr, "maximum lease duration") {
		t.Fatalf("run after max_lease: %s", last)
	}
	if d := time.Since(start); d < 8*time.Second {
		t.Fatalf("the lease ended after %s, before max_lease", d)
	}
}

// TestOwnership is scenario 21: a user cannot read or release another user's
// lease; an admin can.
func TestOwnership(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	id := f.acquire()
	owner := f.e.vars["HOSTEL_TOKEN"]
	other := f.e.useGuestToken()
	f.e.vars["HOSTEL_TOKEN"] = owner
	for _, path := range []string{"/v1/leases/" + id, "/v1/leases/" + id + "/release"} {
		method := "GET"
		if strings.HasSuffix(path, "release") {
			method = "POST"
		}
		if code := f.s.do(method, path, other, nil); code != 403 {
			t.Fatalf("%s %s with another user's token: HTTP %d, want 403", method, path, code)
		}
	}
	// An admin release ends a running "hostel run" at once, with the reason.
	done := make(chan result, 1)
	go func() { done <- f.e.hostel("run", "--", "echo started; sleep 60") }()
	f.e.waitFor("job started", time.Minute, func() bool {
		return strings.Contains(f.c.sh("cat /home/hostel/hostel/slots/a/jobs/*/output 2>/dev/null || true"), "started")
	})
	if code := f.s.do("POST", "/v1/leases/"+id+"/release", f.s.token, nil); code != 200 {
		t.Fatalf("admin release: HTTP %d", code)
	}
	select {
	case r := <-done:
		if r.code != 1 || !strings.Contains(r.stderr, "released by admin (admin)") {
			t.Fatalf("run during the admin release: %s", r)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run did not end within 30 seconds after the admin release")
	}
	f.waitSlotFree()
	f.e.vars["HOSTEL_TOKEN"] = f.s.token
	if r := f.e.hostel("admin", "lease", "release", id); r.code != 0 {
		t.Fatalf("admin lease release of an ended lease: %s", r)
	}
	f.e.vars["HOSTEL_TOKEN"] = owner
}

// TestPlatformsAndDevices is scenario 22 (devices): an OS platform without a
// platform directory, device selection, device facts, and screenshots.
func TestPlatformsAndDevices(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	r := f.e.mustHostel("platforms")
	if !strings.Contains(r.stdout, "fakedevice  device  phone, tablet, watch  1") || !strings.Contains(r.stdout, "linux       os") {
		t.Fatalf("platforms:\n%s", r.stdout)
	}

	// linux is an OS platform without a directory.
	f.e.mustHostel("acquire", "linux", "--task", "plain linux")
	if r := f.e.mustHostel("run", "--", "echo $HOSTEL_PLATFORM $HOSTEL_ROOM_OS"); r.stdout != "linux linux\n" {
		t.Fatalf("linux lease: %q", r.stdout)
	}
	f.e.mustHostel("release")
	f.waitSlotFree()

	f.acquire("--device", "tablet", "--device", "phone")
	r = f.e.mustHostel("run", "--", "echo $HOSTEL_DEVICES ${FAKE_DEVICE_ID%%-*} $FAKE_DEVICE_OS")
	if r.stdout != "tablet,phone tablet linux\n" {
		t.Fatalf("devices: %q", r.stdout)
	}
	shot := filepath.Join(f.e.home, "phone.png")
	r = f.e.mustHostel("screenshot", "--device", "phone", "-o", shot)
	if data, err := os.ReadFile(shot); err != nil || !strings.HasSuffix(string(data), "fake screenshot of phone on linux\n") {
		t.Fatalf("screenshot of phone: %q, %v", data, err)
	}
	r = f.e.mustHostel("screenshot")
	data, err := os.ReadFile(filepath.Join(f.e.cwd, strings.TrimSpace(r.stdout)))
	if err != nil || !strings.HasSuffix(string(data), "fake screenshot of tablet on linux\n") {
		t.Fatalf("default screenshot: %q, %v", data, err)
	}
	if r := f.e.hostel("screenshot", "--device", "watch"); r.code != 1 || !strings.Contains(r.stderr, "not in this lease") {
		t.Fatalf("screenshot of a device that is not in the lease: %s", r)
	}
	// A queued lease shows in the status with its position.
	if st := f.s.status(); len(st.Leases) != 1 || st.Leases[0].State != protocol.LeaseReady {
		t.Fatalf("status leases: %+v", st.Leases)
	}
}
