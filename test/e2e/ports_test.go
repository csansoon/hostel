//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// localHTTP starts an HTTP server on the guest at addr ("127.0.0.1:0" or
// "[::1]:<port>") that answers "hello from <name> <path>".
func localHTTP(t *testing.T, addr, name string) int {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello from %s %s", name, r.URL.Path)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

// roomGet sends an HTTP request from the room to a room port (a tunnel).
func (f *fixture) roomGet(port, path string) result {
	return f.e.hostel("run", "--", fmt.Sprintf(`printf 'GET %s HTTP/1.0\r\n\r\n' | nc -N -w 5 127.0.0.1 %s`, path, port))
}

func (f *fixture) leaseKey() ssh.Signer {
	f.e.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.e.cwd, ".git", "hostel", "lease", "id_ed25519"))
	if err != nil {
		f.e.t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		f.e.t.Fatal(err)
	}
	return signer
}

// sshDial connects with a key to addr as user; it returns the error.
func sshDial(addr, user string, key ssh.Signer) (*ssh.Client, error) {
	return ssh.Dial("tcp", addr, &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(key)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second})
}

const tunnelRepoConfig = `include = [".env"]

[tunnels.api]
local_port_env = "PORT"

[tool.fake-screen]
env = { API_URL = "http://localhost:{tunnels.api.port}" }
`

// TestTunnels is scenario 14: tunnels to a local server that starts after the
// tunnel, port changes without a restart, runtime tunnel changes, an IPv6-only
// local server, and a reconnect after a drop.
func TestTunnels(t *testing.T) {
	t.Parallel()
	free := freePort(t)
	f := newFixture(t, &fixtureOpts{files: map[string]string{
		"hostel.toml": tunnelRepoConfig,
		".gitignore":  ".env\n",
		".env":        fmt.Sprintf("PORT=%d\n", free),
	}})
	f.acquire()
	if r := f.e.mustHostel("run", "--", "echo $API_URL $HOSTEL_TUNNEL_API_PORT"); r.stdout != "http://localhost:7500 7500\n" {
		t.Fatalf("tunnel environment: %q", r.stdout)
	}
	// The room side alone does not make a tunnel work: the state and the
	// facts say what is missing, and the lease goes on.
	nothing := fmt.Sprintf("nothing listens on local port %d", free)
	if r := f.e.mustHostel("tunnel", "list"); !strings.Contains(r.stdout, nothing) || strings.Contains(r.stdout, "confirmed") {
		t.Fatalf("tunnel list without a local server:\n%s", r.stdout)
	}
	if r := f.e.mustHostel("info"); !strings.Contains(r.stdout, fmt.Sprintf("api: room port 7500 → local $PORT (%d): %s", free, nothing)) {
		t.Fatalf("facts without a local server:\n%s", r.stdout)
	}
	if r := f.e.mustHostel("tunnel", "add", "ghost", "--local-port-env", "HOSTEL_NOT_SET"); !strings.Contains(r.stdout, "no local port: HOSTEL_NOT_SET is not set") {
		t.Fatalf("tunnel add with an unset variable:\n%s", r.stdout)
	}
	f.e.mustHostel("tunnel", "remove", "ghost")
	// The local server starts after the tunnel, on the port from .env.
	localHTTP(t, fmt.Sprintf("127.0.0.1:%d", free), "first")
	if r := f.e.mustHostel("tunnel", "list"); !strings.Contains(r.stdout, "confirmed") {
		t.Fatalf("tunnel list with a local server:\n%s", r.stdout)
	}
	if r := f.roomGet("$HOSTEL_TUNNEL_API_PORT", "/a"); !strings.Contains(r.stdout, "hello from first /a") {
		t.Fatalf("tunnel: %s", r)
	}
	// The guest moves its server to another port, on ::1 only: .env changes,
	// and the next connection uses it without a restart.
	v6 := freePort(t)
	localHTTP(t, fmt.Sprintf("[::1]:%d", v6), "second")
	f.write(".env", fmt.Sprintf("PORT=%d\n", v6))
	if r := f.roomGet("7500", "/b"); !strings.Contains(r.stdout, "hello from second /b") {
		t.Fatalf("tunnel after the .env change: %s", r)
	}
	// A room client on ::1 reaches the tunnel too.
	if r := f.e.hostel("run", "--", "printf 'GET /v6 HTTP/1.0\\r\\n\\r\\n' | nc -N -w 5 ::1 7500"); !strings.Contains(r.stdout, "hello from second /v6") {
		t.Fatalf("tunnel from ::1 on the room: %s", r)
	}
	f.e.mustHostel("tunnel", "set", "api", "--local-port", fmt.Sprint(free))
	if r := f.roomGet("7500", "/c"); !strings.Contains(r.stdout, "hello from first /c") {
		t.Fatalf("tunnel after tunnel set --local-port: %s", r)
	}

	// Runtime tunnels with a fixed room port.
	f.e.mustHostel("tunnel", "add", "web", "--local-port", fmt.Sprint(free), "--room-port", "7600")
	if r := f.e.mustHostel("run", "--", "echo $HOSTEL_TUNNEL_WEB_PORT"); r.stdout != "7600\n" {
		t.Fatalf("new tunnel variable: %q", r.stdout)
	}
	if r := f.roomGet("7600", "/web"); !strings.Contains(r.stdout, "hello from first /web") {
		t.Fatalf("new tunnel: %s", r)
	}
	f.e.mustHostel("tunnel", "set", "web", "--room-port", "7601")
	if r := f.roomGet("$HOSTEL_TUNNEL_WEB_PORT", "/moved"); !strings.Contains(r.stdout, "hello from first /moved") {
		t.Fatalf("moved tunnel: %s", r)
	}
	if r := f.e.mustHostel("run", "--", "nc -z 127.0.0.1 7600 && echo open || echo closed"); r.stdout != "closed\n" {
		t.Fatalf("old room port after tunnel set --room-port: %q", r.stdout)
	}
	f.e.mustHostel("tunnel", "remove", "web")
	if r := f.e.mustHostel("run", "--", "echo \"[$HOSTEL_TUNNEL_WEB_PORT]\"; nc -z 127.0.0.1 7601 && echo open || echo closed"); r.stdout != "[]\nclosed\n" {
		t.Fatalf("after tunnel remove: %q", r.stdout)
	}
	// A fixed port that a room service uses fails with a clear message.
	f.e.mustHostel("run", "--detach", "--", "nc -lk 127.0.0.1 7650")
	f.e.waitFor("room service listens", time.Minute, func() bool {
		return f.e.hostel("run", "--", "nc -z 127.0.0.1 7650").code == 0
	})
	r := f.e.hostel("tunnel", "add", "busy", "--local-port", fmt.Sprint(free), "--room-port", "7650")
	if r.code != 1 || !strings.Contains(r.stderr, "refused to listen on port 7650") {
		t.Fatalf("tunnel on a used room port: %s", r)
	}
	if r := f.e.mustHostel("tunnel", "list"); strings.Contains(r.stdout, "busy") || !strings.Contains(r.stdout, "api     7500") {
		t.Fatalf("tunnel list after the refused tunnel:\n%s", r.stdout)
	}

	// The tunnel reconnects after a drop (server restart).
	f.s = f.s.restart()
	f.e.waitFor("tunnel after the restart", time.Minute, func() bool {
		return strings.Contains(f.roomGet("7500", "/again").stdout, "hello from first /again")
	})
}

// TestTunnelInSetup is scenario 14 (setup): the port process starts before the
// setup job, so setup commands reach a guest service.
func TestTunnelInSetup(t *testing.T) {
	t.Parallel()
	port := localHTTP(t, "127.0.0.1:0", "guest")
	f := newFixture(t, &fixtureOpts{files: map[string]string{
		"hostel.toml": `[tunnels.api]
local_port = ` + fmt.Sprint(port) + `

[tool.fake-screen]
setup = ["printf 'GET /setup HTTP/1.0\\r\\n\\r\\n' | nc -N -w 5 127.0.0.1 $HOSTEL_TUNNEL_API_PORT > setup-http.txt"]
`,
	}})
	f.acquire()
	if got := f.work("cat setup-http.txt"); !strings.Contains(got, "hello from guest /setup") {
		t.Fatalf("setup through the tunnel: %q", got)
	}
}

// TestForwards is scenario 14 (forwards and gateway checks): hostel forward
// reaches a room server; the gateway refuses unregistered remote forwards; an
// open forward or tunnel does not prevent idle expiry.
func TestForwards(t *testing.T) {
	t.Parallel()
	port := localHTTP(t, "127.0.0.1:0", "guest")
	f := newFixture(t, &fixtureOpts{serverKeys: "idle_timeout = \"6s\"\n", files: map[string]string{
		"hostel.toml": "[tunnels.api]\nlocal_port = " + fmt.Sprint(port) + "\n",
	}})
	f.acquire()
	f.e.mustHostel("run", "--detach", "--", "while true; do echo hello-room | nc -N -l 127.0.0.1 7700; done")
	r := f.e.mustHostel("forward", "7700")
	local := strings.TrimSpace(r.stdout)
	var got string
	f.e.waitFor("forward answers", time.Minute, func() bool {
		c, err := net.DialTimeout("tcp", local, 2*time.Second)
		if err != nil {
			return false
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		b, _ := io.ReadAll(c)
		got = string(b)
		return got != ""
	})
	if got != "hello-room\n" {
		t.Fatalf("forward: %q", got)
	}
	if r := f.e.mustHostel("forward", "list"); !strings.Contains(r.stdout, "7700") {
		t.Fatalf("forward list: %s", r.stdout)
	}

	// The gateway allows remote forwards only for registered ports, and only
	// on ports connections.
	key := f.leaseKey()
	id := f.lease1(t)
	ports, err := sshDial(f.s.gateway, id+".ports", key)
	if err != nil {
		t.Fatalf("ports connection: %v", err)
	}
	if ln, err := ports.Listen("tcp", "localhost:7999"); err == nil {
		ln.Close()
		t.Fatal("the gateway accepted a remote forward of an unregistered port")
	}
	ports.Close()
	cmd, err := sshDial(f.s.gateway, id+".cmd", key)
	if err != nil {
		t.Fatalf("cmd connection: %v", err)
	}
	if ln, err := cmd.Listen("tcp", "localhost:7500"); err == nil {
		ln.Close()
		t.Fatal("the gateway accepted a remote forward on a cmd connection")
	}
	cmd.Close()

	// The forward and the tunnel do not keep the lease alive.
	f.waitSlotFree()
	f.e.waitFor("port process stops", time.Minute, func() bool {
		_, err := net.DialTimeout("tcp", local, time.Second)
		return err != nil
	})
}

// lease1 returns the lease ID of the fixture's repository.
func (f *fixture) lease1(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.e.cwd, ".git", "hostel", "lease", "lease.json"))
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		LeaseID string `json:"lease_id"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	return st.LeaseID
}

// TestEndOfAccess is scenario 15.
func TestEndOfAccess(t *testing.T) {
	t.Parallel()
	port := localHTTP(t, "127.0.0.1:0", "guest")
	f := newFixture(t, &fixtureOpts{files: map[string]string{
		"hostel.toml": "[tunnels.api]\nlocal_port = " + fmt.Sprint(port) + "\n",
	}})
	first := f.acquire()
	key1 := f.leaseKey()
	room := fmt.Sprintf("127.0.0.1:%d", f.c.port)
	if c, err := sshDial(room, "hostel", key1); err == nil {
		c.Close()
		t.Fatal("the room accepted the lease key directly")
	}
	shell := make(chan result, 1)
	go func() { shell <- f.e.hostel("ssh", "--", "sleep 60") }()
	f.e.waitFor("shell open", time.Minute, func() bool {
		return strings.Contains(f.c.sh("for p in /proc/[0-9]*; do tr '\\0' ' ' < $p/cmdline 2>/dev/null; echo; done"), "sleep 60")
	})
	f.e.mustHostel("release")
	select {
	case r := <-shell:
		if r.code == 0 {
			t.Fatalf("the shell ended with code 0 after the release: %s", r)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the open shell did not close at the release")
	}
	if c, err := sshDial(f.s.gateway, first+".cmd", key1); err == nil {
		c.Close()
		t.Fatal("the gateway accepted the key of a released lease")
	}
	f.waitSlotFree()
	if got := f.c.sh("nc -z 127.0.0.1 7500 && echo open || echo closed"); got != "closed" {
		t.Fatalf("tunnel port after the release: %s", got)
	}

	// The next lease on the same slot binds the same tunnel port.
	second := f.acquire()
	if r := f.roomGet("$HOSTEL_TUNNEL_API_PORT", "/next"); !strings.Contains(r.stdout, "hello from guest /next") || !strings.Contains(f.e.mustHostel("run", "--", "echo $HOSTEL_TUNNEL_API_PORT").stdout, "7500") {
		t.Fatalf("tunnel of the next lease: %s", r)
	}
	if c, err := sshDial(f.s.gateway, second+".cmd", key1); err == nil {
		c.Close()
		t.Fatal("the gateway accepted the key of another lease")
	}
	key2 := f.leaseKey()
	if c, err := sshDial(room, "hostel", key2); err == nil {
		c.Close()
		t.Fatal("the room accepted the lease key directly")
	}

	// An admin release closes an active guest shell at once.
	go func() { shell <- f.e.hostel("ssh", "--", "sleep 60") }()
	f.e.waitFor("shell open", time.Minute, func() bool {
		return strings.Contains(f.c.sh("for p in /proc/[0-9]*; do tr '\\0' ' ' < $p/cmdline 2>/dev/null; echo; done"), "sleep 60")
	})
	if code := f.s.do("POST", "/v1/leases/"+second+"/release", f.s.token, nil); code != 200 {
		t.Fatalf("admin release: HTTP %d", code)
	}
	select {
	case <-shell:
	case <-time.After(15 * time.Second):
		t.Fatal("the open shell did not close at the admin release")
	}
	f.waitSlotFree()
	if c, err := sshDial(room, "hostel", key2); err == nil {
		c.Close()
		t.Fatal("the room accepted the lease key after the lease")
	}
}
