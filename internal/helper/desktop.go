package helper

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/csansoon/hostel/internal/config"
)

// The desktop program ("machine desktop serve") runs as the machine user in the
// desktop session, which SSH sessions cannot reach (Windows: a scheduled task
// of the desktop user starts it; m7-implementation.md). "helper desktop run"
// sends it a command of a lease, and it runs the command there.

const (
	desktopRequestTimeout = 10 * time.Second
	desktopDialTimeout    = 5 * time.Second
	desktopPoll           = 2 * time.Second
	desktopMaxRequest     = 1 << 20
	// desktopWaitDelay: a command's output ends this long after it exits,
	// also when a process that it started keeps the pipes open.
	desktopWaitDelay = 2 * time.Second
)

// Frame kinds from the desktop program to "helper desktop run".
const (
	frameStdout byte = 1
	frameStderr byte = 2
	frameExit   byte = 3
	frameError  byte = 4
)

type desktopAddr struct {
	Addr  string `json:"addr"`
	Token string `json:"token"`
}

type desktopRequest struct {
	Token  string   `json:"token"`
	Room   string   `json:"room"`
	Lease  string   `json:"lease"`
	Argv   []string `json:"argv"`
	Env    []string `json:"env"`
	Dir    string   `json:"dir"`
	Detach bool     `json:"detach"`
}

func (h *helper) desktopFile() string { return filepath.Join(h.root, "desktop.json") }

// leaseEndedFile marks a room whose cleanup has started; the desktop program
// then ends the lease's commands.
func (h *helper) leaseEndedFile(room string) string {
	return filepath.Join(h.roomDir(room), "lease-ended")
}

// desktopLeaseEnded reports whether the lease of a room has ended: no
// lease.json, another lease, or the cleanup has started. Other read errors
// do not end it.
func (h *helper) desktopLeaseEnded(room, leaseID string) bool {
	if _, err := os.Stat(h.leaseEndedFile(room)); err == nil {
		return true
	}
	l, err := h.readLease(room)
	if err != nil {
		_, statErr := os.Stat(filepath.Join(h.roomDir(room), "lease.json"))
		return errors.Is(statErr, os.ErrNotExist)
	}
	return l.LeaseID != leaseID
}

func (h *helper) desktop(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "serve" {
		return streamResult(h.desktopServe(), stderr)
	}
	if len(args) > 0 && args[0] == "run" {
		return h.desktopRun(args[1:], stdout, stderr)
	}
	fmt.Fprintln(stderr, "hostel helper: desktop serve|run")
	return 2
}

// desktopProc is a command that the desktop program started for a lease.
type desktopProc struct {
	cmd   *exec.Cmd
	room  string
	lease string
}

// desktopServe listens on loopback; desktop.json (only the machine user can read
// it) has the address and the token. It ends when its executable changes, so
// the task that started it starts the new helper.
func (h *helper) desktopServe() error {
	exe, err := os.Stat(h.exe)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer ln.Close()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	token := hex.EncodeToString(b)
	data, _ := json.Marshal(desktopAddr{Addr: ln.Addr().String(), Token: token})
	if err := writeFileAtomic(h.desktopFile(), data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", h.desktopFile(), err)
	}
	// A caller must not send its token and environment to a later listener
	// on the same port.
	defer func() {
		if cur, err := os.ReadFile(h.desktopFile()); err == nil && string(cur) == string(data) {
			os.Remove(h.desktopFile())
		}
	}()
	var mu sync.Mutex
	procs := map[*desktopProc]bool{}
	var next int
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			next++
			n := next
			mu.Unlock()
			go h.desktopServeConn(conn, token, n, func(p *desktopProc) {
				mu.Lock()
				procs[p] = true
				mu.Unlock()
			})
		}
	}()
	// A command and the processes that it started stay until the lease ends
	// (an app launcher can exit before its app). Then their job object ends
	// them: a process that cleared HOSTEL_LEASE is not found by cleanup.
	for {
		time.Sleep(desktopPoll)
		mu.Lock()
		for p := range procs {
			if h.desktopLeaseEnded(p.room, p.lease) {
				killProcessGroup(p.cmd)
				releaseGroup(p.cmd)
				delete(procs, p)
			}
		}
		idle := len(procs) == 0
		mu.Unlock()
		// A new helper takes over only when no lease command runs: ending this
		// program ends their job objects.
		if st, err := os.Stat(h.exe); idle && (err != nil || !st.ModTime().Equal(exe.ModTime()) || st.Size() != exe.Size()) {
			return nil
		}
	}
}

func (h *helper) desktopServeConn(conn net.Conn, token string, n int, track func(*desktopProc)) {
	defer conn.Close()
	var req desktopRequest
	conn.SetReadDeadline(time.Now().Add(desktopRequestTimeout))
	if err := json.NewDecoder(bufio.NewReader(io.LimitReader(conn, desktopMaxRequest))).Decode(&req); err != nil {
		return
	}
	conn.SetReadDeadline(time.Time{})
	var wmu sync.Mutex
	send := func(kind byte, p []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		var hdr [5]byte
		hdr[0] = kind
		binary.BigEndian.PutUint32(hdr[1:], uint32(len(p)))
		if _, err := conn.Write(hdr[:]); err != nil {
			return err
		}
		_, err := conn.Write(p)
		return err
	}
	fail := func(err error) { send(frameError, []byte(err.Error())) }
	if subtle.ConstantTimeCompare([]byte(req.Token), []byte(token)) != 1 {
		fail(errors.New("wrong token"))
		return
	}
	if !config.ValidName(req.Room) || len(req.Argv) == 0 {
		fail(errors.New("the request needs a room and a command"))
		return
	}
	lease, err := h.readLease(req.Room)
	if err != nil {
		fail(err)
		return
	}
	// Only the lease of the room, and only while it holds the whole machine: two
	// leases must not share one desktop.
	if req.Lease != lease.LeaseID || h.desktopLeaseEnded(req.Room, lease.LeaseID) {
		fail(fmt.Errorf("the caller is not the lease of room %s", req.Room))
		return
	}
	if !lease.Exclusive {
		fail(errors.New("the desktop needs a lease with windows-desktop: hostel acquire windows-desktop"))
		return
	}
	env := envMap(req.Env)
	env["HOSTEL_LEASE"] = lease.LeaseID
	env["HOSTEL_ROOM"] = req.Room
	path, err := lookPath(req.Argv[0], envList(env))
	if err != nil {
		fail(fmt.Errorf("%s: command not found in the lease PATH", req.Argv[0]))
		return
	}
	dir := req.Dir
	if dir == "" {
		dir = h.workDir(req.Room)
	}
	cmd := &exec.Cmd{Path: path, Args: req.Argv, Dir: dir, Env: envList(env), WaitDelay: desktopWaitDelay}
	if !req.Detach {
		cmd.Stdout = frameWriter{send, frameStdout}
		cmd.Stderr = frameWriter{send, frameStderr}
	}
	if err := startInGroup(cmd, jobGroupName(req.Room, "desktop-"+strconv.Itoa(n))); err != nil {
		fail(fmt.Errorf("start %s: %w", req.Argv[0], err))
		return
	}
	track(&desktopProc{cmd: cmd, room: req.Room, lease: lease.LeaseID})
	done := make(chan struct{})
	go func() {
		cmd.Wait()
		close(done)
	}()
	if req.Detach {
		send(frameExit, []byte("0 "+strconv.Itoa(cmd.Process.Pid)))
		return
	}
	// The caller ended (for example "hostel run" was stopped): end the command.
	go func() {
		io.Copy(io.Discard, conn)
		select {
		case <-done:
		default:
			killProcessGroup(cmd)
		}
	}()
	<-done
	send(frameExit, []byte(strconv.Itoa(exitCode(cmd.ProcessState))))
}

type frameWriter struct {
	send func(byte, []byte) error
	kind byte
}

func (w frameWriter) Write(p []byte) (int, error) {
	if err := w.send(w.kind, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// desktopRun is "helper desktop run [--detach] -- <command>": it runs the
// command in the desktop session with this lease environment and returns its
// output and exit code. With --detach, it prints the PID and returns at once.
func (h *helper) desktopRun(args []string, stdout, stderr io.Writer) int {
	// The flags end at the command, whose own flags pass through.
	detach := len(args) > 0 && args[0] == "--detach"
	if detach {
		args = args[1:]
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: desktop [--detach] <command> [args...]")
		return 2
	}
	argv := args
	room := os.Getenv("HOSTEL_ROOM")
	if room == "" {
		fmt.Fprintln(stderr, "hostel: desktop runs only in a lease (HOSTEL_ROOM is not set)")
		return 2
	}
	var addr desktopAddr
	data, err := os.ReadFile(h.desktopFile())
	if err == nil {
		err = json.Unmarshal(data, &addr)
	}
	var conn net.Conn
	if err == nil {
		conn, err = net.DialTimeout("tcp", addr.Addr, desktopDialTimeout)
	}
	if err != nil {
		fmt.Fprintf(stderr, "hostel: this machine has no desktop session (%v). The machine needs a signed-in desktop and the Hostel desktop task (docs/static-machine-setup.md).\n", err)
		return 69
	}
	defer conn.Close()
	dir, _ := os.Getwd()
	req, _ := json.Marshal(desktopRequest{Token: addr.Token, Room: room, Lease: os.Getenv("HOSTEL_LEASE"), Argv: argv, Env: os.Environ(), Dir: dir, Detach: detach})
	if _, err := conn.Write(req); err != nil {
		fmt.Fprintf(stderr, "hostel: send the command to the desktop session: %v\n", err)
		return 69
	}
	r := bufio.NewReader(conn)
	for {
		var hdr [5]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			fmt.Fprintf(stderr, "hostel: the desktop session closed the connection: %v\n", err)
			return 69
		}
		p := make([]byte, binary.BigEndian.Uint32(hdr[1:]))
		if _, err := io.ReadFull(r, p); err != nil {
			fmt.Fprintf(stderr, "hostel: read from the desktop session: %v\n", err)
			return 69
		}
		switch hdr[0] {
		case frameStdout:
			stdout.Write(p)
		case frameStderr:
			stderr.Write(p)
		case frameError:
			fmt.Fprintf(stderr, "hostel: desktop: %s\n", p)
			return 127
		case frameExit:
			var code, pid int
			fmt.Sscan(string(p), &code, &pid)
			if detach {
				fmt.Fprintf(stdout, "started on the desktop, PID %d\n", pid)
			}
			return code
		}
	}
}
