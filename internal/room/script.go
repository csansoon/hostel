package room

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
)

// outputTailSize is the size of the script output tail that goes to the event log.
const outputTailSize = 4096

// runScript runs a platform script: run-script <platform> <step> [--slot s].
func (h *helper) runScript(args []string, stdin io.Reader) (protocol.RunScriptResponse, error) {
	var resp protocol.RunScriptResponse
	fs := cli.NewFlagSet("hostel room run-script", io.Discard)
	slot := fs.String("slot", "", "slot name")
	pos, err := cli.Parse(fs, args)
	if err != nil {
		return resp, err
	}
	if len(pos) != 2 || !config.ValidName(pos[0]) || (*slot != "" && !config.ValidName(*slot)) {
		return resp, fmt.Errorf("%w: run-script <platform> <step> [--slot <slot>]", cli.ErrUsage)
	}
	if h.os == config.OSWindows {
		return resp, errWindows
	}
	platform, step := pos[0], pos[1]
	var req protocol.RunScriptRequest
	if err := json.NewDecoder(stdin).Decode(&req); err != nil {
		return resp, fmt.Errorf("read the request: %w", err)
	}

	return h.script(platform, step, *slot, req.Args, req.Env, time.Duration(req.TimeoutSeconds)*time.Second)
}

// script runs a platform script step. A missing script is not an error.
func (h *helper) script(platform, step, slot string, args []string, reqEnv map[string]string, timeout time.Duration) (protocol.RunScriptResponse, error) {
	var resp protocol.RunScriptResponse
	script := filepath.Join(h.platformDir(platform), step+"."+h.os+config.ScriptExt(h.os))
	if _, err := os.Stat(script); errors.Is(err, os.ErrNotExist) {
		resp.Missing = true
		return resp, nil
	}
	dir := h.root
	if step != config.StepProvision && step != config.StepHealth {
		if slot == "" {
			return resp, fmt.Errorf("%w: step %s needs --slot", cli.ErrUsage, step)
		}
		dir = h.slotDir(slot)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return resp, err
		}
	}
	env, err := h.scriptEnv(slot, step, reqEnv)
	if err != nil {
		return resp, err
	}
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", append([]string{script}, args...)...)
	cmd.Dir = dir
	cmd.Env = env
	var out tailBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	setProcessGroup(cmd)
	// The script and all processes of its group stop at the deadline.
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 10 * time.Second
	err = cmd.Run()
	resp.OutputTail = out.String()
	var exitErr *exec.ExitError
	switch {
	case ctx.Err() != nil:
		resp.TimedOut = true
		resp.ExitCode = -1
	case errors.As(err, &exitErr):
		resp.ExitCode = exitErr.ExitCode()
	case errors.Is(err, exec.ErrWaitDelay):
		// The script ended, but a process that it started in the background
		// (for example a simulator) still holds its output open.
		resp.ExitCode = cmd.ProcessState.ExitCode()
	case err != nil:
		return resp, fmt.Errorf("run %s: %w", script, err)
	}
	return resp, nil
}

// scriptEnv builds the environment of a platform script: the room user's
// environment, room-env.json (machine env, MISE_*, slot env), the request env
// from the server, the paths, and device.env for screenshot and reset.
func (h *helper) scriptEnv(slot, step string, reqEnv map[string]string) ([]string, error) {
	env := envMap(os.Environ())
	roomEnv, err := h.readRoomEnv()
	if err != nil {
		return nil, err
	}
	maps.Copy(env, roomEnv.Env)
	if slot != "" {
		maps.Copy(env, roomEnv.Slots[slot])
		// A script that a guest runs (screenshot) gets the same lease values as
		// a script that the server runs.
		if l, err := h.readLease(slot); err == nil {
			env["HOSTEL_LEASE_ID"] = l.LeaseID
			env["HOSTEL_PLATFORM"] = l.Platform
			env["HOSTEL_DEVICES"] = strings.Join(l.Devices, ",")
			if p, err := h.platformInfo(l.Platform); err == nil {
				env["HOSTEL_PLATFORM_DEVICES"] = strings.Join(p.Devices, ",")
			}
		}
	}
	maps.Copy(env, reqEnv)
	cache := filepath.Join(h.root, "cache")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return nil, err
	}
	env["HOSTEL_CACHE_DIR"] = cache
	if slot != "" {
		env["HOSTEL_SLOT"] = slot
		env["HOSTEL_SLOT_DIR"] = h.slotDir(slot)
		env["HOSTEL_WORK_DIR"] = filepath.Join(h.slotDir(slot), "work")
	}
	if step == config.StepScreenshot || step == config.StepReset {
		dev, err := readEnvFile(filepath.Join(h.slotDir(slot), "device.env"))
		if err != nil {
			return nil, err
		}
		maps.Copy(env, dev)
	}
	return envList(env), nil
}

func (h *helper) readRoomEnv() (protocol.RoomEnv, error) {
	var re protocol.RoomEnv
	data, err := os.ReadFile(h.roomEnvPath())
	if errors.Is(err, os.ErrNotExist) {
		return re, nil
	}
	if err != nil {
		return re, err
	}
	if err := json.Unmarshal(data, &re); err != nil {
		return re, fmt.Errorf("read room-env.json: %w", err)
	}
	return re, nil
}

// readEnvFile reads KEY=VALUE lines. A missing file is empty.
func readEnvFile(path string) (map[string]string, error) {
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s: line %q is not KEY=VALUE", filepath.Base(path), line)
		}
		out[k] = v
	}
	return out, sc.Err()
}

func envMap(list []string) map[string]string {
	m := make(map[string]string, len(list))
	for _, kv := range list {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

func envList(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// tailBuffer keeps the last outputTailSize bytes written to it.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > outputTailSize {
		t.b = append(t.b[:0], t.b[len(t.b)-outputTailSize:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }
