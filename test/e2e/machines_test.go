//go:build e2e

package e2e

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/csansoon/hostel/internal/hashes"
	"github.com/csansoon/hostel/internal/protocol"
)

const machineReady = 3 * time.Minute

// installScript records each call, with values from its environment, in the
// machine cache.
const installScript = `#!/bin/sh
echo "install $HOSTEL_TOOL var=$MACHINE_VAR mise=$MISE_DATA_DIR home=$PROBE_HOME pwd=$(pwd)" >> "$HOSTEL_CACHE_DIR/install.log"
`

func staticMachineConfig(c *container, tools string) string {
	return serverHeader() + fmt.Sprintf(`
[providers.home]
kind = "static"

[providers.home.machines.box]
address = %q
os = "linux"
tools = [%s]
env = { MACHINE_VAR = "machine-value" }
`, c.address(), tools)
}

// TestStaticMachine: a static machine is probed, gets the helper, mise, and the
// files of its tools, installs and checks them, and has them in its inventory
// (scenario 22, static part). A restart does not install again; changed
// scripts do; a tool that the machine no longer lists leaves the machine and its
// inventory. Init
// rejects a server name, TLS mode, listen address, or public address that the
// server would reject at start, and init and invite reject an invalid user name.
// The schema that init writes has no aging. server invite checks the
// addresses that go into the code.
func TestStaticMachine(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, c := range []struct{ args, want []string }{
		{[]string{"--server-id", "ip-test"}, []string{`"ip-test" must be 1 to 20 lowercase letters and digits`}},
		{[]string{"--tls", "selfsigned"}, []string{"--tls:", `"selfsigned" must be "self-signed"`}},
		{[]string{"--url", "http://100.80.1.2:7410"}, []string{"--url:", `"http://100.80.1.2:7410" must start with https://`}},
		{[]string{"--url", "https://100.80.1.2:7410/?x"}, []string{"--url:", `"https://100.80.1.2:7410/?x" is not a server address`}},
		{[]string{"--url", "https://100.80.1.2:7410#"}, []string{"--url:", `"https://100.80.1.2:7410#" is not a server address`}},
		{[]string{"--user", "a/b"}, []string{"--user:", `"a/b" is not a valid name`}},
		{[]string{"--listen", "localhost"}, []string{"--listen:", `"localhost" is not a host:port address`}},
		{[]string{"--tls", "off", "--url", "http://100.80.1.2:7410"}, []string{"--url:", `"http://100.80.1.2:7410" must start with https://`}},
		{[]string{"--tls", "off", "--listen", "0.0.0.0:7410"}, []string{"--url: give the address that clients use"}},
	} {
		r := e.hostel(append([]string{"server", "init", "--user", "admin"}, c.args...)...)
		for _, w := range c.want {
			if r.code != 1 || !strings.Contains(r.stderr, w) {
				t.Fatalf("server init %v: want exit code 1 and %q: %s", c.args, w, r)
			}
		}
		if _, err := os.Stat(filepath.Join(e.configDir(), "server.toml")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("server init %v wrote server.toml (err %v)", c.args, err)
		}
	}
	// Behind a proxy with TLS, init writes the gateway on the public address;
	// --helper-dir writes the directory of install.sh --server (M10c).
	proxy := newEnv(t)
	if r := proxy.hostel("server", "init", "--user", "admin", "--tls", "off", "--listen", "0.0.0.0:7410",
		"--url", "https://hostel.example.com", "--helper-dir", "/opt/hostel/helpers"); r.code != 0 {
		t.Fatalf("server init behind a proxy: %s", r)
	}
	toml := readFile(t, filepath.Join(proxy.configDir(), "server.toml"))
	for _, want := range []string{`gateway_public = "wss://hostel.example.com/v1/gateway"`, `helper_dir = "/opt/hostel/helpers"`} {
		if !strings.Contains(toml, want) {
			t.Fatalf("server.toml from init behind a proxy has no %s:\n%s", want, toml)
		}
	}
	if r := proxy.hostel("admin", "config", "check"); r.code != 0 {
		t.Fatalf("admin config check of the proxy server.toml: %s", r)
	}
	e.init()
	if r := e.hostel("server", "invite", "--name", "a/b"); r.code != 1 || !strings.Contains(r.stderr, `"a/b" is not a valid name`) {
		t.Fatalf("server invite with an invalid name: %s", r)
	}
	// server invite checks the addresses that go into the code (tls = "off":
	// https:// only, http:// only on loopback).
	e.writeConfig(strings.Replace(serverHeader(), `tls = "self-signed"`+"\n"+`public_urls = ["https://127.0.0.1:0"]`,
		`tls = "off"`+"\n"+`public_urls = ["http://100.80.1.2:7410"]`, 1) + staticTemplate)
	if r := e.hostel("server", "invite"); r.code != 1 || !strings.Contains(r.stderr, `public_urls[0]: "http://100.80.1.2:7410" must start with https://`) {
		t.Fatalf("server invite with an http:// public URL: %s", r)
	}
	// The schema that init writes has the M8 keys, and not aging (removed).
	schema, err := os.ReadFile(filepath.Join(e.configDir(), "hostel.schema.json"))
	if err != nil || !strings.Contains(string(schema), `"public_urls"`) || strings.Contains(string(schema), `"aging"`) {
		t.Fatalf("hostel.schema.json (err %v): want public_urls and no aging", err)
	}
	c := e.startStaticMachine("box")
	e.writeTool("probe", map[string]string{
		"tool.toml":        "description = \"Probe\"\nenv = { PROBE_HOME = \"{cache}/probe\" }\n",
		"install.linux.sh": installScript,
		"install.macos.sh": "#!/bin/sh\nexit 1\n",
		"check.linux.sh":   "#!/bin/sh\necho checking\necho 2.0\n",
	})
	e.writeTool("probe-two", map[string]string{"tool.toml": "description = \"Second probe\"\n", "check.linux.sh": "#!/bin/sh\necho 3.1\n"})
	e.writeConfig(staticMachineConfig(c, `"probe", "probe-two"`))
	s := e.startServer(e.vars["HOSTEL_TOKEN"])

	r := s.waitMachine("home/box", protocol.MachineRunning, machineReady)
	if r.OS != "linux" || r.Arch != dockerArch {
		t.Fatalf("machine = %+v, want linux/%s", r, dockerArch)
	}
	if p := inventoryOf(r, "probe"); p == nil || p.State != "ready" || !slices.Equal(p.Options, []string{"2.0"}) || !slices.Equal(p.Env, []string{"PROBE_HOME"}) {
		t.Fatalf("probe in the inventory: %+v (inventory %+v)", p, r.Tools)
	}
	if p := inventoryOf(r, "probe-two"); p == nil || !slices.Equal(p.Options, []string{"3.1"}) {
		t.Fatalf("probe-two in the inventory: %+v", r.Tools)
	}
	if len(r.Rooms) != 1 || r.Rooms[0].Name != "a" || r.Rooms[0].State != protocol.RoomFree {
		t.Fatalf("machine rooms = %+v, want one free room a", r.Rooms)
	}

	for _, f := range []string{"hostel-linux-" + dockerArch, "mise-linux-" + dockerArch} {
		want, err := hashes.File(filepath.Join(helperDir, f))
		if err != nil {
			t.Fatal(err)
		}
		remote := "/home/hostel/hostel/bin/" + strings.Split(f, "-")[0]
		if got := strings.Fields(c.sh("sha256sum " + remote))[0]; got != want {
			t.Fatalf("%s on the machine: sha256 %s, want %s", remote, got, want)
		}
	}
	if got := c.sh("ls /home/hostel/hostel/tools/probe"); got != "check.linux.sh\ninstall.linux.sh\ntool.toml" {
		t.Fatalf("tool files on the machine:\n%s\nwant tool.toml and the linux scripts only", got)
	}
	if got := c.sh("stat -c %a /home/hostel/hostel/machine-env.json"); got != "600" {
		t.Fatalf("machine-env.json mode %s, want 600", got)
	}
	wantLog := "install probe var=machine-value mise=/home/hostel/hostel/cache/mise home=/home/hostel/hostel/cache/probe pwd=/home/hostel/hostel"
	if got := c.sh("cat /home/hostel/hostel/cache/install.log"); got != wantLog {
		t.Fatalf("install log:\n%s\nwant:\n%s", got, wantLog)
	}
	if r := e.hostel("status"); r.code != 0 || !regexp.MustCompile(`home/box\s+probe 2\.0, probe-two 3\.1\s+free`).MatchString(r.stdout) {
		t.Fatalf("hostel status: %s", r)
	}

	// A restart probes again but does not run install again.
	s.stop()
	s = e.startServer(s.token)
	s.waitMachine("home/box", protocol.MachineRunning, machineReady)
	if got := c.sh("wc -l < /home/hostel/hostel/cache/install.log"); got != "1" {
		t.Fatalf("install ran %s times after a restart, want 1", got)
	}

	// Changed tool scripts (pushed, without a restart): the free machine
	// installs again.
	e.writeTool("probe", map[string]string{"install.linux.sh": installScript + "# changed\n"})
	e.pushTool("probe")
	e.waitFor("install runs again", machineReady, func() bool {
		return c.sh("wc -l < /home/hostel/hostel/cache/install.log") == "2"
	})
	s.waitMachine("home/box", protocol.MachineRunning, machineReady)
	// A tool that the machine no longer lists leaves the machine and the
	// inventory.
	if r := e.applyDoc(strings.Replace(s.configDoc().Document, `"probe", "probe-two"`, `"probe"`, 1)); r.code != 0 {
		t.Fatalf("apply: %s", r)
	}
	e.waitFor("probe-two leaves the machine", machineReady, func() bool {
		return c.sh("test -e /home/hostel/hostel/tools/probe-two && echo exists || echo removed") == "removed"
	})
	r = s.waitMachine("home/box", protocol.MachineRunning, machineReady)
	if inventoryOf(r, "probe-two") != nil || inventoryOf(r, "probe") == nil {
		t.Fatalf("inventory after the removal of probe-two: %+v", r.Tools)
	}
}

// TestProvisionBackgroundProcess: an install script that leaves a background
// process with its output open (for example a simulator) still succeeds.
func TestProvisionBackgroundProcess(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.init()
	c := e.startStaticMachine("box")
	e.writeTool("probe", map[string]string{"tool.toml": "description = \"Probe\"\n", "install.linux.sh": "#!/bin/sh\nsleep 600 &\necho started\n"})
	e.writeConfig(staticMachineConfig(c, `"probe"`))
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	s.waitMachine("home/box", protocol.MachineRunning, machineReady)
}

// TestStaticMachineOffline: a static host that is offline at start is broken with
// the reason "unreachable", and becomes running by itself when the host is back
// (scenario 25, offline at start).
func TestStaticMachineOffline(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.init()
	c := e.createStaticMachine("box")
	e.writeConfig(staticMachineConfig(c, ""))
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	r := s.waitMachine("home/box", protocol.MachineBroken, time.Minute)
	if r.Reason != "unreachable" {
		t.Fatalf("machine = %+v, want reason unreachable", r)
	}
	c.start()
	s.waitMachine("home/box", protocol.MachineRunning, machineReady)
}

// TestProvisionFailure: a failed install script makes the machine broken.
func TestProvisionFailure(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.init()
	c := e.startStaticMachine("box")
	e.writeTool("probe", map[string]string{"tool.toml": "description = \"Probe\"\n", "install.linux.sh": "#!/bin/sh\necho no simulator runtime >&2\nexit 3\n"})
	e.writeConfig(staticMachineConfig(c, `"probe"`))
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	r := s.waitMachine("home/box", protocol.MachineBroken, machineReady)
	if r.Reason != "tool probe: install script exited with code 3" {
		t.Fatalf("machine = %+v, want the install failure as the reason", r)
	}
	if p := inventoryOf(r, "probe"); p == nil || p.State != "failed" {
		t.Fatalf("probe in the inventory: %+v", r.Tools)
	}
}

// TestTokens: admins create and revoke machine tokens; members cannot, and user
// tokens come only from invite codes (M8).
func TestTokens(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.initServer(serverHeader() + staticTemplate)
	s := e.startServer("")
	admin := e.admin

	r := e.hostel("admin", "token", "create", "--name", "box", "--role", "machine", "--machine", "home/box")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "hostel_") {
		t.Fatalf("machine token create: %s", r)
	}
	machine := strings.TrimSpace(r.stdout)
	if code := s.get("/v1/info", machine, nil); code != 200 {
		t.Fatalf("machine token: GET /v1/info HTTP %d", code)
	}
	if code := s.get("/v1/status", machine, nil); code != 401 {
		t.Fatalf("machine token: GET /v1/status HTTP %d, want 401", code)
	}
	if r := e.hostel("admin", "token", "create", "--name", "agent", "--role", "guest"); r.code != 1 || !strings.Contains(r.stderr, "hostel config users invite") {
		t.Fatalf("user token create: %s, want a refusal that names invites", r)
	}
	if r := e.hostel("admin", "token", "create", "--name", "r", "--role", "machine", "--machine", "home/nothing"); r.code != 1 || !strings.Contains(r.stderr, "static machine") {
		t.Fatalf("machine token for an unknown machine: %s", r)
	}
	e.useGuestToken()
	if r := e.hostel("status"); r.code != 0 {
		t.Fatalf("status with a member token: %s", r)
	}
	if r := e.hostel("admin", "token", "create", "--name", "x", "--role", "machine", "--machine", "home/box"); r.code != 1 || !strings.Contains(r.stderr, "only an admin") {
		t.Fatalf("token create with a member token: %s", r)
	}
	e.vars["HOSTEL_TOKEN"] = admin
	if r := e.hostel("admin", "token", "revoke", "2"); r.code != 0 {
		t.Fatalf("token revoke: %s", r)
	}
	if code := s.get("/v1/info", machine, nil); code != 401 {
		t.Fatalf("revoked token: GET /v1/info HTTP %d, want 401", code)
	}
	if r := e.hostel("admin", "pubkey"); r.code != 0 || !strings.HasPrefix(r.stdout, "ssh-ed25519 ") {
		t.Fatalf("admin pubkey: %s", r)
	}
}

// TestOldHelperReplaced (M10g): a machine with the binary of a server before
// M10g has no "helper" command. The server replaces it, instead of breaking
// the machine.
func TestOldHelperReplaced(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.s.stop()
	script := `printf '%s\n' '#!/bin/sh' 'echo hostel: unknown command \"helper\" >&2' 'exit 2' > /home/hostel/hostel/bin/hostel`
	if out, err := docker("exec", f.c.name, "sh", "-c", script); err != nil {
		t.Fatalf("install the old binary: %v: %s", err, out)
	}
	db, err := sql.Open("sqlite", filepath.Join(f.e.dataDir(), "hostel.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`UPDATE machines SET helper_sha256 = 'old'`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	f.s = f.s.restart()
	f.s.waitMachine("home/box", protocol.MachineRunning, machineReady)
	f.e.mustHostel("acquire", "fake-screen", "--task", "after the update")
	f.e.mustHostel("release")
}

// TestOldGuestState (M10g): a guest updates while its lease.json has the keys
// from before M10g, and migration 013 ended that lease. Acquire reports the
// end instead of success with an empty room.
func TestOldGuestState(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	lease := f.acquire()
	path := filepath.Join(f.e.cwd, ".git", "hostel", "lease", "lease.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	st["machine"] = st["template"]
	delete(st, "template")
	access := st["access"].(map[string]any)
	for now, old := range map[string]string{"room": "slot", "machine_os": "room_os", "machine_arch": "room_arch"} {
		access[old] = access[now]
		delete(access, now)
	}
	if data, err = json.Marshal(st); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	f.s.stop()
	db, err := sql.Open("sqlite", filepath.Join(f.e.dataDir(), "hostel.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`UPDATE leases SET state = 'cleaning', end_state = 'failed', end_reason = 'the server was updated; acquire again', revision = revision + 1 WHERE id = ?`, lease)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	f.s = f.s.restart()
	f.e.waitFor("the end of the migrated lease", 2*time.Minute, func() bool { return f.lease(lease).State == "failed" })
	r := f.e.hostel("acquire", "fake-screen", "--task", "after the update")
	if r.code == 0 || !strings.Contains(r.stderr, "the server was updated; acquire again") {
		t.Fatalf("acquire with the old state: %s", r)
	}
	f.waitRoomFree()
	f.e.mustHostel("acquire", "fake-screen", "--task", "after the update")
	f.e.mustHostel("release")
}

// TestOldHelperCleanup (M10g): a lease that migration 013 moved to cleaning
// runs on a machine with the old binary. The server replaces the binary and
// cleans the room, instead of marking the machine broken.
func TestOldHelperCleanup(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	lease := f.acquire()
	f.e.mustHostel("run", "--detach", "--", "sleep 4242")
	f.s.stop()
	// The binary and the room directory of a server before M10g.
	script := `mkdir -p /home/hostel/hostel/slots && chown hostel /home/hostel/hostel/slots && mv /home/hostel/hostel/rooms/a /home/hostel/hostel/slots/a && printf '%s\n' '#!/bin/sh' 'echo hostel: unknown command \"helper\" >&2' 'exit 2' > /home/hostel/hostel/bin/.old && chmod 755 /home/hostel/hostel/bin/.old && mv /home/hostel/hostel/bin/.old /home/hostel/hostel/bin/hostel`
	if out, err := docker("exec", f.c.name, "sh", "-c", script); err != nil {
		t.Fatalf("install the old state: %v: %s", err, out)
	}
	db, err := sql.Open("sqlite", filepath.Join(f.e.dataDir(), "hostel.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`UPDATE leases SET state = 'cleaning', end_state = 'failed', end_reason = 'the server was updated; acquire again', revision = revision + 1 WHERE id = ?`, lease)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	f.s = f.s.restart()
	f.e.waitFor("the end of the migrated lease", 2*time.Minute, func() bool { return f.lease(lease).State == "failed" })
	f.waitRoomFree()
	if r := f.s.machine("home/box"); r.State == protocol.MachineBroken {
		t.Fatalf("the machine is broken: %s", r.Reason)
	}
	// The cleanup used the state of the old directory: the job is gone, and so
	// are both directories.
	out, err := docker("exec", f.c.name, "sh", "-c", `pgrep -f "sleep [4]242" | wc -l; ls /home/hostel/hostel/slots/a /home/hostel/hostel/rooms/a 2>/dev/null | wc -l`)
	if err != nil || strings.Join(strings.Fields(out), " ") != "0 0" {
		t.Fatalf("after the cleanup (jobs, directory entries): %q, %v", out, err)
	}
}

// TestOldHelperCleanupRoomUser (M10g): a room with its own OS user has its own
// helper in its own home, so the server replaces that one.
func TestOldHelperCleanupRoomUser(t *testing.T) {
	t.Parallel()
	f := newFixture(t, &fixtureOpts{roomUsers: []string{"hostel-a"}, templateTools: []string{"fake-screen", "fake-sdk"},
		templateKeys: "rooms = [{ name = \"a\", user = \"hostel-a\" }]\n"})
	lease := f.acquire()
	f.s.stop()
	script := `printf '%s\n' '#!/bin/sh' 'echo hostel: unknown command \"helper\" >&2' 'exit 2' > /home/hostel-a/hostel/bin/.old && chmod 755 /home/hostel-a/hostel/bin/.old && mv /home/hostel-a/hostel/bin/.old /home/hostel-a/hostel/bin/hostel`
	if out, err := docker("exec", f.c.name, "sh", "-c", script); err != nil {
		t.Fatalf("install the old binary: %v: %s", err, out)
	}
	db, err := sql.Open("sqlite", filepath.Join(f.e.dataDir(), "hostel.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`UPDATE leases SET state = 'cleaning', end_state = 'failed', end_reason = 'the server was updated; acquire again', revision = revision + 1 WHERE id = ?`, lease)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	f.s = f.s.restart()
	f.e.waitFor("the end of the migrated lease", 2*time.Minute, func() bool { return f.lease(lease).State == "failed" })
	f.waitRoomFree()
	if r := f.s.machine("home/box"); r.State == protocol.MachineBroken {
		t.Fatalf("the machine is broken: %s", r.Reason)
	}
}
