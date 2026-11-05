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

// fixture is a server with one static Docker room that has the test tools
// fake-screen and fake-sdk (its need), and a test repository.
type fixture struct {
	e *env
	s *serverProc
	c *container
}

const fixtureRepoConfig = `include = [".env"]
setup = ["echo setup-ran > setup.txt"]
env = { REPO_VAR = "repo-value" }
`

// fixtureOpts changes the fixture: server keys (top level of server.toml),
// keys of the machine "box", and the repository files.
type fixtureOpts struct {
	serverKeys  string
	machineKeys string
	files       map[string]string
	agentDevice bool
	// tools are extra or changed tool files: tool → file → content.
	tools map[string]map[string]string
	// machineTools are the tools of the machine box (default fake-screen).
	machineTools []string
	// noTools: the server has no test tools (the machine none), so its first
	// start imports server.toml with the machine.
	noTools bool
	// slotUsers are extra users of the room container.
	slotUsers []string
	// helperDir replaces the shared helper_dir (a test that changes it).
	helperDir string
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
	e.slotUsers = opts.slotUsers
	c := e.startStaticRoom("box")
	if opts.noTools {
		opts.machineTools = []string{}
	} else {
		e.useTools("fake-sdk", "fake-screen", "fake-desktop")
	}
	for name, files := range opts.tools {
		e.writeTool(name, files)
	}
	machineTools := opts.machineTools
	if machineTools == nil {
		machineTools = []string{"fake-screen"}
	}
	header := serverHeaderWith(opts.agentDevice)
	if opts.helperDir != "" {
		header = strings.Replace(header, fmt.Sprintf("helper_dir = %q", helperDir), fmt.Sprintf("helper_dir = %q", opts.helperDir), 1)
	}
	e.writeConfig(header + opts.serverKeys + fmt.Sprintf(`
[providers.home]
kind = "static"

[providers.home.machines.box]
address = %q
os = "linux"
tools = %s
env = { MACHINE_VAR = "machine-value" }
`, c.address(), tomlList(machineTools)) + opts.machineKeys)
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

// tomlList formats a TOML array of strings.
func tomlList(items []string) string {
	quoted := make([]string, len(items))
	for i, it := range items {
		quoted[i] = fmt.Sprintf("%q", it)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

var leaseIDRE = regexp.MustCompile(`Lease ([a-z0-9]+) is ready`)

// acquire runs "hostel acquire fake-screen" (or the tools in args) until the
// lease is ready and returns its ID. Exit code 75 (not finished, for example
// a slow setup on a loaded machine) runs the same command again, as an agent
// does, up to three times.
func (f *fixture) acquire(args ...string) string {
	f.e.t.Helper()
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		args = append([]string{"fake-screen"}, args...)
	}
	cmd := append([]string{"acquire", "--task", "e2e test"}, args...)
	r := f.e.hostel(cmd...)
	for i := 0; i < 2 && r.code == 75; i++ {
		r = f.e.hostel(cmd...)
	}
	if r.code != 0 {
		f.e.t.Fatalf("hostel %s: %s", strings.Join(cmd, " "), r)
	}
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
	r = f.e.mustHostel("run", "--", `echo "$REPO_VAR $MACHINE_VAR $FAKE_SCREEN_ID $HOSTEL_TOOLS"; cat .env; test ! -e ignored.txt`)
	if !regexp.MustCompile(`^repo-value machine-value option-a-[a-z0-9]+ fake-screen:option-a\nSECRET=1\n$`).MatchString(r.stdout) {
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
	if got := f.c.sh("cat /home/hostel/hostel/cache/fake-screen.log"); !strings.Contains(got, "start linux") || !strings.Contains(got, "stop linux") {
		t.Fatalf("tool scripts: %q, want start and stop", got)
	}
	if r := f.e.hostel("run", "--", "true"); r.code != 1 || !strings.Contains(r.stderr, "no active lease") {
		t.Fatalf("run after release: %s", r)
	}
}
