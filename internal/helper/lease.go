package helper

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/shell"
)

// miseEnvTimeout bounds "mise env --json" at each job or shell start.
const miseEnvTimeout = 30 * time.Second

func (h *helper) workDir(room string) string { return filepath.Join(h.roomDir(room), "work") }

func roomFlag(name string, args []string) (string, []string, error) {
	fs := cli.NewFlagSet(name, io.Discard)
	room := fs.String("room", "", "room name")
	pos, err := cli.Parse(fs, args)
	if err != nil {
		return "", nil, err
	}
	if !config.ValidName(*room) {
		return "", nil, fmt.Errorf("%w: %s needs --room <room>", cli.ErrUsage, name)
	}
	return *room, pos, nil
}

// begin writes the lease data of a room and saves authorized_keys and the
// persistence locations as the base that cleanup restores (a guest must not
// keep a key or a program that starts later).
func (h *helper) begin(args []string, stdin io.Reader) (struct{}, error) {
	room, _, err := roomFlag("begin", args)
	if err != nil {
		return struct{}{}, err
	}
	var req protocol.BeginRequest
	if err := json.NewDecoder(stdin).Decode(&req); err != nil {
		return struct{}{}, fmt.Errorf("read the request: %w", err)
	}
	// rooms/ only for the user: lease users may traverse hostel/ (M11).
	if err := os.MkdirAll(filepath.Join(h.root, "rooms"), 0o700); err != nil {
		return struct{}{}, err
	}
	if err := os.Chmod(filepath.Join(h.root, "rooms"), 0o700); err != nil {
		return struct{}{}, err
	}
	if err := os.MkdirAll(h.workDir(room), 0o755); err != nil {
		return struct{}{}, err
	}
	data, err := json.Marshal(req)
	if err != nil {
		return struct{}{}, err
	}
	if err := writeFileAtomic(filepath.Join(h.roomDir(room), "lease.json"), data, 0o644); err != nil {
		return struct{}{}, err
	}
	// The base comes from a moment when the machine has no other lease: with a
	// lease in another room, authorized_keys can have a key that that lease
	// added, and the base must not keep it.
	if busy, err := h.otherRoomBusy(room); err != nil || busy {
		return struct{}{}, err
	}
	keys, err := os.ReadFile(h.authorizedKeysPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return struct{}{}, err
	}
	if err := writeFileAtomic(h.authorizedKeysBase(), keys, 0o600); err != nil {
		return struct{}{}, err
	}
	if err := h.snapshotPersistence(); err != nil {
		return struct{}{}, fmt.Errorf("copy the persistence locations: %w", err)
	}
	return struct{}{}, nil
}

// otherRoomBusy reports whether another room of the machine has a lease.
func (h *helper) otherRoomBusy(room string) (bool, error) {
	des, err := os.ReadDir(filepath.Join(h.root, "rooms"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	for _, de := range des {
		if de.Name() == room || !de.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(h.roomDir(de.Name()), "lease.json")); err == nil {
			return true, nil
		}
	}
	return false, nil
}

func (h *helper) authorizedKeysPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ssh", "authorized_keys")
}

func (h *helper) authorizedKeysBase() string { return filepath.Join(h.root, "authorized_keys.base") }

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (h *helper) readLease(room string) (protocol.BeginRequest, error) {
	var l protocol.BeginRequest
	data, err := os.ReadFile(filepath.Join(h.roomDir(room), "lease.json"))
	if errors.Is(err, os.ErrNotExist) {
		return l, fmt.Errorf("room %s has no lease", room)
	}
	if err != nil {
		return l, err
	}
	return l, json.Unmarshal(data, &l)
}

// setEnv writes lease.env: the repository env and the tunnel ports
// that the guest computed.
func (h *helper) setEnv(args []string, stdin io.Reader) (struct{}, error) {
	room, _, err := roomFlag("set-env", args)
	if err != nil {
		return struct{}{}, err
	}
	var req struct {
		Env map[string]string `json:"env"`
	}
	if err := json.NewDecoder(stdin).Decode(&req); err != nil {
		return struct{}{}, fmt.Errorf("read the request: %w", err)
	}
	keys := make([]string, 0, len(req.Env))
	for k, v := range req.Env {
		if k == "" || strings.ContainsAny(k, "=\n\x00") || strings.ContainsAny(v, "\n\x00") {
			return struct{}{}, fmt.Errorf("invalid variable %q: names cannot have = or a line break; values cannot have a line break", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + req.Env[k] + "\n")
	}
	return struct{}{}, writeFileAtomic(filepath.Join(h.roomDir(room), "lease.env"), []byte(b.String()), 0o600)
}

// leaseEnv builds the environment of jobs and shells (design, section 10). A
// later source overrides an earlier one.
func (h *helper) leaseEnv(room string, warn io.Writer) ([]string, error) {
	lease, err := h.readLease(room)
	if err != nil {
		return nil, err
	}
	// 1. The machine user's environment.
	env := envMap(os.Environ())
	// 2. Provider, machine, and room env, and the MISE_* variables.
	machineEnv, err := h.readMachineEnv()
	if err != nil {
		return nil, err
	}
	mergeEnv(env, machineEnv.Env)
	mergeEnv(env, machineEnv.Rooms[room])
	env["PATH"] = filepath.Join(h.root, "bin") + string(os.PathListSeparator) + env["PATH"]
	// 3. mise env in work/: tool paths and the repository's mise [env].
	mergeEnv(env, h.miseEnv(h.workDir(room), env, warn))
	// 4. The env and path of the machine's software tools and of the
	// screen tools that started for the lease.
	screens := map[string]bool{}
	for _, st := range lease.Started {
		screens[st.Name] = true
	}
	if err := h.addToolEnv(env, screens); err != nil {
		return nil, err
	}
	// 5. tool.env from start. 6. lease.env from the guest.
	for _, f := range []string{"tool.env", "lease.env"} {
		m, err := readEnvFile(filepath.Join(h.roomDir(room), f))
		if err != nil {
			return nil, err
		}
		mergeEnv(env, m)
	}
	// 7. The lease variables.
	env["HOSTEL_LEASE"] = lease.LeaseID
	env["HOSTEL_TOOLS"] = strings.Join(lease.Tools, ",")
	env["HOSTEL_ROOM"] = room
	env["HOSTEL_WORK_DIR"] = h.workDir(room)
	env["HOSTEL_CACHE_DIR"] = filepath.Join(h.root, "cache")
	// agent-device keeps sessions and its daemon here, so each room has its own
	// and cleanup deletes them with the room. Its iOS runner build stays in
	// ~/.agent-device and is reused.
	env["AGENT_DEVICE_STATE_DIR"] = filepath.Join(h.roomDir(room), "agent-device")
	env["HOSTEL_MACHINE_OS"] = h.os
	env["HOSTEL_MACHINE_ARCH"] = runtime.GOARCH
	return envList(env), nil
}

// miseEnv returns the output of "mise env --json" in dir: the tool paths and
// the [env] of the mise configuration files that apply there.
func (h *helper) miseEnv(dir string, env map[string]string, warn io.Writer) map[string]string {
	if _, err := os.Stat(h.misePath()); err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), miseEnvTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.misePath(), "env", "--json")
	cmd.Dir = dir
	cmd.Env = envList(env)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(warn, "hostel: mise env failed (%v); tools from mise.toml may be missing: %s\n", err, strings.TrimSpace(errb.String()))
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		fmt.Fprintf(warn, "hostel: mise env printed invalid JSON: %v\n", err)
		return nil
	}
	return m
}

// miseCurrent returns the tools that the mise files of the work directory
// select, in the job environment. The machine's own mise tools have their
// source outside the work directory.
func (h *helper) miseCurrent(args []string) ([]protocol.MiseTool, error) {
	room, _, err := roomFlag("mise-current", args)
	if err != nil {
		return nil, err
	}
	out := []protocol.MiseTool{}
	if _, err := os.Stat(h.misePath()); err != nil {
		return out, nil
	}
	env, err := h.leaseEnv(room, io.Discard)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), miseEnvTimeout)
	defer cancel()
	work := h.workDir(room)
	cmd := exec.CommandContext(ctx, h.misePath(), "ls", "--current", "--json")
	cmd.Dir = work
	cmd.Env = env
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("mise ls: %w", err)
	}
	var list map[string][]struct {
		Version string `json:"version"`
		Source  struct {
			Path string `json:"path"`
		} `json:"source"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("read the output of mise ls: %w", err)
	}
	if real, err := filepath.EvalSymlinks(work); err == nil {
		work = real
	}
	for name, versions := range list {
		if len(versions) == 0 {
			continue
		}
		rel, err := filepath.Rel(work, versions[0].Source.Path)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		var all []string
		for _, v := range versions {
			all = append(all, v.Version)
		}
		out = append(out, protocol.MiseTool{Name: name, Version: strings.Join(all, "/")})
	}
	slices.SortFunc(out, func(a, b protocol.MiseTool) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// execCommand runs a guest command (from the gateway) with the lease
// environment in work/. The helper process is replaced by the command, so the
// exit code, signals, and the terminal pass through.
func (h *helper) execCommand(args []string, stderr io.Writer) error {
	fs := cli.NewFlagSet("exec", io.Discard)
	room := fs.String("room", "", "room name")
	b64 := fs.String("command-b64", "", "command, base64")
	if _, err := cli.Parse(fs, args); err != nil {
		return err
	}
	cmd, err := base64.StdEncoding.DecodeString(*b64)
	if err != nil || !config.ValidName(*room) {
		return fmt.Errorf("%w: exec --room <room> --command-b64 <base64>", cli.ErrUsage)
	}
	env, err := h.leaseEnv(*room, stderr)
	if err != nil {
		return err
	}
	if err := h.recordSession(*room); err != nil {
		return err
	}
	h.trackSessions(*room, envMap(env)["HOSTEL_LEASE"], stderr)
	if err := os.Chdir(h.workDir(*room)); err != nil {
		return err
	}
	// On Windows, a guest command runs in a job object that it cannot break
	// away from; the job supervisor must break away, so the helper runs its
	// own "job start" itself, outside that job.
	if runtime.GOOS == "windows" {
		if start, err := shell.Command(shell.PowerShell, h.exe, "helper", "job", "start", "--room", *room); err == nil && strings.EqualFold(string(cmd), start) {
			os.Exit(Main([]string{"job", "start", "--room", *room}, os.Stdin, os.Stdout, os.Stderr))
		}
	}
	if err := confine(); err != nil {
		return err
	}
	path, argv := shellCommand(string(cmd), env)
	return execProcess(path, argv, env)
}

// shell runs the machine user's shell with the lease environment in work/.
func (h *helper) shell(args []string, stderr io.Writer) error {
	room, _, err := roomFlag("shell", args)
	if err != nil {
		return err
	}
	env, err := h.leaseEnv(room, stderr)
	if err != nil {
		return err
	}
	if err := h.recordSession(room); err != nil {
		return err
	}
	h.trackSessions(room, envMap(env)["HOSTEL_LEASE"], stderr)
	if err := os.Chdir(h.workDir(room)); err != nil {
		return err
	}
	if err := confine(); err != nil {
		return err
	}
	path, argv := interactiveShell(env)
	return execProcess(path, argv, env)
}
