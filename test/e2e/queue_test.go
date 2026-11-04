//go:build e2e

package e2e

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
)

var createdRE = regexp.MustCompile(`lease ([a-z0-9]+) created`)

func createdID(t *testing.T, r result) string {
	t.Helper()
	m := createdRE.FindStringSubmatch(r.stderr)
	if m == nil {
		t.Fatalf("no created lease: %s", r)
	}
	return m[1]
}

// useRepo makes guest commands run in another repository (worktree).
func (f *fixture) useRepo(name string) {
	f.e.cwd = filepath.Join(f.e.home, name)
}

// queued starts a lease that must wait: acquire returns 75 and names the same
// command; it returns the lease ID.
func (f *fixture) queued(args ...string) string {
	f.e.t.Helper()
	r := f.e.hostel(append([]string{"acquire", "fakedevice", "--task", "wait", "--wait", "2s"}, args...)...)
	if r.code != 75 || !strings.HasSuffix(strings.TrimSpace(r.stderr), "not finished; run the same command again") {
		f.e.t.Fatalf("acquire of a waiting lease: %s", r)
	}
	return createdID(f.e.t, r)
}

// acquireUntil starts an acquire that does not finish (exit code 75) and waits
// until the lease is in state.
func (f *fixture) acquireUntil(state string) string {
	f.e.t.Helper()
	r := f.e.hostel("acquire", "fakedevice", "--task", "until "+state, "--wait", "1s")
	if r.code != 75 {
		f.e.t.Fatalf("acquire: %s", r)
	}
	id := createdID(f.e.t, r)
	f.e.waitFor("lease "+state, time.Minute, func() bool { return f.lease(id).State == state })
	return id
}

func (f *fixture) lease(id string) protocol.Lease {
	f.e.t.Helper()
	var l protocol.Lease
	if code := f.s.get("/v1/leases/"+id, f.s.token, &l); code != 200 {
		f.e.t.Fatalf("GET lease %s: HTTP %d", id, code)
	}
	return l
}

// TestTwoGuestsOneSlot is scenarios 2 and 7: the second guest waits, sees its
// position, and gets the slot after the first releases; the same acquire
// command continues the same lease.
func TestTwoGuestsOneSlot(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.acquire()
	f.e.newRepoNamed("repo2", defaultRepoFiles())
	r := f.e.hostel("acquire", "fakedevice", "--task", "second", "--wait", "5s")
	if r.code != 75 || !strings.Contains(r.stderr, "queued, position 1 (no free slot)") {
		t.Fatalf("second guest: %s", r)
	}
	id := createdID(t, r)
	f.useRepo("repo")
	f.e.mustHostel("release")
	f.useRepo("repo2")
	r = f.e.mustHostel("acquire", "fakedevice", "--task", "second")
	if !strings.Contains(r.stdout, "Lease "+id+" is ready") || strings.Contains(r.stderr, "created") {
		t.Fatalf("the same command does not continue lease %s: %s", id, r)
	}
}

// TestPriority is scenario 3: a high lease passes a normal lease in the queue.
func TestPriority(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.acquire()
	f.e.newRepoNamed("normal", defaultRepoFiles())
	normal := f.queued()
	f.e.newRepoNamed("high", defaultRepoFiles())
	high := f.queued("--priority", "high")
	if l := f.lease(high); l.Position != 1 {
		t.Fatalf("high lease position %d, want 1", l.Position)
	}
	if l := f.lease(normal); l.Position != 2 {
		t.Fatalf("normal lease position %d, want 2", l.Position)
	}
	f.useRepo("repo")
	f.e.mustHostel("release")
	f.e.waitFor("high lease ready", time.Minute, func() bool { return f.lease(high).State == protocol.LeaseReady })
	if l := f.lease(normal); l.State != protocol.LeaseQueued || l.Position != 1 {
		t.Fatalf("normal lease = %s position %d, want queued at 1", l.State, l.Position)
	}
}

// TestBlockedMachines is scenario 5: a lease that waits for a full machine
// does not block a lease for another machine.
func TestBlockedMachines(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.init()
	dev := e.startStaticRoom("dev")
	plain := e.startStaticRoom("plain")
	e.usePlatform("fakedevice")
	e.writeConfig(serverHeader() + `
[providers.home]
kind = "static"

[providers.home.machines.dev]
address = "` + dev.address() + `"
os = "linux"
platforms = ["fakedevice"]

[providers.home.machines.plain]
address = "` + plain.address() + `"
os = "linux"
`)
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	s.waitRoom("home/dev", protocol.RoomRunning, roomReady)
	s.waitRoom("home/plain", protocol.RoomRunning, roomReady)
	e.useGuestToken()
	f := &fixture{e: e, s: s, c: dev}
	e.newRepo(defaultRepoFiles())
	f.acquire()
	e.newRepoNamed("waits", defaultRepoFiles())
	f.queued()
	// The linux lease can use both machines; dev is busy and blocked, so it
	// gets plain.
	e.newRepoNamed("linux", defaultRepoFiles())
	r := e.mustHostel("acquire", "linux", "--task", "other machine")
	if !strings.Contains(r.stderr, "on home/plain") {
		t.Fatalf("linux lease: %s", r)
	}
}

// TestRequestErrors is scenario 6: exit code 69 at once when no machine can
// ever satisfy the request.
func TestRequestErrors(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"unknown platform", []string{"acquire", "nothing", "--task", "x"}, `unknown platform "nothing"`},
		{"unknown device", []string{"acquire", "fakedevice", "--task", "x", "--device", "toaster"}, `has no device "toaster"`},
		{"too many devices", []string{"acquire", "fakedevice", "--task", "x", "--device", "phone", "--device", "tablet", "--device", "watch"}, "at most 2 devices"},
		{"no machine with the labels", []string{"acquire", "fakedevice", "--task", "x", "--require", "gpu"}, "with the labels gpu"},
		{"OS platform without machine", []string{"acquire", "windows", "--task", "x"}, "no machine can serve platform windows"},
	}
	for _, c := range cases {
		r := f.e.hostel(c.args...)
		if r.code != 69 || !strings.Contains(r.stderr, c.want) {
			t.Fatalf("%s: want exit code 69 and %q: %s", c.name, c.want, r)
		}
	}
	// More tunnels than the slot has pool ports.
	var tunnels strings.Builder
	for i := range 11 {
		tunnels.WriteString("[tunnels.t" + string(rune('a'+i)) + "]\nlocal_port = 3000\n")
	}
	f.e.newRepoNamed("tunnels", map[string]string{"hostel.toml": tunnels.String()})
	r := f.e.hostel("acquire", "fakedevice", "--task", "x")
	if r.code != 69 || !strings.Contains(r.stderr, "and 11 tunnel ports") {
		t.Fatalf("too many tunnels: %s", r)
	}
}

// TestPollTimeout is scenario 8: a queued lease that nobody polls is
// cancelled after poll_timeout.
func TestPollTimeout(t *testing.T) {
	t.Parallel()
	f := newFixture(t, &fixtureOpts{serverKeys: "poll_timeout = \"3s\"\n"})
	f.acquire()
	f.e.newRepoNamed("waits", defaultRepoFiles())
	id := f.queued()
	f.e.waitFor("queued lease cancelled", time.Minute, func() bool { return f.lease(id).State == protocol.LeaseCancelled })
	if l := f.lease(id); !strings.Contains(l.EndReason, "poll timeout") {
		t.Fatalf("end reason %q", l.EndReason)
	}
	r := f.e.hostel("acquire", "fakedevice", "--task", "wait", "--wait", "2s")
	if r.code != 1 || !strings.Contains(r.stderr, "ended (cancelled: poll timeout") {
		t.Fatalf("acquire after the poll timeout: %s", r)
	}
}

// TestAcquireBackground: "acquire --background" returns at once, and a
// detached keeper polls the waiting lease, so it keeps its place in the queue
// for longer than poll_timeout without acquire; a lease without it ends after
// poll_timeout (acquire says so). When the room is free, the kept lease is
// ready, the same command continues it, and the keeper stops.
func TestAcquireBackground(t *testing.T) {
	t.Parallel()
	f := newFixture(t, &fixtureOpts{serverKeys: "poll_timeout = \"6s\"\n"})
	f.acquire()

	f.e.newRepoNamed("repo2", defaultRepoFiles())
	r := f.e.hostel("acquire", "fakedevice", "--task", "in the background", "--background")
	if r.code != 75 || !strings.Contains(r.stderr, "keeps the place of lease") {
		t.Fatalf("acquire --background: %s", r)
	}
	kept := createdID(t, r)

	f.e.newRepoNamed("repo3", defaultRepoFiles())
	r = f.e.hostel("acquire", "fakedevice", "--task", "without polls", "--wait", "2s")
	if r.code != 75 || !strings.Contains(r.stderr, "the server keeps the place of lease") || !strings.Contains(r.stderr, "for 6 s without this command") {
		t.Fatalf("acquire of a waiting lease: %s", r)
	}
	dropped := createdID(t, r)
	f.e.waitFor("the end of the lease without polls", time.Minute, func() bool { return f.lease(dropped).State != protocol.LeaseQueued })

	// The kept lease outlives 3 poll timeouts without an acquire of its own.
	start := time.Now()
	f.e.waitFor("3 poll timeouts", time.Minute, func() bool { return time.Since(start) > 18*time.Second })
	if st := f.lease(kept).State; st != protocol.LeaseQueued {
		t.Fatalf("the kept lease after 3 poll timeouts: %s, want queued", st)
	}

	f.useRepo("repo")
	f.e.mustHostel("release")
	f.useRepo("repo2")
	f.e.waitFor("the kept lease ready", 2*time.Minute, func() bool { return f.lease(kept).State == protocol.LeaseReady })
	if r := f.e.mustHostel("acquire", "fakedevice", "--task", "in the background"); !strings.Contains(r.stdout, "Lease "+kept+" is ready") {
		t.Fatalf("acquire after the background wait: %s", r)
	}
	f.e.waitFor("the keeper stops", time.Minute, func() bool {
		out, _ := exec.Command("pgrep", "-f", "keep-place --dir "+filepath.Join(f.e.home, "repo2")).Output()
		return len(strings.TrimSpace(string(out))) == 0
	})
	f.e.mustHostel("release")
}
