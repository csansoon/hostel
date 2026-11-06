package room

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

func (h *helper) workDir(slot string) string { return filepath.Join(h.slotDir(slot), "work") }

func slotFlag(name string, args []string) (string, []string, error) {
	fs := cli.NewFlagSet(name, io.Discard)
	slot := fs.String("slot", "", "slot name")
	pos, err := cli.Parse(fs, args)
	if err != nil {
		return "", nil, err
	}
	if !config.ValidName(*slot) {
		return "", nil, fmt.Errorf("%w: %s needs --slot <slot>", cli.ErrUsage, name)
	}
	return *slot, pos, nil
}

// begin writes the lease data of a slot and saves authorized_keys as the base
// that cleanup restores (a guest must not keep a key it added).
func (h *helper) begin(args []string, stdin io.Reader) (struct{}, error) {
	slot, _, err := slotFlag("begin", args)
	if err != nil {
		return struct{}{}, err
	}
	var req protocol.BeginRequest
	if err := json.NewDecoder(stdin).Decode(&req); err != nil {
		return struct{}{}, fmt.Errorf("read the request: %w", err)
	}
	if err := os.MkdirAll(h.workDir(slot), 0o755); err != nil {
		return struct{}{}, err
	}
	data, err := json.Marshal(req)
	if err != nil {
		return struct{}{}, err
	}
	if err := writeFileAtomic(filepath.Join(h.slotDir(slot), "lease.json"), data, 0o644); err != nil {
		return struct{}{}, err
	}
	// The base comes from a moment when the room has no other lease: with a
	// lease in another slot, authorized_keys can have a key that that lease
	// added, and the base must not keep it.
	if busy, err := h.otherSlotBusy(slot); err != nil || busy {
		return struct{}{}, err
	}
	keys, err := os.ReadFile(h.authorizedKeysPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return struct{}{}, err
	}
	return struct{}{}, writeFileAtomic(h.authorizedKeysBase(), keys, 0o600)
}

// otherSlotBusy reports whether another slot of the room has a lease.
func (h *helper) otherSlotBusy(slot string) (bool, error) {
	des, err := os.ReadDir(filepath.Join(h.root, "slots"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	for _, de := range des {
		if de.Name() == slot || !de.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(h.slotDir(de.Name()), "lease.json")); err == nil {
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

func (h *helper) readLease(slot string) (protocol.BeginRequest, error) {
	var l protocol.BeginRequest
	data, err := os.ReadFile(filepath.Join(h.slotDir(slot), "lease.json"))
	if errors.Is(err, os.ErrNotExist) {
		return l, fmt.Errorf("slot %s has no lease", slot)
	}
	if err != nil {
		return l, err
	}
	return l, json.Unmarshal(data, &l)
}

// setEnv writes lease.env: the repository env and the tunnel ports
// that the guest computed.
func (h *helper) setEnv(args []string, stdin io.Reader) (struct{}, error) {
	slot, _, err := slotFlag("set-env", args)
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
	return struct{}{}, writeFileAtomic(filepath.Join(h.slotDir(slot), "lease.env"), []byte(b.String()), 0o600)
}

// leaseEnv builds the environment of jobs and shells (design, section 10). A
// later source overrides an earlier one.
func (h *helper) leaseEnv(slot string, warn io.Writer) ([]string, error) {
	lease, err := h.readLease(slot)
	if err != nil {
		return nil, err
	}
	// 1. The room user's environment.
	env := envMap(os.Environ())
	// 2. Provider, machine, and slot env, and the MISE_* variables.
	roomEnv, err := h.readRoomEnv()
	if err != nil {
		return nil, err
	}
	mergeEnv(env, roomEnv.Env)
	mergeEnv(env, roomEnv.Slots[slot])
	env["PATH"] = filepath.Join(h.root, "bin") + string(os.PathListSeparator) + env["PATH"]
	// 3. mise env in work/: tool paths and the repository's mise [env].
	mergeEnv(env, h.miseEnv(h.workDir(slot), env, warn))
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
		m, err := readEnvFile(filepath.Join(h.slotDir(slot), f))
		if err != nil {
			return nil, err
		}
		mergeEnv(env, m)
	}
	// 7. The lease variables.
	env["HOSTEL_LEASE"] = lease.LeaseID
	env["HOSTEL_TOOLS"] = strings.Join(lease.Tools, ",")
	env["HOSTEL_SLOT"] = slot
	env["HOSTEL_WORK_DIR"] = h.workDir(slot)
	env["HOSTEL_CACHE_DIR"] = filepath.Join(h.root, "cache")
	// agent-device keeps sessions and its daemon here, so each slot has its own
	// and cleanup deletes them with the slot. Its iOS runner build stays in
	// ~/.agent-device and is reused.
	env["AGENT_DEVICE_STATE_DIR"] = filepath.Join(h.slotDir(slot), "agent-device")
	env["HOSTEL_ROOM_OS"] = h.os
	env["HOSTEL_ROOM_ARCH"] = runtime.GOARCH
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
	slot, _, err := slotFlag("mise-current", args)
	if err != nil {
		return nil, err
	}
	out := []protocol.MiseTool{}
	if _, err := os.Stat(h.misePath()); err != nil {
		return out, nil
	}
	env, err := h.leaseEnv(slot, io.Discard)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), miseEnvTimeout)
	defer cancel()
	work := h.workDir(slot)
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
	slot := fs.String("slot", "", "slot name")
	b64 := fs.String("command-b64", "", "command, base64")
	if _, err := cli.Parse(fs, args); err != nil {
		return err
	}
	cmd, err := base64.StdEncoding.DecodeString(*b64)
	if err != nil || !config.ValidName(*slot) {
		return fmt.Errorf("%w: exec --slot <slot> --command-b64 <base64>", cli.ErrUsage)
	}
	env, err := h.leaseEnv(*slot, stderr)
	if err != nil {
		return err
	}
	if err := h.recordSession(*slot); err != nil {
		return err
	}
	h.trackSessions(*slot, envMap(env)["HOSTEL_LEASE"], stderr)
	if err := os.Chdir(h.workDir(*slot)); err != nil {
		return err
	}
	// On Windows, a guest command runs in a job object that it cannot break
	// away from; the job supervisor must break away, so the helper runs its
	// own "job start" itself, outside that job.
	if runtime.GOOS == "windows" {
		if start, err := shell.Command(shell.PowerShell, h.exe, "room", "job", "start", "--slot", *slot); err == nil && strings.EqualFold(string(cmd), start) {
			os.Exit(Main([]string{"job", "start", "--slot", *slot}, os.Stdin, os.Stdout, os.Stderr))
		}
	}
	if err := confine(); err != nil {
		return err
	}
	path, argv := shellCommand(string(cmd), env)
	return execProcess(path, argv, env)
}

// shell runs the room user's shell with the lease environment in work/.
func (h *helper) shell(args []string, stderr io.Writer) error {
	slot, _, err := slotFlag("shell", args)
	if err != nil {
		return err
	}
	env, err := h.leaseEnv(slot, stderr)
	if err != nil {
		return err
	}
	if err := h.recordSession(slot); err != nil {
		return err
	}
	h.trackSessions(slot, envMap(env)["HOSTEL_LEASE"], stderr)
	if err := os.Chdir(h.workDir(slot)); err != nil {
		return err
	}
	if err := confine(); err != nil {
		return err
	}
	path, argv := interactiveShell(env)
	return execProcess(path, argv, env)
}
