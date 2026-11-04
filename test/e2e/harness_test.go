//go:build e2e

// Package e2e runs the real hostel binaries (design, section 20).
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
)

// status returns the server status.
func (s *serverProc) status() protocol.Status {
	s.e.t.Helper()
	var st protocol.Status
	if code := s.get("/v1/status", s.token, &st); code != http.StatusOK {
		s.e.t.Fatalf("GET /v1/status: HTTP %d", code)
	}
	return st
}

// room returns the status of the room of a machine ("<provider>/<machine>").
func (s *serverProc) room(machine string) protocol.RoomStatus {
	s.e.t.Helper()
	for _, r := range s.status().Rooms {
		if r.Provider+"/"+r.Machine == machine {
			return r
		}
	}
	return protocol.RoomStatus{}
}

// waitRoom waits until the room of a machine is in state.
func (s *serverProc) waitRoom(machine, state string, timeout time.Duration) protocol.RoomStatus {
	s.e.t.Helper()
	var r protocol.RoomStatus
	s.e.waitFor("room "+machine+" "+state, timeout, func() bool {
		r = s.room(machine)
		return r.State == state
	})
	return r
}

// hostelBin is the host binary that TestMain builds, in binDir.
var hostelBin, binDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "hostel-e2e-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		os.Exit(1)
	}
	binDir, hostelBin = dir, filepath.Join(dir, "hostel")
	build := exec.Command("go", "build", "-o", hostelBin, "../../cmd/hostel")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: build hostel:", err)
		os.Exit(1)
	}
	if err := setupDocker(dir); err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// env is one isolated machine: a temporary home directory for the server and
// the guest.
type env struct {
	t     *testing.T
	home  string
	runID string
	// vars are extra environment variables for guest commands, for example
	// HOSTEL_SERVER after the server starts.
	vars map[string]string
	// cwd is the directory of guest commands (the test repository, if any).
	cwd string
	// admin is the token of the first admin user, made when the server first
	// starts ("hostel server invite" and "hostel connect"); key is the
	// server's pinned TLS key ("" for a server without its own TLS).
	admin string
	key   string
	// slotUsers are extra users of static room containers (slot users).
	slotUsers []string
	// guests counts the member users of useGuestToken.
	guests int
	// bin and serverBin replace hostelBin for guest commands and for the
	// server (the update tests run other versions).
	bin, serverBin string
}

// guestBin is the binary of guest commands.
func (e *env) guestBin() string {
	if e.bin != "" {
		return e.bin
	}
	return hostelBin
}

func newEnv(t *testing.T) *env {
	t.Helper()
	b := make([]byte, 6)
	rand.Read(b)
	e := &env{t: t, home: t.TempDir(), runID: fmt.Sprintf("%x", b), vars: map[string]string{}}
	e.cwd = e.home
	t.Cleanup(e.removeContainers)
	t.Cleanup(e.stopPortProcesses)
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		logs, _ := filepath.Glob(filepath.Join(e.home, "*", ".git", "hostel", "*", "ports.log"))
		for _, l := range logs {
			data, _ := os.ReadFile(l)
			t.Logf("%s:\n%s", l, data)
		}
	})
	return e
}

func (e *env) environ() []string {
	out := []string{"HOME=" + e.home, "PATH=" + os.Getenv("PATH"), "TMPDIR=" + os.TempDir()}
	for k, v := range e.vars {
		out = append(out, k+"="+v)
	}
	return out
}

func (e *env) configDir() string { return filepath.Join(e.home, ".config", "hostel") }
func (e *env) dataDir() string   { return filepath.Join(e.home, ".local", "share", "hostel") }

type result struct {
	code           int
	stdout, stderr string
}

func (r result) String() string {
	return fmt.Sprintf("exit code %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
}

// hostel runs the hostel binary with a deadline and returns its result.
func (e *env) hostel(args ...string) result {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.guestBin(), args...)
	cmd.Env = e.environ()
	cmd.Dir = e.cwd
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			e.t.Fatalf("run hostel %s: %v", strings.Join(args, " "), err)
		}
		code = exitErr.ExitCode()
	}
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

var (
	inviteRE = regexp.MustCompile(`hostel connect (hostel1-\S+)`)
	tokenRE  = regexp.MustCompile(`(?m)^token = '?"?([^'"\n]+)`)
	keyRE    = regexp.MustCompile(`(?m)^key = '?"?([^'"\n]+)`)
)

// init runs "hostel server init". The first admin connects when the server
// first starts (startServer); guest commands use the admin's token unless a
// test sets HOSTEL_TOKEN. It returns "" (the token does not exist yet).
func (e *env) init() string {
	e.t.Helper()
	r := e.hostel("server", "init", "--user", "admin")
	if r.code != 0 {
		e.t.Fatalf("server init: %s", r)
	}
	return ""
}

// connect runs "hostel connect" with the code on a new computer (its own home)
// and returns the token and the pinned key from its connection file.
func (e *env) connect(code, name string) (string, string) {
	e.t.Helper()
	home := e.t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, hostelBin, "connect", code, "--name", name)
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "TMPDIR=" + os.TempDir()}
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("hostel connect as %s: %v\n%s", name, err, out)
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "hostel", "config.toml"))
	if err != nil {
		e.t.Fatal(err)
	}
	tok, key := tokenRE.FindSubmatch(data), keyRE.FindSubmatch(data)
	if tok == nil {
		e.t.Fatalf("the connection file has no token:\n%s", data)
	}
	if key == nil {
		return string(tok[1]), ""
	}
	return string(tok[1]), string(key[1])
}

// newUser invites a user (as the admin) and connects it; it returns its token.
func (e *env) newUser(name, role string) string {
	e.t.Helper()
	saved := e.vars["HOSTEL_TOKEN"]
	e.vars["HOSTEL_TOKEN"] = e.admin
	r := e.mustHostel("config", "users", "invite", "--name", name, "--role", role)
	e.vars["HOSTEL_TOKEN"] = saved
	m := inviteRE.FindStringSubmatch(r.stdout)
	if m == nil {
		e.t.Fatalf("users invite printed no code: %s", r)
	}
	tok, _ := e.connect(m[1], name)
	return tok
}

// writeConfig replaces server.toml.
func (e *env) writeConfig(config string) {
	e.writeFile(filepath.Join(e.configDir(), "server.toml"), config)
}

// initServer runs init and writes server.toml.
func (e *env) initServer(config string) string {
	e.t.Helper()
	token := e.init()
	e.writeConfig(config)
	return token
}

func (e *env) writeFile(path, body string) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// serverHeader returns the server.toml keys that every test uses: a unique
// server_id and random free ports on 127.0.0.1. Rooms get no agent-device (a
// download), except in TestMise.
func serverHeader() string { return serverHeaderWith(false) }

func serverHeaderWith(agentDevice bool) string {
	b := make([]byte, 6)
	rand.Read(b)
	id := fmt.Sprintf("e2e%x", b)
	return fmt.Sprintf("server_id = %q\nlisten = \"127.0.0.1:0\"\ngateway_listen = \"127.0.0.1:0\"\nhelper_dir = %q\n"+
		"tls = \"self-signed\"\npublic_urls = [\"https://127.0.0.1:0\"]\n"+
		"intervals = { scheduler = \"1s\", reconcile = \"2s\", tunnel_check = \"1s\", presence = \"1s\" }\nmise = { agent_device = %t }\n", id, helperDir, agentDevice)
}

type serverProc struct {
	e       *env
	cmd     *exec.Cmd
	addr    string
	gateway string
	url     string
	token   string
	done    chan struct{}
	log     *syncBuffer
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startServer starts "hostel server run" and waits until it listens. The
// server stops at the end of the test; its log is printed if the test failed.
func (e *env) startServer(token string) *serverProc {
	e.t.Helper()
	addrFile := filepath.Join(e.dataDir(), "server.json")
	os.Remove(addrFile)
	s := &serverProc{e: e, token: token, done: make(chan struct{}), log: &syncBuffer{}}
	bin := hostelBin
	if e.serverBin != "" {
		bin = e.serverBin
	}
	s.cmd = exec.Command(bin, "server", "run")
	s.cmd.Env = e.environ()
	s.cmd.Stdout, s.cmd.Stderr = s.log, s.log
	if err := s.cmd.Start(); err != nil {
		e.t.Fatalf("start server: %v", err)
	}
	go func() { s.cmd.Wait(); close(s.done) }()
	e.t.Cleanup(func() {
		s.stop()
		if e.t.Failed() {
			e.t.Logf("server log:\n%s", s.log.String())
		}
	})
	e.waitFor("server listens", 30*time.Second, func() bool {
		select {
		case <-s.done:
			e.t.Fatalf("server exited at start: %s", s.log.String())
		default:
		}
		data, err := os.ReadFile(addrFile)
		if err != nil {
			return false
		}
		var a protocol.Addresses
		if json.Unmarshal(data, &a) != nil {
			return false
		}
		s.addr, s.gateway, s.url = a.Listen, a.Gateway, a.URL
		return true
	})
	e.vars["HOSTEL_SERVER"] = s.url
	if e.admin == "" {
		r := e.mustHostel("server", "invite", "--role", "admin", "--name", "admin")
		m := inviteRE.FindStringSubmatch(r.stdout)
		if m == nil {
			e.t.Fatalf("server invite printed no code: %s", r)
		}
		e.admin, e.key = e.connect(m[1], "admin")
	}
	s.token = e.admin
	if e.key != "" {
		e.vars["HOSTEL_SERVER_KEY"] = e.key
	}
	if e.vars["HOSTEL_TOKEN"] == "" {
		e.vars["HOSTEL_TOKEN"] = e.admin
	}
	return s
}

// stop sends SIGTERM and waits; after 10 seconds it kills the server.
func (s *serverProc) stop() {
	select {
	case <-s.done:
		return
	default:
	}
	s.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
		s.cmd.Process.Kill()
		<-s.done
	}
}

// get sends an API request with the token and decodes the JSON response.
func (s *serverProc) get(path, token string, out any) int {
	s.e.t.Helper()
	return s.do(http.MethodGet, path, token, out)
}

// put sends a PUT request with a JSON body and returns the HTTP status.
func (s *serverProc) put(path, token, body string) int {
	s.e.t.Helper()
	return s.send(http.MethodPut, path, token, strings.NewReader(body), nil)
}

// do sends an API request without a body and decodes the JSON response.
func (s *serverProc) do(method, path, token string, out any) int {
	s.e.t.Helper()
	return s.send(method, path, token, nil, out)
}

func (s *serverProc) send(method, path, token string, body io.Reader, out any) int {
	s.e.t.Helper()
	req, err := http.NewRequest(method, s.url+path, body)
	if err != nil {
		s.e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := insecureClient().Do(req)
	if err != nil {
		s.e.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			s.e.t.Fatalf("GET %s: decode: %v", path, err)
		}
	}
	return resp.StatusCode
}

// insecureClient reaches the test's own server; the client commands check
// the pinned key.
func insecureClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	return &http.Client{Timeout: 30 * time.Second, Transport: transport}
}

// waitFor polls cond until it is true. It fails the test after timeout.
func (e *env) waitFor(what string, timeout time.Duration, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timeout after %s: %s", timeout, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// newRepo creates a git repository with files (path → content) and makes it
// the directory of guest commands.
func (e *env) newRepo(files map[string]string) string {
	e.t.Helper()
	return e.newRepoNamed("repo", files)
}

// newRepoNamed creates another repository (another worktree of a guest).
func (e *env) newRepoNamed(name string, files map[string]string) string {
	e.t.Helper()
	dir := filepath.Join(e.home, name)
	for p, body := range files {
		e.writeFile(filepath.Join(dir, p), body)
	}
	e.git(dir, "init", "-q")
	e.cwd = dir
	return dir
}

func (e *env) git(dir string, args ...string) {
	e.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = e.environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		e.t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

// usePlatform copies a platform directory from test/platforms to the server
// configuration.
func (e *env) usePlatform(name string) {
	e.t.Helper()
	src := filepath.Join("..", "platforms", name)
	des, err := os.ReadDir(src)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, de := range des {
		data, err := os.ReadFile(filepath.Join(src, de.Name()))
		if err != nil {
			e.t.Fatal(err)
		}
		e.writeFile(filepath.Join(e.configDir(), "platforms", name, de.Name()), string(data))
	}
}

// mustHostel runs a guest command and fails the test if its exit code is not 0.
func (e *env) mustHostel(args ...string) result {
	e.t.Helper()
	r := e.hostel(args...)
	if r.code != 0 {
		e.t.Fatalf("hostel %s: %s", strings.Join(args, " "), r)
	}
	return r
}

// restart stops the server and starts it again on the same ports, so guests
// and port processes find it again.
func (s *serverProc) restart() *serverProc {
	s.e.t.Helper()
	s.stop()
	s.pinPorts()
	return s.e.startServer(s.token)
}

// pinPorts writes the server's ports into server.toml, so that a new start
// binds the same ports.
func (s *serverProc) pinPorts() {
	s.e.t.Helper()
	file := filepath.Join(s.e.configDir(), "server.toml")
	data, err := os.ReadFile(file)
	if err != nil {
		s.e.t.Fatal(err)
	}
	cfg := strings.Replace(string(data), "\nlisten = \"127.0.0.1:0\"", fmt.Sprintf("\nlisten = %q", s.addr), 1)
	cfg = strings.Replace(cfg, "\ngateway_listen = \"127.0.0.1:0\"", fmt.Sprintf("\ngateway_listen = %q", s.gateway), 1)
	s.e.writeConfig(cfg)
}

// useGuestToken makes a member user and makes guest commands use its token.
// The admin's token stays in serverProc.token. Each call makes a new user.
func (e *env) useGuestToken() string {
	e.t.Helper()
	e.guests++
	name := "guest"
	if e.guests > 1 {
		name = fmt.Sprintf("guest%d", e.guests)
	}
	e.vars["HOSTEL_TOKEN"] = e.newUser(name, "member")
	return e.vars["HOSTEL_TOKEN"]
}

// stopPortProcesses stops the "hostel ports" processes of this test: their
// arguments contain the test's home directory, with symbolic links resolved
// (git gives the resolved path; on macOS /var is a link to /private/var).
func (e *env) stopPortProcesses() {
	home, err := filepath.EvalSymlinks(e.home)
	if err != nil {
		home = e.home
	}
	exec.Command("pkill", "-f", "hostel ports --dir "+home+"/").Run()
}

// kill stops the server with SIGKILL, as a crash.
func (s *serverProc) kill() {
	s.cmd.Process.Kill()
	<-s.done
}

// restartAfterKill kills the server and starts it again on the same ports.
func (s *serverProc) restartAfterKill() *serverProc {
	s.e.t.Helper()
	s.kill()
	return s.restart()
}
