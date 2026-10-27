//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
)

// instanceMachine is an instance machine of the namespace provider "ns".
const instanceMachine = `
[providers.ns.machines.box]
spec = { product = "instance", machine_type = "linux/amd64:4x16", image = "ubuntu:24.04" }
platforms = ["fakedevice"]
`

// useFakeNsc puts the fake nsc CLI (test/fakeprovider under the name "nsc")
// first on the PATH of the server, with its settings.
func (e *env) useFakeNsc(vars map[string]string) string {
	e.t.Helper()
	bin := filepath.Join(e.home, "fake-bin")
	state := filepath.Join(e.home, "fake-nsc")
	for _, d := range []string{bin, state} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			e.t.Fatal(err)
		}
	}
	if err := os.Symlink(fakeProviderBin, filepath.Join(bin, "nsc")); err != nil {
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

// fakeNsc runs the fake nsc CLI as the server does.
func (e *env) fakeNsc(args ...string) string {
	e.t.Helper()
	cmd := exec.Command(filepath.Join(e.home, "fake-bin", "nsc"), args...)
	cmd.Env = e.environ()
	out, err := cmd.Output()
	if err != nil {
		e.t.Fatalf("nsc %s: %v", args[0], err)
	}
	return string(out)
}

// instances returns the room names of the fake instances.
func (e *env) instances() []string {
	e.t.Helper()
	var list []struct {
		Labels map[string]string `json:"labels"`
	}
	if err := json.Unmarshal([]byte(e.fakeNsc("list", "-o", "json")), &list); err != nil {
		e.t.Fatal(err)
	}
	var names []string
	for _, i := range list {
		names = append(names, i.Labels["hostel.name"])
	}
	return names
}

// newInstanceFixture starts a server with the namespace provider "ns" and the
// fake nsc CLI; serverKeys go to the top level of server.toml.
func newInstanceFixture(t *testing.T, serverKeys, providerKeys string, vars map[string]string, files map[string]string) (*fixture, string) {
	t.Helper()
	e := newEnv(t)
	e.init()
	e.usePlatform("fakedevice")
	state := e.useFakeNsc(vars)
	e.writeConfig(serverHeader() + serverKeys + "[providers.ns]\nkind = \"namespace\"\n" + providerKeys + instanceMachine)
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	e.useGuestToken()
	if files == nil {
		files = defaultRepoFiles()
	}
	e.newRepo(files)
	return &fixture{e: e, s: s}, state
}

// TestNamespaceInstance: a lease on a Namespace instance. The server creates a
// bare instance with a duration and its labels, starts the room container
// with the server key, and reaches its SSH server through "nsc instance
// port-forward" (also a tunnel and after a restart). A second instance with
// the room's name is destroyed, and a leaked instance with the server's prefix is
// destroyed, and so is the room's instance at the end.
func TestNamespaceInstance(t *testing.T) {
	t.Parallel()
	free := freePort(t)
	f, state := newInstanceFixture(t, "", "warm_running = \"10m\"\n", nil, map[string]string{
		"hostel.toml": tunnelRepoConfig,
		".gitignore":  ".env\n",
		".env":        fmt.Sprintf("PORT=%d\n", free),
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
	for _, want := range []string{"--bare", "--machine_type linux/amd64:4x16", "--duration ", "--label hostel.name=" + room,
		"--label hostel.machine=box", "--purpose hostel room " + room} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("nsc create args %q do not contain %q", args, want)
		}
	}
	if n := strings.Count(string(args), "\n"); n != 1 {
		t.Fatalf("%d nsc create calls, want 1", n)
	}
	if run, err := os.ReadFile(filepath.Join(state, "docker-"+room+".args")); err != nil || !strings.Contains(string(run), "--network host") ||
		!strings.Contains(string(run), " ubuntu:24.04 -c ") {
		t.Fatalf("docker run on the instance: %q, %v", run, err)
	}
	if r := f.e.mustHostel("run", "--", "id -un"); r.stdout != "hostel\n" {
		t.Fatalf("room user: %q", r.stdout)
	}
	// A tunnel needs remote forwards, which the instance's own SSH server
	// refuses; the room container's sshd accepts them.
	localHTTP(t, fmt.Sprintf("127.0.0.1:%d", free), "guest")
	if r := f.roomGet("$HOSTEL_TUNNEL_API_PORT", "/api"); !strings.Contains(r.stdout, "hello from guest /api") {
		t.Fatalf("tunnel: %s", r)
	}

	// A leaked instance with the server's prefix.
	sid := strings.Split(room, "-")[1]
	leaked := fmt.Sprintf("hostel-%s-999", sid)
	key := filepath.Join(f.e.home, "leak.pub")
	f.e.writeFile(key, "ssh-ed25519 AAAA leak\n")
	f.e.fakeNsc("create", "--bare", "--machine_type", "linux/amd64:2x4", "--duration", "10m", "--ssh_key", key,
		"--label", "hostel.name="+leaked, "--label", "hostel.machine=box", "-o", "json")
	f.e.waitFor("the leaked instance destroyed", time.Minute, func() bool {
		names := f.e.instances()
		return len(names) == 1 && names[0] == room
	})
	// A second instance with the room's name, as a create call of a killed
	// server can make late: reconcile keeps only the recorded one.
	f.e.fakeNsc("create", "--bare", "--machine_type", "linux/amd64:2x4", "--duration", "10m", "--ssh_key", key,
		"--label", "hostel.name="+room, "--label", "hostel.machine=box", "-o", "json")
	f.e.waitFor("the second instance destroyed", time.Minute, func() bool { return len(f.e.instances()) == 1 })
	if r := f.e.mustHostel("run", "--", "echo still-here"); r.stdout != "still-here\n" {
		t.Fatalf("run after the second instance was destroyed: %q", r.stdout)
	}

	f.s = f.s.restart()
	if r := f.e.mustHostel("run", "--", "echo after-restart"); r.stdout != "after-restart\n" {
		t.Fatalf("run after the restart: %q", r.stdout)
	}
	f.e.mustHostel("release")
	f.e.waitFor("slot free", time.Minute, func() bool {
		rs := f.nsRooms()
		return len(rs) == 1 && rs[0].Slots[0].State == protocol.SlotFree && rs[0].Slots[0].LeaseID == ""
	})
	f.admin("admin", "room", "destroy", fmt.Sprint(rooms[0].ID))
	f.e.waitFor("instance destroyed", 2*time.Minute, func() bool {
		return len(f.nsRooms()) == 0 && len(f.e.instances()) == 0 && len(f.e.fakeRooms()) == 0
	})
}

// TestNamespaceInstanceKill: after kill -9 of the server during "nsc create",
// the restarted server finds the instance by its label, does not create
// another one, and starts the room container on it.
func TestNamespaceInstanceKill(t *testing.T) {
	t.Parallel()
	// The create takes 20 s, a wide window for the kill also on a slow machine.
	f, state := newInstanceFixture(t, "", "warm_running = \"3s\"\n", map[string]string{"FAKE_DELAY": "20s"}, nil)
	f.acquireUntil(protocol.LeaseProvisioning)
	f.e.waitFor("nsc create started", time.Minute, func() bool { return len(f.e.instances()) == 1 })
	before := f.nsRooms()
	f.s = f.s.restartAfterKill()
	f.acquire()
	if after := f.nsRooms(); len(before) != 1 || len(after) != 1 || after[0].ID != before[0].ID {
		t.Fatalf("rooms before the kill %+v, after %+v; want the same one", before, after)
	}
	if names := f.e.instances(); len(names) != 1 {
		t.Fatalf("instances during the lease: %v, want 1", names)
	}
	calls, err := os.ReadFile(filepath.Join(state, "create-"+before[0].Name+".args"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(calls), "\n"); n != 1 {
		t.Fatalf("%d nsc create calls, want 1", n)
	}
	if r := f.e.mustHostel("run", "--", "echo ok"); r.stdout != "ok\n" {
		t.Fatalf("run: %q", r.stdout)
	}
	f.e.mustHostel("release")
	f.e.waitFor("no instance left", 2*time.Minute, func() bool { return len(f.nsRooms()) == 0 && len(f.e.instances()) == 0 })
}

// TestNamespaceInstanceLimit: at the plan limit "nsc create" blocks; after its
// deadline the server reports the provider limit, and no instance remains.
func TestNamespaceInstanceLimit(t *testing.T) {
	t.Parallel()
	f, _ := newInstanceFixture(t, "limit_retry = \"3s\"\n", "", map[string]string{"FAKE_LIMIT": "0"}, nil)
	var r result
	// The create deadline is 2 minutes.
	f.e.waitFor("the provider limit", 4*time.Minute, func() bool {
		r = f.e.hostel("acquire", "fakedevice", "--task", "limited", "--wait", "5s")
		if r.code != 75 {
			t.Fatalf("acquire at the plan limit: %s", r)
		}
		return strings.Contains(r.stderr, "provider limit reached")
	})
	if names := f.e.instances(); len(names) != 0 {
		t.Fatalf("instances at the limit: %v", names)
	}
}

// TestNamespaceInstanceLogin: the server does not start without an nsc login.
func TestNamespaceInstanceLogin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.useFakeNsc(map[string]string{"FAKE_LOGGED_OUT": "1"})
	e.initServer(serverHeader() + "[providers.ns]\nkind = \"namespace\"\n" + instanceMachine)
	e.usePlatform("fakedevice")
	r := e.hostel("server", "run")
	if r.code != 1 || !strings.Contains(r.stderr, `providers.ns: the nsc CLI is not ready (install it and run "nsc login" as the server user)`) ||
		!strings.Contains(r.stderr, "You are not logged in") {
		t.Fatalf("server run without an nsc login: %s", r)
	}
}
