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

// machineConnect runs "hostel helper connect" as the machine's service would, with
// the machine's SSH server at sshAddr. stop ends it.
type machineConnect struct {
	cmd *exec.Cmd
	mu  sync.Mutex
	out bytes.Buffer
}

func (r *machineConnect) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.out.Write(p)
}

func (r *machineConnect) output() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.out.String()
}

func (e *env) startMachineConnect(server, tokenFile, sshAddr string) *machineConnect {
	e.t.Helper()
	r := &machineConnect{}
	r.cmd = exec.Command(hostelBin, "machine", "connect", "--server", server, "--token-file", tokenFile, "--ssh", sshAddr)
	r.cmd.Env = e.environ()
	r.cmd.Stdout, r.cmd.Stderr = r, r
	if err := r.cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(r.stop)
	return r
}

func (r *machineConnect) stop() {
	if r.cmd.ProcessState == nil {
		r.cmd.Process.Kill()
		r.cmd.Wait()
	}
}

// TestMachineConnect: a static machine behind NAT (connect = true). The server
// reaches the machine's SSH server only through the machine's outbound connection to
// the gateway, here over the WebSocket gateway on the server's own HTTPS (the
// pinned key). A user token and a token of
// another machine cannot connect, and a revoked token loses its connection. The machine is unreachable without the
// connection; when the machine connects, the server probes it at once (here
// reconcile runs only at the start).
func TestMachineConnect(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.init()
	c := e.startStaticMachine("box")
	e.useTools("fake-sdk", "fake-screen")
	header := strings.Replace(serverHeader(), `reconcile = "2s"`, `reconcile = "1h"`, 1)
	e.writeConfig(header + `

[providers.home]
kind = "static"

[providers.home.machines.box]
address = "hostel@localhost"
os = "linux"
tools = ["fake-screen"]
connect = true

[providers.home.machines.other]
address = "hostel@localhost"
os = "linux"
tools = ["fake-screen"]
connect = true
`)
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	server := s.url
	sshAddr := fmt.Sprintf("127.0.0.1:%d", c.port)
	s.waitMachine("home/box", protocol.MachineBroken, machineReady)

	tokenFile := func(args ...string) string {
		r := e.mustHostel(append([]string{"admin", "token", "create"}, args...)...)
		f := filepath.Join(e.home, fmt.Sprintf("token-%d", time.Now().UnixNano()))
		if err := os.WriteFile(f, []byte(r.stdout), 0o600); err != nil {
			t.Fatal(err)
		}
		return f
	}
	// A user token is not a machine token.
	userFile := filepath.Join(e.home, "user-token")
	if err := os.WriteFile(userFile, []byte(e.admin), 0o600); err != nil {
		t.Fatal(err)
	}
	guest := e.startMachineConnect(server, userFile, sshAddr)
	e.waitFor("the guest token refused", time.Minute, func() bool {
		return strings.Contains(guest.output(), "the gateway refused the connection")
	})
	guest.stop()
	// A machine token of another machine connects as that machine only.
	other := e.startMachineConnect(server, tokenFile("--name", "other", "--role", "machine", "--machine", "home/other"), sshAddr)
	s.waitMachine("home/other", protocol.MachineRunning, machineReady)
	if r := s.machine("home/box"); r.State != protocol.MachineBroken {
		t.Fatalf("home/box with only the connection of home/other: %s", r.State)
	}
	other.stop()

	boxToken := tokenFile("--name", "box", "--role", "machine", "--machine", "home/box")
	machine := e.startMachineConnect(server, boxToken, sshAddr)
	s.waitMachine("home/box", protocol.MachineRunning, machineReady)
	if !strings.Contains(machine.output(), "connected to the gateway at wss://127.0.0.1:") {
		t.Fatalf("machine connect output: %s", machine.output())
	}
	e.useGuestToken()
	e.newRepo(defaultRepoFiles())
	f := &fixture{e: e, s: s, c: c}
	id := f.acquire()
	if r := e.mustHostel("run", "--", "echo through-machine-connect"); r.stdout != "through-machine-connect\n" {
		t.Fatalf("run: %q", r.stdout)
	}

	// The connection ends: guest commands fail; they work again in the same
	// lease when the connection comes back.
	machine.stop()
	r := e.hostel("run", "--", "true")
	if r.code == 0 || !strings.Contains(r.stderr, "connection to the machine lost") {
		t.Fatalf("run without the connection: %s", r)
	}
	machine = e.startMachineConnect(server, boxToken, sshAddr)
	e.waitFor("run works again", time.Minute, func() bool {
		r := e.hostel("run", "--", "echo again")
		return r.code == 0 && r.stdout == "again\n"
	})
	if l := f.lease(id); l.State != protocol.LeaseReady {
		t.Fatalf("lease after the connection came back: %s", l.State)
	}

	// Revoking the machine token (ID 3: after the admin's token and the machine
	// token of home/other) closes its live connection at once.
	guestToken := e.vars["HOSTEL_TOKEN"]
	e.vars["HOSTEL_TOKEN"] = e.admin
	e.mustHostel("admin", "token", "revoke", "3")
	e.vars["HOSTEL_TOKEN"] = guestToken
	e.waitFor("the revoked token's connection closed", time.Minute, func() bool {
		return strings.Contains(machine.output(), "the server refused the machine token")
	})
	if r := e.hostel("run", "--", "true"); r.code == 0 {
		t.Fatalf("run after the machine token was revoked: %s", r)
	}
}
