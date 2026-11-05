//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
)

// adminHostel runs a hostel command with the first admin's token.
func (e *env) adminHostel(args ...string) result {
	e.t.Helper()
	saved := e.vars["HOSTEL_TOKEN"]
	e.vars["HOSTEL_TOKEN"] = e.admin
	defer func() { e.vars["HOSTEL_TOKEN"] = saved }()
	return e.hostel(args...)
}

// configDoc returns the latest configuration document as an admin sees it.
func (s *serverProc) configDoc() protocol.ConfigDocument {
	s.e.t.Helper()
	var doc protocol.ConfigDocument
	if code := s.get("/v1/config", s.token, &doc); code != 200 {
		s.e.t.Fatalf("GET /v1/config: HTTP %d", code)
	}
	return doc
}

// applyDoc writes a document to a file and applies it as an admin.
func (e *env) applyDoc(doc string) result {
	e.t.Helper()
	file := filepath.Join(e.t.TempDir(), "config.toml")
	if err := os.WriteFile(file, []byte(doc), 0o600); err != nil {
		e.t.Fatal(err)
	}
	return e.adminHostel("config", "apply", file)
}

// TestConfigLive (M9): the providers, machines, and settings of server.toml
// move into the database at the first start (revision 1); an admin changes
// them from a client, and a change applies without a restart: the scheduler
// uses new machine tools at once. A member reads the document without secret
// values and cannot change it. An old base revision, an invalid document, and
// the removal of a machine with an active lease are refused; a free removed
// machine's room is retired.
func TestConfigLive(t *testing.T) {
	t.Parallel()
	// No test tools: they need a running server, and this test checks the
	// import at the first start.
	f := newFixture(t, &fixtureOpts{noTools: true})
	e, s := f.e, f.s

	// The import.
	local := readFile(t, filepath.Join(e.configDir(), "server.toml"))
	if strings.Contains(local, "[providers") || !strings.Contains(local, "hostel config edit") {
		t.Fatalf("server.toml after the import:\n%s", local)
	}
	if old := readFile(t, filepath.Join(e.configDir(), "server.toml.pre-m9")); !strings.Contains(old, "[providers.home.machines.box]") {
		t.Fatalf("server.toml.pre-m9:\n%s", old)
	}
	r := e.adminHostel("config")
	if r.code != 0 || !strings.Contains(r.stdout, "# Revision 1 ") || !strings.Contains(r.stdout, `MACHINE_VAR = "machine-value"`) {
		t.Fatalf("hostel config as an admin: %s", r)
	}
	if r := e.hostel("config", "history"); r.code != 0 || !strings.Contains(r.stdout, "import of server.toml") {
		t.Fatalf("history: %s", r)
	}

	// A member.
	r = e.hostel("config")
	if r.code != 0 || strings.Contains(r.stdout, "machine-value") || !strings.Contains(r.stdout, `MACHINE_VAR = '***'`) {
		t.Fatalf("hostel config as a member: %s", r)
	}
	if r := e.hostel("config", "settings", "set", "idle_timeout=20m"); r.code != 1 || !strings.Contains(r.stderr, "only an admin") {
		t.Fatalf("a member changes a setting: %s", r)
	}

	// A setting.
	if r := e.adminHostel("config", "settings", "set", "idle_timeout=20m"); r.code != 0 || !strings.Contains(r.stdout, "Revision 2 applies now") {
		t.Fatalf("settings set: %s", r)
	}
	if r := e.adminHostel("config", "settings"); !strings.Contains(r.stdout, `idle_timeout = "20m"`) {
		t.Fatalf("settings: %s", r)
	}
	if r := e.hostel("config", "history"); !strings.Contains(r.stdout, "2         admin") {
		t.Fatalf("history after a change: %s", r)
	}

	// New machine tools apply without a restart.
	e.useTools("fake-sdk")
	e.pushTool("fake-sdk")
	if r := e.hostel("acquire", "fake-sdk", "--task", "sdk"); r.code != 69 || !strings.Contains(r.stderr, "no machine has fake-sdk") {
		t.Fatalf("acquire of a tool that no machine has: %s, want exit code 69", r)
	}
	doc := s.configDoc().Document
	withTool := strings.Replace(doc, "tools = []", `tools = ["fake-sdk"]`, 1)
	if r := e.applyDoc(withTool); r.code != 0 || !strings.Contains(r.stdout, "Revision 3") {
		t.Fatalf("apply new tools: %s", r)
	}
	s.waitRoom("home/box", protocol.RoomRunning, roomReady)
	lease := f.acquire("fake-sdk")
	// The address of a room with a lease stays.
	if r := e.applyDoc(strings.Replace(withTool, "hostel@127.0.0.1:", "hostel@localhost:", 1)); r.code != 1 ||
		!strings.Contains(r.stderr, "slots, address, connect, os, and the lease's tools stay while the room home/box has the active lease "+lease) {
		t.Fatalf("change the address of a room with a lease: %s", r)
	}

	// An old base revision.
	body := strings.NewReader(`{"base_revision": 2, "document": ""}`)
	if code := s.send("PUT", "/v1/config", s.token, body, nil); code != 409 {
		t.Fatalf("PUT with an old base revision: HTTP %d, want 409", code)
	}
	// An invalid document: the error names the revision and the key.
	if r := e.applyDoc(strings.Replace(withTool, `os = "linux"`, `os = "plan9"`, 1)); r.code != 1 ||
		!strings.Contains(r.stderr, "configuration revision 4: providers.home.machines.box.os") {
		t.Fatalf("an invalid document: %s", r)
	}
	// The removal of a machine with an active lease.
	removed := withTool[:strings.Index(withTool, "[providers.home.machines.box]")]
	if r := e.applyDoc(removed); r.code != 1 || !strings.Contains(r.stderr, "active lease "+lease) || !strings.Contains(r.stderr, "drain") {
		t.Fatalf("remove a machine with an active lease: %s", r)
	}
	e.mustHostel("release")
	e.waitFor("the lease ends", time.Minute, func() bool {
		var l protocol.Lease
		s.get("/v1/leases/"+lease, s.token, &l)
		return l.State == protocol.LeaseReleased
	})
	if r := e.applyDoc(removed); r.code != 0 {
		t.Fatalf("remove a free machine: %s", r)
	}
	s.waitRoom("home/box", protocol.RoomRetired, time.Minute)

	// A document key in server.toml after the import stops the server.
	s.stop()
	if err := os.WriteFile(filepath.Join(e.configDir(), "server.toml"), []byte(local+"idle_timeout = \"1m\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := e.hostel("server", "run"); r.code != 1 || !strings.Contains(r.stderr, "idle_timeout: is in the configuration in the database") {
		t.Fatalf("server run with a document key in server.toml: %s", r)
	}
}

// TestToolsLive (M10e): an admin adds a tool of their own from a client
// (new, push), adds it to a machine, and a lease for it runs after the room
// installs it, without a restart; pull gives the same files; a tool that a
// machine lists cannot be removed, and a machine cannot stop listing a tool of
// its active lease; a default tool can be changed, reset, and removed.
func TestToolsLive(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	e, s := f.e, f.s

	r := e.hostel("config", "tools")
	if r.code != 0 || !regexp.MustCompile(`fake-screen\s+yes\s+home/box\s+own \(rev 1, admin`).MatchString(r.stdout) ||
		!regexp.MustCompile(`fake-sdk\s+-\s+home/box\s+own`).MatchString(r.stdout) ||
		!regexp.MustCompile(`ios-simulator\s+yes\s+-\s+default`).MatchString(r.stdout) {
		t.Fatalf("config tools: %s", r)
	}

	dir := filepath.Join(t.TempDir(), "tvos")
	if r := e.adminHostel("config", "tools", "new", "tvos", dir, "--screen"); r.code != 0 {
		t.Fatalf("tools new: %s", r)
	}
	if r := e.hostel("config", "tools", "push", dir); r.code != 1 || !strings.Contains(r.stderr, "only an admin") {
		t.Fatalf("a member pushes a tool: %s", r)
	}
	if r := e.adminHostel("config", "tools", "push", dir); r.code != 0 {
		t.Fatalf("tools push: %s", r)
	}
	doc := s.configDoc().Document
	if r := e.applyDoc(strings.Replace(doc, `tools = ["fake-screen"]`, `tools = ["fake-screen", "tvos"]`, 1)); r.code != 0 {
		t.Fatalf("add tvos to the machine: %s", r)
	}
	e.waitFor("a tvos lease", roomReady, func() bool {
		return e.hostel("acquire", "tvos", "--task", "tvos", "--wait", "5s").code == 0
	})
	if r := e.mustHostel("run", "--", "echo \"$SCREEN\""); strings.TrimSpace(r.stdout) != "tvos-1" {
		t.Fatalf("the fact of the tvos screen: %q", r.stdout)
	}
	if r := e.adminHostel("config", "tools", "remove", "tvos"); r.code != 1 || !strings.Contains(r.stderr, "the machine home/box lists the tool tvos") {
		t.Fatalf("remove a tool that a machine lists: %s", r)
	}
	// A tool keeps its version while a lease uses it.
	if r := e.adminHostel("config", "tools", "push", dir); r.code != 1 || !strings.Contains(r.stderr, "uses the tool tvos") {
		t.Fatalf("push a tool that a lease uses: %s", r)
	}
	// The machine cannot stop listing tvos while the lease uses it.
	if r := e.applyDoc(doc); r.code != 1 || !strings.Contains(r.stderr, "the lease's tools stay while the room home/box has the active lease") {
		t.Fatalf("remove tvos from the machine during its lease: %s", r)
	}
	e.mustHostel("release")

	pulled := filepath.Join(t.TempDir(), "pulled")
	if r := e.hostel("config", "tools", "pull", "tvos", pulled); r.code != 0 {
		t.Fatalf("tools pull: %s", r)
	}
	for _, name := range []string{"tool.toml", "start.linux.sh", "check.linux.sh", "GUIDE.md"} {
		if readFile(t, filepath.Join(dir, name)) != readFile(t, filepath.Join(pulled, name)) {
			t.Fatalf("pulled %s differs", name)
		}
	}

	// A default tool: changed, back to the default, removed.
	ios := filepath.Join(t.TempDir(), "ios-simulator")
	e.mustHostel("config", "tools", "pull", "ios-simulator", ios)
	if err := os.WriteFile(filepath.Join(ios, "GUIDE.md"), []byte("our iOS\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := e.adminHostel("config", "tools", "push", ios); r.code != 0 {
		t.Fatalf("push a changed default: %s", r)
	}
	if r := e.hostel("config", "tools"); !regexp.MustCompile(`ios-simulator\s+yes\s+-\s+changed \(rev 1, admin`).MatchString(r.stdout) {
		t.Fatalf("a changed default: %s", r)
	}
	if r := e.adminHostel("config", "tools", "reset", "ios-simulator"); r.code != 0 {
		t.Fatalf("reset: %s", r)
	}
	if r := e.adminHostel("config", "tools", "remove", "android-emulator"); r.code != 0 {
		t.Fatalf("remove a default: %s", r)
	}
	r = e.hostel("config", "tools")
	if !regexp.MustCompile(`ios-simulator\s+yes\s+-\s+default\n`).MatchString(r.stdout) || strings.Contains(r.stdout, "android-emulator") {
		t.Fatalf("after reset and remove: %s", r)
	}
}

// TestConfigRooms (M9): an admin lists the machines with their rooms, changes a
// machine's tools (a new lease uses them at once), drains and undrains its
// room, adds and removes a provider, and removes the machine; the targeted
// commands keep the rest of the document. A member cannot change them.
func TestConfigRooms(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	e, s := f.e, f.s

	r := e.hostel("config", "rooms")
	if r.code != 0 || !regexp.MustCompile(`home/box\s+static\s+linux\s+fake-screen\S*\s+home/box \(running\)`).MatchString(r.stdout) {
		t.Fatalf("config rooms: %s", r)
	}
	if r := e.hostel("config", "rooms", "set", "box", "tools=fake-screen,fake-desktop"); r.code != 1 || !strings.Contains(r.stderr, "only an admin") {
		t.Fatalf("a member sets tools: %s", r)
	}
	if r := e.adminHostel("config", "rooms", "set", "box", "tools=fake-screen, fake-desktop"); r.code != 0 {
		t.Fatalf("rooms set: %s", r)
	}
	if doc := s.configDoc().Document; !strings.Contains(doc, `tools = ["fake-screen", "fake-desktop"]`) || !strings.Contains(doc, `MACHINE_VAR = "machine-value"`) {
		t.Fatalf("the document after rooms set:\n%s", doc)
	}
	lease := f.acquire("fake-desktop")
	e.mustHostel("release")
	e.waitFor("the lease ends", time.Minute, func() bool {
		var l protocol.Lease
		s.get("/v1/leases/"+lease, s.token, &l)
		return l.State == protocol.LeaseReleased
	})

	// Drain: a new lease waits; undrain: it starts.
	if r := e.adminHostel("config", "rooms", "drain", "home/box"); r.code != 0 {
		t.Fatalf("drain: %s", r)
	}
	if r := e.hostel("config", "rooms"); !strings.Contains(r.stdout, "running, drained") {
		t.Fatalf("rooms after drain: %s", r)
	}
	if r := e.hostel("acquire", "fake-screen", "--task", "drained", "--wait", "1s"); r.code != 75 {
		t.Fatalf("acquire on a drained room: %s, want exit code 75", r)
	}
	if r := e.adminHostel("config", "rooms", "undrain", "box"); r.code != 0 {
		t.Fatalf("undrain: %s", r)
	}
	f.acquire()
	e.mustHostel("release")

	// Providers.
	if r := e.adminHostel("config", "providers", "add", "lab", "--kind", "static"); r.code != 0 {
		t.Fatalf("providers add: %s", r)
	}
	if r := e.hostel("config", "providers"); !regexp.MustCompile(`home\s+static\s+box\n`).MatchString(r.stdout) ||
		!regexp.MustCompile(`lab\s+static\s+-\n`).MatchString(r.stdout) {
		t.Fatalf("providers: %s", r)
	}
	if r := e.adminHostel("config", "providers", "remove", "lab"); r.code != 0 {
		t.Fatalf("providers remove: %s", r)
	}

	// The machine.
	e.waitFor("the room is free", time.Minute, func() bool {
		return s.room("home/box").Slots[0].State == protocol.SlotFree
	})
	if r := e.adminHostel("config", "rooms", "remove", "box"); r.code != 0 {
		t.Fatalf("rooms remove: %s", r)
	}
	s.waitRoom("home/box", protocol.RoomRetired, time.Minute)
	if doc := s.configDoc().Document; strings.Contains(doc, "machines.box") || !strings.Contains(doc, "[providers.home]") {
		t.Fatalf("the document after rooms remove:\n%s", doc)
	}
}
