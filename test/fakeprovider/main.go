//go:build !windows

// Command fakeprovider is an external provider for the E2E tests (design,
// section 20). It creates Docker rooms from test/roomimage. Environment:
//
//	FAKE_RUN_ID     label value hostel-e2e=<run id> of all its containers (required)
//	FAKE_STATE_DIR  directory for lock files and counters (required)
//	FAKE_IMAGE      room image (default hostel-e2e-room)
//	FAKE_DELAY      wait before docker run, for example 5s (a window for kill -9)
//	FAKE_LIMIT      maximum number of its containers; more gives the "limit" error
//	FAKE_FAIL       number of provision calls that fail with "other" before one works
//	FAKE_CAN_STOP   "1": machines can stop and resume
//	FAKE_PROXY      "1": access goes through a proxy command (nc) instead of host and port
//
// Under the name "devbox" (a link), it is a fake Namespace devbox CLI instead
// (devbox.go); under the name "nsc", a fake nsc CLI, and under the name
// "docker", the docker shim of a fake instance (nsc.go).
//
// It runs on Unix test hosts only (it uses flock).
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type request struct {
	Name      string `json:"name"`
	Machine   string `json:"machine"`
	ID        string `json:"id"`
	ServerKey string `json:"server_key"`
	Prefix    string `json:"prefix"`
}

type failure struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

var errNotFound = errors.New("not found")

func main() {
	switch filepath.Base(os.Args[0]) {
	case "devbox":
		devboxMain(os.Args[1:])
		return
	case "nsc":
		nscMain(os.Args[1:])
		return
	case "docker":
		dockerShimMain(os.Args[1:])
		return
	}
	if len(os.Args) != 2 {
		fail("other", "usage: fakeprovider <operation>")
	}
	var req request
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		fail("other", "read the request: "+err.Error())
	}
	out, err := run(os.Args[1], req)
	switch {
	case errors.Is(err, errNotFound):
		fail("not_found", err.Error())
	case errors.Is(err, errLimit):
		fail("limit", err.Error())
	case err != nil:
		fail("other", err.Error())
	}
	json.NewEncoder(os.Stdout).Encode(out)
}

var errLimit = errors.New("fake limit reached")

func fail(kind, msg string) {
	json.NewEncoder(os.Stdout).Encode(failure{Error: kind, Message: msg})
	os.Exit(1)
}

func docker(args ...string) (string, error) {
	var out, errb bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func runID() string { return os.Getenv("FAKE_RUN_ID") }

// containers returns name → state of the containers of this run that this
// provider created.
func containers() (map[string]string, error) {
	out, err := docker("ps", "-a", "--filter", "label=hostel-e2e="+runID(), "--filter", "label=hostel-fake=1",
		"--format", "{{.Names}}\t{{.State}}")
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if name, state, ok := strings.Cut(line, "\t"); ok {
			m[name] = state
		}
	}
	return m, nil
}

func run(op string, req request) (any, error) {
	switch op {
	case "capabilities":
		// extend is a no-op: test rooms end with the test.
		return map[string]bool{"can_stop": os.Getenv("FAKE_CAN_STOP") == "1", "can_extend": true}, nil
	case "provision":
		return provision(req)
	case "access":
		return access(req.ID)
	case "resume":
		if err := exists(req.ID); err != nil {
			return nil, err
		}
		if _, err := docker("start", req.ID); err != nil {
			return nil, err
		}
		return struct{}{}, waitSSH(req.ID)
	case "stop":
		if err := exists(req.ID); err != nil {
			return nil, err
		}
		_, err := docker("stop", "-t", "1", req.ID)
		return struct{}{}, err
	case "extend":
		return struct{}{}, nil
	case "destroy":
		name := req.ID
		if name == "" {
			name = req.Name
		}
		if err := exists(name); err != nil {
			return nil, err
		}
		_, err := docker("rm", "-f", "-v", name)
		return struct{}{}, err
	case "list":
		cs, err := containers()
		if err != nil {
			return nil, err
		}
		type room struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			State string `json:"state"`
		}
		rooms := []room{}
		for name, state := range cs {
			if strings.HasPrefix(name, req.Prefix) {
				if state != "running" {
					state = "stopped"
				}
				rooms = append(rooms, room{ID: name, Name: name, State: state})
			}
		}
		return map[string]any{"rooms": rooms}, nil
	}
	return nil, fmt.Errorf("unknown operation %q", op)
}

func exists(name string) error {
	cs, err := containers()
	if err != nil {
		return err
	}
	if _, ok := cs[name]; !ok {
		return fmt.Errorf("%w: no room %s", errNotFound, name)
	}
	return nil
}

// provision is idempotent by name, also under concurrent calls: a lock file
// for each name makes a second call wait, and it then returns the room.
func provision(req request) (any, error) {
	dir := os.Getenv("FAKE_STATE_DIR")
	lock, err := os.OpenFile(filepath.Join(dir, req.Name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	cs, err := containers()
	if err != nil {
		return nil, err
	}
	if state, ok := cs[req.Name]; ok {
		if state != "running" {
			if _, err := docker("start", req.Name); err != nil {
				return nil, err
			}
		}
		if err := waitSSH(req.Name); err != nil {
			return nil, err
		}
		return access(req.Name)
	}
	if n, err := strconv.Atoi(os.Getenv("FAKE_LIMIT")); err == nil && len(cs) >= n {
		return nil, fmt.Errorf("%w (%d rooms)", errLimit, n)
	}
	if n, err := strconv.Atoi(os.Getenv("FAKE_FAIL")); err == nil {
		counter := filepath.Join(dir, "failures")
		data, _ := os.ReadFile(counter)
		done, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		if done < n {
			os.WriteFile(counter, []byte(strconv.Itoa(done+1)), 0o600)
			return nil, fmt.Errorf("injected failure %d of %d", done+1, n)
		}
	}
	if d, err := time.ParseDuration(os.Getenv("FAKE_DELAY")); err == nil {
		time.Sleep(d)
	}
	image := os.Getenv("FAKE_IMAGE")
	if image == "" {
		image = "hostel-e2e-room"
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	if _, err := docker("run", "-d", "--name", req.Name, "--label", "hostel-e2e="+runID(), "--label", "hostel-fake=1",
		"-p", fmt.Sprintf("127.0.0.1:%d:22", port), "-e", "HOSTEL_AUTHORIZED_KEY="+req.ServerKey, image); err != nil {
		return nil, err
	}
	if err := waitSSH(req.Name); err != nil {
		return nil, err
	}
	return access(req.Name)
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func hostPort(name string) (int, error) {
	out, err := docker("port", name, "22/tcp")
	if err != nil {
		return 0, err
	}
	_, p, err := net.SplitHostPort(strings.Split(out, "\n")[0])
	if err != nil {
		return 0, fmt.Errorf("parse the port of %s: %q", name, out)
	}
	return strconv.Atoi(p)
}

func waitSSH(name string) error {
	port, err := hostPort(name)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err == nil {
			c.SetDeadline(time.Now().Add(2 * time.Second))
			buf := make([]byte, 4)
			_, err = io.ReadFull(c, buf)
			c.Close()
			if err == nil && string(buf) == "SSH-" {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("room %s: no SSH banner after 1 minute", name)
}

func access(name string) (any, error) {
	if err := exists(name); err != nil {
		return nil, err
	}
	port, err := hostPort(name)
	if err != nil {
		return nil, err
	}
	if os.Getenv("FAKE_PROXY") == "1" {
		return map[string]any{"id": name, "user": "hostel", "proxy_command": []string{"nc", "127.0.0.1", strconv.Itoa(port)}}, nil
	}
	return map[string]any{"id": name, "user": "hostel", "ssh": map[string]any{"host": "127.0.0.1", "port": port}}, nil
}
