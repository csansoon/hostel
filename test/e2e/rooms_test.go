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

	"github.com/csansoon/hostel/internal/hashes"
	"github.com/csansoon/hostel/internal/protocol"
)

const roomReady = 3 * time.Minute

// provisionScript records each call, with values from its environment, in the
// room cache.
const provisionScript = `#!/bin/sh
echo "provision $HOSTEL_PLATFORM devices=$HOSTEL_PLATFORM_DEVICES var=$MACHINE_VAR mise=$MISE_DATA_DIR pwd=$(pwd)" >> "$HOSTEL_CACHE_DIR/provision.log"
echo "provisioned $HOSTEL_PLATFORM"
`

func staticRoomConfig(c *container, platforms string) string {
	return serverHeader() + fmt.Sprintf(`
[providers.home]
kind = "static"

[providers.home.machines.box]
address = %q
os = "linux"
platforms = [%s]
env = { MACHINE_VAR = "machine-value" }
`, c.address(), platforms)
}

func (e *env) writePlatform(name string, files map[string]string) {
	for f, body := range files {
		e.writeFile(filepath.Join(e.configDir(), "platforms", name, f), body)
	}
}

// TestStaticRoom: a static room is probed, gets the helper, mise, and the
// platform scripts, and is ready for its platforms at server start (scenario 22,
// static part). A restart does not provision again; changed scripts do.
func TestStaticRoom(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.init()
	c := e.startStaticRoom("box")
	e.writePlatform("linux", map[string]string{"health.linux.sh": "#!/bin/sh\n"})
	e.writePlatform("fakedevice", map[string]string{
		"platform.toml":      "devices = [\"phone\", \"tablet\"]\n",
		"prepare.linux.sh":   "#!/bin/sh\n",
		"provision.linux.sh": provisionScript,
		"provision.macos.sh": "#!/bin/sh\nexit 1\n",
		"README.md":          "not uploaded\n",
	})
	e.writeConfig(staticRoomConfig(c, `"fakedevice"`))
	s := e.startServer(e.vars["HOSTEL_TOKEN"])

	r := s.waitRoom("home/box", protocol.RoomRunning, roomReady)
	if r.OS != "linux" || r.Arch != dockerArch {
		t.Fatalf("room = %+v, want linux/%s", r, dockerArch)
	}
	if !slices.Contains(r.Platforms, "fakedevice") || !slices.Contains(r.Platforms, "linux") {
		t.Fatalf("room platforms = %v, want fakedevice and linux", r.Platforms)
	}
	if len(r.Slots) != 1 || r.Slots[0].Name != "a" || r.Slots[0].State != protocol.SlotFree {
		t.Fatalf("room slots = %+v, want one free slot a", r.Slots)
	}

	for _, f := range []string{"hostel-linux-" + dockerArch, "mise-linux-" + dockerArch} {
		want, err := hashes.File(filepath.Join(helperDir, f))
		if err != nil {
			t.Fatal(err)
		}
		remote := "/home/hostel/hostel/bin/" + strings.Split(f, "-")[0]
		if got := strings.Fields(c.sh("sha256sum " + remote))[0]; got != want {
			t.Fatalf("%s on the room: sha256 %s, want %s", remote, got, want)
		}
	}
	if got := c.sh("ls /home/hostel/hostel/platforms/fakedevice"); got != "platform.toml\nprepare.linux.sh\nprovision.linux.sh" {
		t.Fatalf("platform files on the room:\n%s\nwant platform.toml and the linux scripts only", got)
	}
	if got := c.sh("stat -c %a /home/hostel/hostel/room-env.json"); got != "600" {
		t.Fatalf("room-env.json mode %s, want 600", got)
	}
	wantLog := "provision fakedevice devices=phone,tablet var=machine-value mise=/home/hostel/hostel/cache/mise pwd=/home/hostel/hostel"
	if got := c.sh("cat /home/hostel/hostel/cache/provision.log"); got != wantLog {
		t.Fatalf("provision log:\n%s\nwant:\n%s", got, wantLog)
	}
	if r := e.hostel("status"); r.code != 0 || !strings.Contains(r.stdout, "home/box") || !strings.Contains(r.stdout, "running") {
		t.Fatalf("hostel status: %s", r)
	}

	// A restart probes again but does not run provision again.
	s.stop()
	s = e.startServer(s.token)
	s.waitRoom("home/box", protocol.RoomRunning, roomReady)
	if got := c.sh("wc -l < /home/hostel/hostel/cache/provision.log"); got != "1" {
		t.Fatalf("provision ran %s times after a restart, want 1", got)
	}

	if got := c.sh("ls /home/hostel/hostel/platforms/linux"); got != "health.linux.sh" {
		t.Fatalf("linux platform files on the room: %q", got)
	}

	// Changed platform scripts: provision runs again at the next start. Removed
	// OS platform scripts are removed on the room.
	e.writePlatform("fakedevice", map[string]string{"provision.linux.sh": provisionScript + "# changed\n"})
	if err := os.Remove(filepath.Join(e.configDir(), "platforms", "linux", "health.linux.sh")); err != nil {
		t.Fatal(err)
	}
	s.stop()
	s = e.startServer(s.token)
	s.waitRoom("home/box", protocol.RoomRunning, roomReady)
	if got := c.sh("wc -l < /home/hostel/hostel/cache/provision.log"); got != "2" {
		t.Fatalf("provision ran %s times after a script change, want 2", got)
	}
	if got := c.sh("test -e /home/hostel/hostel/platforms/linux && echo exists || echo removed"); got != "removed" {
		t.Fatalf("old linux platform scripts on the room: %s", got)
	}
}

// TestProvisionBackgroundProcess: a provision script that leaves a background
// process with its output open (for example a simulator) still succeeds.
func TestProvisionBackgroundProcess(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.init()
	c := e.startStaticRoom("box")
	e.writePlatform("fakedevice", map[string]string{
		"platform.toml":      "devices = [\"phone\"]\n",
		"prepare.linux.sh":   "#!/bin/sh\n",
		"provision.linux.sh": "#!/bin/sh\nsleep 600 &\necho started\n",
	})
	e.writeConfig(staticRoomConfig(c, `"fakedevice"`))
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	s.waitRoom("home/box", protocol.RoomRunning, roomReady)
}

// TestStaticRoomOffline: a static host that is offline at start is broken with
// the reason "unreachable", and becomes running by itself when the host is back
// (scenario 25, offline at start).
func TestStaticRoomOffline(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.init()
	c := e.createStaticRoom("box")
	e.writeConfig(staticRoomConfig(c, ""))
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	r := s.waitRoom("home/box", protocol.RoomBroken, time.Minute)
	if r.Reason != "unreachable" {
		t.Fatalf("room = %+v, want reason unreachable", r)
	}
	c.start()
	s.waitRoom("home/box", protocol.RoomRunning, roomReady)
}

// TestProvisionFailure: a failed provision script makes the room broken.
func TestProvisionFailure(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.init()
	c := e.startStaticRoom("box")
	e.writePlatform("fakedevice", map[string]string{
		"platform.toml":      "devices = [\"phone\"]\n",
		"prepare.linux.sh":   "#!/bin/sh\n",
		"provision.linux.sh": "#!/bin/sh\necho no simulator runtime >&2\nexit 3\n",
	})
	e.writeConfig(staticRoomConfig(c, `"fakedevice"`))
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	r := s.waitRoom("home/box", protocol.RoomBroken, roomReady)
	if r.Reason != "platform fakedevice: provision script exited with code 3" {
		t.Fatalf("room = %+v, want the provision failure as the reason", r)
	}
}

// TestTokens: admins create and revoke tokens; guest tokens cannot.
func TestTokens(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	admin := e.initServer(serverHeader() + staticMachine)
	s := e.startServer(admin)

	r := e.hostel("admin", "token", "create", "--name", "agent-1")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "hostel_") {
		t.Fatalf("token create: %s", r)
	}
	guest := strings.TrimSpace(r.stdout)
	if code := s.get("/v1/info", guest, nil); code != 200 {
		t.Fatalf("guest token: GET /v1/info HTTP %d", code)
	}
	e.vars["HOSTEL_TOKEN"] = guest
	if r := e.hostel("status"); r.code != 0 {
		t.Fatalf("status with a guest token: %s", r)
	}
	if r := e.hostel("admin", "token", "create", "--name", "x"); r.code != 1 || !strings.Contains(r.stderr, "needs an admin token") {
		t.Fatalf("token create with a guest token: %s", r)
	}
	e.vars["HOSTEL_TOKEN"] = admin
	if r := e.hostel("admin", "token", "create", "--name", "r", "--role", "room", "--machine", "home/nothing"); r.code != 1 || !strings.Contains(r.stderr, "static machine") {
		t.Fatalf("room token for an unknown machine: %s", r)
	}
	if r := e.hostel("admin", "token", "revoke", "2"); r.code != 0 {
		t.Fatalf("token revoke: %s", r)
	}
	if code := s.get("/v1/info", guest, nil); code != 401 {
		t.Fatalf("revoked token: GET /v1/info HTTP %d, want 401", code)
	}
	if r := e.hostel("admin", "pubkey"); r.code != 0 || !strings.HasPrefix(r.stdout, "ssh-ed25519 ") {
		t.Fatalf("admin pubkey: %s", r)
	}
}
