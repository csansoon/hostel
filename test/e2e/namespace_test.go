//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
)

// devboxMachine is a devbox machine of the namespace provider "ns".
const devboxMachine = `
[providers.ns.machines.box]
spec = { product = "devbox", platform = "linux", size = "s", image = "builtin:default" }
tools = ["fake-screen"]
max_lifetime = "3h"
`

// useFakeDevbox puts the fake devbox CLI (test/fakeprovider under the name
// "devbox") first on the PATH of the server, with its settings.
func (e *env) useFakeDevbox(vars map[string]string) string {
	e.t.Helper()
	bin := filepath.Join(e.home, "fake-bin")
	state := filepath.Join(e.home, "fake-devbox")
	for _, d := range []string{bin, state} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			e.t.Fatal(err)
		}
	}
	if err := os.Symlink(fakeProviderBin, filepath.Join(bin, "devbox")); err != nil {
		e.t.Fatal(err)
	}
	e.vars["PATH"] = bin + string(os.PathListSeparator) + os.Getenv("PATH")
	e.vars["FAKE_RUN_ID"] = e.runID
	e.vars["FAKE_STATE_DIR"] = state
	for k, v := range vars {
		e.vars[k] = v
	}
	return state
}

// newDevboxFixture starts a server with the namespace provider "ns" and the
// fake devbox CLI.
func newDevboxFixture(t *testing.T, providerKeys string, vars map[string]string) (*fixture, string) {
	t.Helper()
	e := newEnv(t)
	e.init()
	e.useTools("fake-sdk", "fake-screen")
	state := e.useFakeDevbox(vars)
	e.writeConfig(serverHeader() + "[providers.ns]\nkind = \"namespace\"\n" + providerKeys + devboxMachine)
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	e.useGuestToken()
	e.newRepo(defaultRepoFiles())
	return &fixture{e: e, s: s}, state
}

func (f *fixture) nsRooms() []protocol.RoomStatus {
	var out []protocol.RoomStatus
	for _, r := range f.s.status().Rooms {
		if r.Provider == "ns" {
			out = append(out, r)
		}
	}
	return out
}

// TestNamespaceDevbox: leases on an ephemeral devbox through the devbox SSH
// proxy and the devbox key; check only for the tools that the room has
// installed; a task marker only while a job runs, and cleanup
// removes the marker of a killed job supervisor; access after the key file is
// lost; a destroyed room takes its key files with it.
func TestNamespaceDevbox(t *testing.T) {
	t.Parallel()
	f, state := newDevboxFixture(t, "warm_running = \"10m\"\n", nil)
	// A first lease without tools: the on-demand room has not installed the
	// screen tool fake-screen yet, so cleanup does not run its check script
	// (it needs the result of its install script).
	f.e.mustHostel("acquire", "--task", "no tools")
	f.e.mustHostel("release")
	f.e.waitFor("slot free after the lease without tools", time.Minute, func() bool {
		rs := f.nsRooms()
		return len(rs) == 1 && rs[0].State == protocol.RoomRunning && rs[0].Slots[0].State == protocol.SlotFree && rs[0].Slots[0].LeaseID == ""
	})
	f.acquire()
	rooms := f.nsRooms()
	if len(rooms) != 1 {
		t.Fatalf("rooms: %+v", rooms)
	}
	room := rooms[0].Name
	args, err := os.ReadFile(filepath.Join(state, "create-"+room+".args"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--platform linux", "--size s", "--image builtin:default", "--ephemeral",
		"--auto_stop_idle_timeout 15m", "--no_checkout", "--purpose hostel room " + room} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("devbox create args %q do not contain %q", args, want)
		}
	}
	markers := func() string { return f.roomSh(room, "ls /var/run/devbox/tasks | tr '\\n' ' '") }
	r := f.e.mustHostel("run", "--detach", "--", "sleep 300")
	job := strings.TrimSpace(r.stdout)
	f.e.waitFor("the marker of the running job only", time.Minute, func() bool { return markers() == "hostel-a."+job })
	f.e.mustHostel("stop", job)
	f.e.waitFor("no marker after the job", time.Minute, func() bool { return markers() == "" })
	// A killed supervisor leaves its marker; cleanup removes it.
	r = f.e.mustHostel("run", "--detach", "--", "sleep 300")
	job = strings.TrimSpace(r.stdout)
	f.roomSh(room, "kill -9 $(sed 's/.*\"supervisor_pid\":\\([0-9]*\\).*/\\1/' /home/hostel/hostel/slots/a/jobs/"+job+"/supervisor.json)")
	if got := markers(); got != "hostel-a."+job {
		t.Fatalf("markers after the supervisor was killed: %q", got)
	}
	// The CLI writes the key again; a new server connection needs it.
	key := filepath.Join(f.e.home, ".namespace", "ssh", room+".devbox.namespace.key")
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	f.s = f.s.restart()
	if r := f.e.mustHostel("run", "--", "echo ok"); r.stdout != "ok\n" {
		t.Fatalf("run after the key file was lost: %q", r.stdout)
	}
	f.e.mustHostel("release")
	f.e.waitFor("slot free", time.Minute, func() bool {
		rs := f.nsRooms()
		return len(rs) == 1 && rs[0].Slots[0].State == protocol.SlotFree && rs[0].Slots[0].LeaseID == ""
	})
	if got := markers(); got != "" {
		t.Fatalf("markers after cleanup: %q", got)
	}
	f.admin("admin", "room", "destroy", fmt.Sprint(rooms[0].ID))
	f.e.waitFor("room destroyed", 2*time.Minute, func() bool { return len(f.nsRooms()) == 0 && len(f.e.fakeRooms()) == 0 })
	if _, err := os.Stat(key); !os.IsNotExist(err) {
		t.Fatalf("the key file stays after destroy: %v", err)
	}
}

// TestNamespaceLimit: the devbox limit error of the CLI is a provider limit; a
// devbox without the prefix stays.
func TestNamespaceLimit(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	// Another devbox of the account (not Hostel's) fills the limit of 1.
	foreign := "foreign-" + e.runID
	if _, err := docker("run", "-d", "--name", foreign, "--label", dockerLabel+"="+e.runID, "--label", "hostel-fake=1", roomImage); err != nil {
		t.Fatal(err)
	}
	e.init()
	e.useTools("fake-sdk", "fake-screen")
	e.useFakeDevbox(map[string]string{"FAKE_LIMIT": "1"})
	e.writeConfig(serverHeader() + "limit_retry = \"3s\"\n[providers.ns]\nkind = \"namespace\"\n" + devboxMachine)
	e.startServer(e.vars["HOSTEL_TOKEN"])
	e.useGuestToken()
	e.newRepo(defaultRepoFiles())
	// The same command continues the same lease until the limit shows.
	var r result
	e.waitFor("the provider limit", 2*time.Minute, func() bool {
		r = e.hostel("acquire", "fake-screen", "--task", "limited", "--wait", "5s")
		if r.code != 75 {
			t.Fatalf("acquire at the devbox limit: %s", r)
		}
		return strings.Contains(r.stderr, "provider limit reached")
	})
	if !slices.Contains(e.fakeRooms(), foreign) {
		t.Fatal("reconcile removed a devbox without the prefix")
	}
	if _, err := docker("rm", "-f", foreign); err != nil {
		t.Fatal(err)
	}
	// After limit_retry the lease gets a new devbox; as a guest does, run the
	// same command again until it is ready.
	e.waitFor("the lease ready after the limit", 3*time.Minute, func() bool {
		r = e.hostel("acquire", "fake-screen", "--task", "limited", "--wait", "20s")
		if r.code != 0 && r.code != 75 {
			t.Fatalf("acquire after the limit: %s", r)
		}
		return r.code == 0
	})
}

// TestNamespaceKill: after kill -9 of the server during "devbox create", the
// restarted server finds the devbox in the list, does not create another one,
// and waits until it accepts SSH. No devbox remains after the lease.
func TestNamespaceKill(t *testing.T) {
	t.Parallel()
	// The devbox boots for 20 s, a wide window for the kill also on a slow
	// machine.
	f, state := newDevboxFixture(t, "warm_running = \"3s\"\n", map[string]string{"FAKE_DELAY": "20s"})
	f.acquireUntil(protocol.LeaseProvisioning)
	f.e.waitFor("devbox create started", time.Minute, func() bool {
		boots, _ := filepath.Glob(filepath.Join(state, "*.boot"))
		return len(boots) == 1
	})
	before := f.nsRooms()
	f.s = f.s.restartAfterKill()
	f.acquire()
	// The same room: its first connection worked, and the server did not
	// destroy it and create another one.
	if after := f.nsRooms(); len(before) != 1 || len(after) != 1 || after[0].ID != before[0].ID {
		t.Fatalf("rooms before the kill %+v, after %+v; want the same one", before, after)
	}
	if rooms := f.e.fakeRooms(); len(rooms) != 1 {
		t.Fatalf("devboxes during the lease: %v, want 1", rooms)
	}
	calls, err := os.ReadFile(filepath.Join(state, "create-"+before[0].Name+".args"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(calls), "\n"); n != 1 {
		t.Fatalf("%d devbox create calls, want 1", n)
	}
	if r := f.e.mustHostel("run", "--", "echo ok"); r.stdout != "ok\n" {
		t.Fatalf("run: %q", r.stdout)
	}
	f.e.mustHostel("release")
	f.e.waitFor("no devbox left", 2*time.Minute, func() bool { return len(f.nsRooms()) == 0 && len(f.e.fakeRooms()) == 0 })
}

// TestNamespaceCreateError: a ResourceExhausted error without the devbox limit
// text (for example a rate limit) is a failed create, not a provider limit.
func TestNamespaceCreateError(t *testing.T) {
	t.Parallel()
	f, _ := newDevboxFixture(t, "", map[string]string{
		"FAKE_CREATE_ERROR": "rpc error: code = ResourceExhausted desc = rate limit exceeded (rid=fake)"})
	r := f.e.hostel("acquire", "fake-screen", "--task", "rate limited", "--wait", "60s")
	if r.code != 1 || !strings.Contains(r.stderr, "the provider failed to create a room") || !strings.Contains(r.stderr, "rate limit exceeded") {
		t.Fatalf("acquire with a rate limit on create: %s", r)
	}
}

// TestNamespaceLogin: the server does not start when the devbox CLI is not
// logged in.
func TestNamespaceLogin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.useFakeDevbox(map[string]string{"FAKE_LOGGED_OUT": "1"})
	e.initServer(serverHeader() + "[providers.ns]\nkind = \"namespace\"\n" + devboxMachine)
	e.useTools("fake-sdk", "fake-screen")
	r := e.hostel("server", "run")
	if r.code != 1 || !strings.Contains(r.stderr, `providers.ns: the devbox CLI is not ready (install it and run "devbox login" as the server user)`) ||
		!strings.Contains(r.stderr, "not logged in; run devbox login") {
		t.Fatalf("server run without a devbox login: %s", r)
	}
}
