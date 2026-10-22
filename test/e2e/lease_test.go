//go:build e2e

package e2e

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
)

// fixture is a server with one static Docker room that serves the fakedevice
// platform, and a test repository.
type fixture struct {
	e *env
	s *serverProc
	c *container
}

const fixtureRepoConfig = `include = [".env"]

[platforms.fakedevice]
setup = ["echo setup-ran > setup.txt"]
env = { REPO_VAR = "repo-value" }
`

// fixtureOpts changes the fixture: server keys (top level of server.toml),
// keys of the machine "box", and the repository files.
type fixtureOpts struct {
	serverKeys  string
	machineKeys string
	files       map[string]string
	// platforms are extra platform files: platform → file → content.
	platforms map[string]map[string]string
}

func defaultRepoFiles() map[string]string {
	return map[string]string{
		"hostel.toml": fixtureRepoConfig,
		"a.txt":       "hello\n",
		".gitignore":  "ignored.txt\n.env\n",
		"ignored.txt": "not sent\n",
		".env":        "SECRET=1\n",
	}
}

func newFixture(t *testing.T, opts *fixtureOpts) *fixture {
	t.Helper()
	e := newEnv(t)
	e.init()
	return newFixtureWithEnv(t, e, opts)
}

// newFixtureWithEnv builds the fixture in an initialized environment.
func newFixtureWithEnv(t *testing.T, e *env, opts *fixtureOpts) *fixture {
	t.Helper()
	if opts == nil {
		opts = &fixtureOpts{}
	}
	c := e.startStaticRoom("box")
	e.usePlatform("fakedevice")
	for name, files := range opts.platforms {
		e.writePlatform(name, files)
	}
	e.writeConfig(serverHeader() + opts.serverKeys + fmt.Sprintf(`
[providers.home]
kind = "static"

[providers.home.machines.box]
address = %q
os = "linux"
platforms = ["fakedevice"]
env = { MACHINE_VAR = "machine-value" }
`, c.address()) + opts.machineKeys)
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	s.waitRoom("home/box", protocol.RoomRunning, roomReady)
	e.useGuestToken()
	files := opts.files
	if files == nil {
		files = defaultRepoFiles()
	}
	e.newRepo(files)
	return &fixture{e: e, s: s, c: c}
}

var leaseIDRE = regexp.MustCompile(`Lease ([a-z0-9]+) is ready`)

// acquire runs "hostel acquire" until the lease is ready and returns its ID.
func (f *fixture) acquire(args ...string) string {
	f.e.t.Helper()
	r := f.e.mustHostel(append([]string{"acquire", "fakedevice", "--task", "e2e test"}, args...)...)
	m := leaseIDRE.FindStringSubmatch(r.stdout)
	if m == nil {
		f.e.t.Fatalf("acquire printed no lease ID: %s", r)
	}
	return m[1]
}

// slotExists reports whether the slot directory exists on the room.
func (f *fixture) slotExists() bool {
	return f.c.sh("test -e /home/hostel/hostel/slots/a && echo yes || echo no") == "yes"
}

// waitSlotFree waits until cleanup freed the slot.
func (f *fixture) waitSlotFree() {
	f.e.t.Helper()
	f.e.waitFor("slot a free", time.Minute, func() bool {
		r := f.s.room("home/box")
		return len(r.Slots) == 1 && r.Slots[0].State == protocol.SlotFree && r.Slots[0].LeaseID == ""
	})
}

// TestAcquireRunRelease is scenario 1: acquire, sync, run, release. The slot
// directory no longer exists after cleanup.
func TestAcquireRunRelease(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.acquire()

	r := f.e.mustHostel("run", "--", "cat a.txt setup.txt")
	if r.stdout != "hello\nsetup-ran\n" {
		t.Fatalf("run output %q, want the synced file and the setup output", r.stdout)
	}
	if !strings.Contains(r.stderr, "exited with code 0") {
		t.Fatalf("run: no exit line: %s", r)
	}
	r = f.e.mustHostel("run", "--", `echo "$REPO_VAR $MACHINE_VAR $FAKE_DEVICE_ID $HOSTEL_DEVICES $HOSTEL_PLATFORM"; cat .env; test ! -e ignored.txt`)
	if !regexp.MustCompile(`^repo-value machine-value phone-[a-z0-9]+ phone fakedevice\nSECRET=1\n$`).MatchString(r.stdout) {
		t.Fatalf("lease environment: %q", r.stdout)
	}
	// The exit code of the remote command passes through, with the last line.
	r = f.e.hostel("run", "--", "exit 3")
	if r.code != 3 || !strings.HasSuffix(strings.TrimSpace(r.stderr), "exited with code 3") {
		t.Fatalf("run exit 3: %s", r)
	}
	// Several arguments run without a shell.
	r = f.e.mustHostel("run", "--", "printf", "%s|", "a b", "$HOME")
	if r.stdout != "a b|$HOME|" {
		t.Fatalf("run without a shell: %q", r.stdout)
	}
	// Commands are found in the job PATH: mise is only in hostel/bin.
	if r := f.e.mustHostel("run", "--", "mise", "--version"); !strings.HasPrefix(r.stdout, "2026.10.17") {
		t.Fatalf("mise in the job PATH: %s", r)
	}
	if !f.slotExists() {
		t.Fatal("the slot directory does not exist during the lease")
	}

	f.e.mustHostel("release")
	f.waitSlotFree()
	if f.slotExists() {
		t.Fatal("the slot directory still exists after cleanup")
	}
	if got := f.c.sh("cat /home/hostel/hostel/cache/fakedevice.log"); !strings.Contains(got, "prepare linux") || !strings.Contains(got, "reset linux") {
		t.Fatalf("platform scripts: %q, want prepare and reset", got)
	}
	if r := f.e.hostel("run", "--", "true"); r.code != 1 || !strings.Contains(r.stderr, "no active lease") {
		t.Fatalf("run after release: %s", r)
	}
}
