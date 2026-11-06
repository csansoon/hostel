package room

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
)

// outputTailSize is the size of the script output tail that goes to the event log.
const outputTailSize = 4096

// runScript runs a tool script: run-script <tool> <step> [--slot s].
func (h *helper) runScript(args []string, stdin io.Reader) (protocol.RunScriptResponse, error) {
	var resp protocol.RunScriptResponse
	fs := cli.NewFlagSet("hostel room run-script", io.Discard)
	slot := fs.String("slot", "", "slot name")
	pos, err := cli.Parse(fs, args)
	if err != nil {
		return resp, err
	}
	if len(pos) != 2 || !config.ValidName(pos[0]) || (*slot != "" && !config.ValidName(*slot)) {
		return resp, fmt.Errorf("%w: run-script <tool> <step> [--slot <slot>]", cli.ErrUsage)
	}
	tool, step := pos[0], pos[1]
	var req protocol.RunScriptRequest
	if err := json.NewDecoder(stdin).Decode(&req); err != nil {
		return resp, fmt.Errorf("read the request: %w", err)
	}

	return h.script(tool, step, *slot, req.Args, req.Env, time.Duration(req.TimeoutSeconds)*time.Second)
}

// script runs a tool script step. A missing script is not an error.
func (h *helper) script(tool, step, slot string, args []string, reqEnv map[string]string, timeout time.Duration) (protocol.RunScriptResponse, error) {
	var resp protocol.RunScriptResponse
	script := filepath.Join(h.toolDir(tool), step+"."+h.os+config.ScriptExt(h.os))
	if _, err := os.Stat(script); errors.Is(err, os.ErrNotExist) {
		resp.Missing = true
		return resp, nil
	}
	dir := h.root
	if step != config.StepInstall && step != config.StepCheck {
		if slot == "" {
			return resp, fmt.Errorf("%w: step %s needs --slot", cli.ErrUsage, step)
		}
		dir = h.slotDir(slot)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return resp, err
		}
	}
	env, err := h.scriptEnv(tool, slot, step, reqEnv)
	if err != nil {
		return resp, err
	}
	// Each start script writes its own facts to tool.env; the helper keeps
	// the facts of the screen tools that started before it.
	var earlier map[string]string
	toolEnv := filepath.Join(dir, "tool.env")
	if step == config.StepStart {
		if earlier, err = readEnvFile(toolEnv); err != nil {
			return resp, err
		}
		if err := os.Remove(toolEnv); err != nil && !errors.Is(err, os.ErrNotExist) {
			return resp, err
		}
	}
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	path, argv := scriptCommand(script, args, env)
	cmd := exec.CommandContext(ctx, path)
	cmd.Args = argv
	cmd.Dir = dir
	cmd.Env = env
	var out tailBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	// The script and all processes of its group stop at the deadline.
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 10 * time.Second
	if err = startInGroup(cmd, ""); err == nil {
		err = cmd.Wait()
	}
	resp.OutputTail = out.String()
	resp.LastLine = lastLine(resp.OutputTail)
	var exitErr *exec.ExitError
	var runErr error
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
		runErr = fmt.Errorf("run %s: %w", script, err)
	}
	// Also when the script did not run: the stop of the earlier tools needs
	// their facts.
	if step == config.StepStart {
		facts, err := readEnvFile(toolEnv)
		if err != nil {
			return resp, errors.Join(runErr, err)
		}
		resp.Facts = sortedNames(facts)
		mergeEnv(earlier, facts)
		if err := writeEnvFile(toolEnv, earlier); err != nil {
			return resp, errors.Join(runErr, err)
		}
	}
	return resp, runErr
}

func sortedNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func writeEnvFile(path string, env map[string]string) error {
	var b strings.Builder
	for _, k := range sortedNames(env) {
		b.WriteString(k + "=" + env[k] + "\n")
	}
	return writeFileAtomic(path, []byte(b.String()), 0o644)
}

// lastLine returns the last line of out that is not empty, without spaces at
// its ends.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// scriptEnv builds the environment of a tool script: the room user's
// environment, room-env.json (machine env, MISE_*, slot env), the tool
// variables, the request env from the server, the paths, the env and path of
// the machine's software tools and of the tool itself, and tool.env for
// screenshot and stop.
func (h *helper) scriptEnv(tool, slot, step string, reqEnv map[string]string) ([]string, error) {
	env := envMap(os.Environ())
	roomEnv, err := h.readRoomEnv()
	if err != nil {
		return nil, err
	}
	mergeEnv(env, roomEnv.Env)
	if tool != "" {
		env["HOSTEL_TOOL"] = tool
		if t, err := h.toolInfo(tool); err == nil {
			options := t.Options
			if o, ok := roomEnv.ToolOptions[tool]; ok {
				options = o
			}
			env["HOSTEL_TOOL_ALL_OPTIONS"] = strings.Join(options, ",")
		}
	}
	if slot != "" {
		mergeEnv(env, roomEnv.Slots[slot])
		// A script that a guest runs (screenshot) gets the same lease values as
		// a script that the server runs.
		if l, err := h.readLease(slot); err == nil {
			env["HOSTEL_LEASE_ID"] = l.LeaseID
			env["HOSTEL_TOOLS"] = strings.Join(l.Tools, ",")
			env["HOSTEL_EXCLUSIVE"] = exclusiveEnv(l.Exclusive)
			for _, st := range l.Started {
				if st.Name == tool {
					env["HOSTEL_TOOL_OPTIONS"] = strings.Join(st.Options, ",")
				}
			}
		}
	}
	mergeEnv(env, reqEnv)
	// The machine-level mise tools are on the PATH of scripts.
	env["PATH"] = filepath.Join(h.root, "bin") + string(os.PathListSeparator) + env["PATH"]
	mergeEnv(env, h.miseEnv(h.root, env, io.Discard))
	if err := h.addToolEnv(env, map[string]bool{tool: true}); err != nil {
		return nil, err
	}
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
	if step == config.StepScreenshot || step == config.StepStop {
		facts, err := readEnvFile(filepath.Join(h.slotDir(slot), "tool.env"))
		if err != nil {
			return nil, err
		}
		mergeEnv(env, facts)
	}
	return envList(env), nil
}

// addToolEnv adds the env and path of the tools in hostel/tools (design,
// section 8): of each software tool, and of the screen tools in screens.
// {cache} is HOSTEL_CACHE_DIR, {home} the room user's home directory. The
// tools are in name order; each tool's path entries go first on PATH in
// their order.
func (h *helper) addToolEnv(env map[string]string, screens map[string]bool) error {
	des, err := os.ReadDir(h.toolsDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	home := filepath.Dir(h.root)
	expand := func(v string) string {
		v = strings.ReplaceAll(v, "{cache}", filepath.Join(h.root, "cache"))
		return filepath.FromSlash(strings.ReplaceAll(v, "{home}", home))
	}
	var paths []string
	for _, de := range des {
		if !de.IsDir() || !config.ValidName(de.Name()) {
			continue
		}
		t, err := h.toolInfo(de.Name())
		if err != nil {
			return fmt.Errorf("tool %s: %w", de.Name(), err)
		}
		if t.Screen && !screens[t.Name] {
			continue
		}
		for _, k := range sortedNames(t.Env) {
			env[envKey(k)] = expand(t.Env[k])
		}
		for _, p := range t.Path {
			paths = append(paths, expand(p))
		}
	}
	if len(paths) > 0 {
		env[envKey("PATH")] = strings.Join(append(paths, env[envKey("PATH")]), string(os.PathListSeparator))
	}
	return nil
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

// exclusiveEnv is HOSTEL_EXCLUSIVE: "1" for a lease that holds the whole room
// (only then may a script change the desktop), else empty.
func exclusiveEnv(exclusive bool) string {
	if exclusive {
		return "1"
	}
	return ""
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
			m[envKey(k)] = v
		}
	}
	return m
}

// mergeEnv copies src into env; a later source overrides an earlier one.
func mergeEnv(env, src map[string]string) {
	for k, v := range src {
		env[envKey(k)] = v
	}
}

// envKey is the map key of a variable name. Windows ignores the case of
// names ("Path" is PATH): there, the keys are upper case, so a later source
// overrides an earlier one with another case.
func envKey(k string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(k)
	}
	return k
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

// miseInstall installs the machine-level tools of hostel/mise/config.toml and,
// with agent_device, agent-device and its Node (server only): mise-install, with
// a MiseInstallRequest on stdin.
func (h *helper) miseInstall(stdin io.Reader) (protocol.MiseInstallResponse, error) {
	var resp protocol.MiseInstallResponse
	var req protocol.MiseInstallRequest
	if err := json.NewDecoder(stdin).Decode(&req); err != nil {
		return resp, fmt.Errorf("read the request: %w", err)
	}
	env, err := h.scriptEnv("", "", config.StepInstall, nil)
	if err != nil {
		return resp, err
	}
	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 20 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	node := "node@" + config.AgentDeviceNode
	steps := [][]string{{"install"}}
	if req.AgentDevice {
		// The npm backend needs npm on the PATH, so Node comes first.
		steps = append(steps, []string{"install", node},
			[]string{"exec", node, "--", h.misePath(), "install", "npm:agent-device@" + config.AgentDeviceVersion})
	}
	var out tailBuffer
	for _, args := range steps {
		cmd := exec.CommandContext(ctx, h.misePath(), args...)
		cmd.Dir, cmd.Env = h.root, env
		cmd.Stdout, cmd.Stderr = &out, &out
		err = cmd.Run()
		resp.OutputTail = out.String()
		var exitErr *exec.ExitError
		switch {
		case ctx.Err() != nil:
			resp.TimedOut, resp.ExitCode = true, -1
			return resp, nil
		case errors.As(err, &exitErr):
			resp.ExitCode = exitErr.ExitCode()
			return resp, nil
		case err != nil:
			return resp, fmt.Errorf("run mise %s: %w", args[0], err)
		}
	}
	if err := h.writeDesktopCommand(); err != nil {
		return resp, err
	}
	if runtime.GOOS == "windows" && req.AgentDevice {
		if err := h.installWinApp(ctx); err != nil {
			return resp, err
		}
	}
	if resp.Versions, err = h.miseVersions(ctx, env); err != nil {
		return resp, err
	}
	return resp, h.writeAgentDevice(req.AgentDevice, node)
}

// miseVersions returns the installed versions of the machine-level mise
// tools: tool name as in the configuration → versions, comma-separated.
func (h *helper) miseVersions(ctx context.Context, env []string) (map[string]string, error) {
	cmd := exec.CommandContext(ctx, h.misePath(), "ls", "--current", "--json")
	cmd.Dir, cmd.Env = h.root, env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("list the mise tools: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	var list map[string][]struct {
		Version   string `json:"version"`
		Installed bool   `json:"installed"`
	}
	if err := json.Unmarshal(out.Bytes(), &list); err != nil {
		return nil, fmt.Errorf("list the mise tools: %w", err)
	}
	versions := map[string]string{}
	for name, entries := range list {
		// The configuration names the JDK "jdk"; mise names it "java".
		if name == "java" {
			name = "jdk"
		}
		var vs []string
		for _, e := range entries {
			if e.Installed && e.Version != "" {
				vs = append(vs, e.Version)
			}
		}
		versions[name] = strings.Join(vs, ",")
	}
	return versions, nil
}

// writeAgentDevice writes hostel/bin/agent-device, which runs the pinned
// agent-device with its pinned Node, or removes it.
func (h *helper) writeAgentDevice(enabled bool, node string) error {
	path := filepath.Join(h.root, "bin", "agent-device")
	mise := "'" + strings.ReplaceAll(h.misePath(), "'", `'\''`) + "'"
	content := fmt.Sprintf("#!/bin/sh\nexec %s exec %s npm:agent-device@%s -- agent-device \"$@\"\n",
		mise, node, config.AgentDeviceVersion)
	if runtime.GOOS == "windows" {
		path += ".cmd"
		content = fmt.Sprintf("@\"%s\" exec %s npm:agent-device@%s -- agent-device %%*\r\n",
			h.misePath(), node, config.AgentDeviceVersion)
	}
	if !enabled {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove the agent-device command: %w", err)
		}
		return nil
	}
	if err := writeFileAtomic(path, []byte(content), 0o755); err != nil {
		return fmt.Errorf("write the agent-device command: %w", err)
	}
	return nil
}
