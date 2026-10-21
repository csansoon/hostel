//go:build e2e

// Package e2e runs the real hostel binaries (design, section 20).
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
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

// hostelBin is the host binary that TestMain builds.
var hostelBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "hostel-e2e-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		os.Exit(1)
	}
	hostelBin = filepath.Join(dir, "hostel")
	build := exec.Command("go", "build", "-o", hostelBin, "../../cmd/hostel")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: build hostel:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// env is one isolated machine: a temporary home directory for the server and
// the guest.
type env struct {
	t    *testing.T
	home string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return &env{t: t, home: t.TempDir()}
}

func (e *env) environ() []string {
	return []string{"HOME=" + e.home, "PATH=" + os.Getenv("PATH"), "TMPDIR=" + os.TempDir()}
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
	cmd := exec.CommandContext(ctx, hostelBin, args...)
	cmd.Env = e.environ()
	cmd.Dir = e.home
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

var adminTokenRE = regexp.MustCompile(`admin token \(shown only once\): (\S+)`)

// initServer runs "hostel server init", replaces the template with config, and
// returns the admin token.
func (e *env) initServer(config string) string {
	e.t.Helper()
	r := e.hostel("server", "init")
	if r.code != 0 {
		e.t.Fatalf("server init: %s", r)
	}
	m := adminTokenRE.FindStringSubmatch(r.stdout)
	if m == nil {
		e.t.Fatalf("server init printed no admin token: %s", r)
	}
	e.writeFile(filepath.Join(e.configDir(), "server.toml"), config)
	return m[1]
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
// server_id and random free ports on 127.0.0.1.
func serverHeader() string {
	b := make([]byte, 6)
	rand.Read(b)
	id := fmt.Sprintf("e2e%x", b)
	return fmt.Sprintf("server_id = %q\nlisten = \"127.0.0.1:0\"\ngateway_listen = \"127.0.0.1:0\"\n", id)
}

type serverProc struct {
	e     *env
	cmd   *exec.Cmd
	addr  string
	token string
	done  chan struct{}
	log   *syncBuffer
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
	s.cmd = exec.Command(hostelBin, "server", "run")
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
		s.addr = a.Listen
		return true
	})
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
	req, err := http.NewRequest(http.MethodGet, "http://"+s.addr+path, nil)
	if err != nil {
		s.e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
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
