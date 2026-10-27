//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
)

// roomConnect runs "hostel room connect" as the room's service would, with
// the room's SSH server at sshAddr. stop ends it.
type roomConnect struct {
	cmd *exec.Cmd
	mu  sync.Mutex
	out bytes.Buffer
}

func (r *roomConnect) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.out.Write(p)
}

func (r *roomConnect) output() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.out.String()
}

func (e *env) startRoomConnect(server, tokenFile, sshAddr string) *roomConnect {
	e.t.Helper()
	r := &roomConnect{}
	r.cmd = exec.Command(hostelBin, "room", "connect", "--server", server, "--token-file", tokenFile, "--ssh", sshAddr)
	r.cmd.Env = e.environ()
	r.cmd.Stdout, r.cmd.Stderr = r, r
	if err := r.cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(r.stop)
	return r
}

func (r *roomConnect) stop() {
	if r.cmd.ProcessState == nil {
		r.cmd.Process.Kill()
		r.cmd.Wait()
	}
}

// TestRoomConnect: a static room behind NAT (connect = true). The server
// reaches the room's SSH server only through the room's outbound connection to
// the gateway, here over the WebSocket gateway. A guest token and a token of
// another machine cannot connect. The room is unreachable without the
// connection and recovers when it comes back.
func TestRoomConnect(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.init()
	c := e.startStaticRoom("box")
	e.usePlatform("fakedevice")
	e.writeConfig(serverHeader() + `gateway_public = "ws://127.0.0.1:0/v1/gateway"

[providers.home]
kind = "static"

[providers.home.machines.box]
address = "hostel@localhost"
os = "linux"
platforms = ["fakedevice"]
connect = true

[providers.home.machines.other]
address = "hostel@localhost"
os = "linux"
platforms = ["fakedevice"]
connect = true
`)
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	server := "http://" + s.addr
	sshAddr := fmt.Sprintf("127.0.0.1:%d", c.port)
	s.waitRoom("home/box", protocol.RoomBroken, roomReady)

	tokenFile := func(args ...string) string {
		r := e.mustHostel(append([]string{"admin", "token", "create"}, args...)...)
		f := filepath.Join(e.home, fmt.Sprintf("token-%d", time.Now().UnixNano()))
		if err := os.WriteFile(f, []byte(r.stdout), 0o600); err != nil {
			t.Fatal(err)
		}
		return f
	}
	// A guest token is not a room token.
	guest := e.startRoomConnect(server, tokenFile("--name", "not-a-room"), sshAddr)
	e.waitFor("the guest token refused", time.Minute, func() bool {
		return strings.Contains(guest.output(), "the gateway refused the connection")
	})
	guest.stop()
	// A room token of another machine connects as that machine only.
	other := e.startRoomConnect(server, tokenFile("--name", "other", "--role", "room", "--machine", "home/other"), sshAddr)
	s.waitRoom("home/other", protocol.RoomRunning, roomReady)
	if r := s.room("home/box"); r.State != protocol.RoomBroken {
		t.Fatalf("home/box with only the connection of home/other: %s", r.State)
	}
	other.stop()

	boxToken := tokenFile("--name", "box", "--role", "room", "--machine", "home/box")
	room := e.startRoomConnect(server, boxToken, sshAddr)
	s.waitRoom("home/box", protocol.RoomRunning, roomReady)
	if !strings.Contains(room.output(), "connected to the gateway at ws://127.0.0.1:") {
		t.Fatalf("room connect output: %s", room.output())
	}
	e.useGuestToken()
	e.newRepo(defaultRepoFiles())
	f := &fixture{e: e, s: s, c: c}
	id := f.acquire()
	if r := e.mustHostel("run", "--", "echo through-room-connect"); r.stdout != "through-room-connect\n" {
		t.Fatalf("run: %q", r.stdout)
	}

	// The connection ends: guest commands fail; they work again in the same
	// lease when the connection comes back.
	room.stop()
	r := e.hostel("run", "--", "true")
	if r.code == 0 || !strings.Contains(r.stderr, "connection to the room lost") {
		t.Fatalf("run without the connection: %s", r)
	}
	e.startRoomConnect(server, boxToken, sshAddr)
	e.waitFor("run works again", time.Minute, func() bool {
		r := e.hostel("run", "--", "echo again")
		return r.code == 0 && r.stdout == "again\n"
	})
	if l := f.lease(id); l.State != protocol.LeaseReady {
		t.Fatalf("lease after the connection came back: %s", l.State)
	}
}
