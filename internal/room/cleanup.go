package room

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
)

// stopWait is the time between SIGTERM and SIGKILL in cleanup.
const stopWait = 10 * time.Second

// toolInfo reads the options and the script timeouts of an installed tool
// directory.
func (h *helper) toolInfo(name string) (config.Tool, error) {
	t := config.Tool{Name: name}
	data, err := os.ReadFile(filepath.Join(h.toolDir(name), config.ToolFile))
	if err != nil {
		return t, err
	}
	err = toml.NewDecoder(bytes.NewReader(data)).Decode(&t)
	return t, err
}

// cleanup deletes everything a lease left (design, section 17). Each step runs
// even if an earlier step failed.
func (h *helper) cleanup(args []string, stdin io.Reader) (protocol.CleanupResponse, error) {
	var resp protocol.CleanupResponse
	slot, _, err := slotFlag("cleanup", args)
	if err != nil {
		return resp, err
	}
	var req protocol.CleanupRequest
	if err := json.NewDecoder(stdin).Decode(&req); err != nil {
		return resp, fmt.Errorf("read the request: %w", err)
	}
	if (req.Tool != "" && !config.ValidName(req.Tool)) || req.LeaseID == "" {
		return resp, errors.New("cleanup needs lease_id and a valid tool name")
	}
	step := func(name string, fn func() error) {
		st := protocol.CleanupStep{Name: name}
		if err := fn(); err != nil {
			st.Error = err.Error()
		}
		resp.Steps = append(resp.Steps, st)
	}
	// The desktop program ends the lease's desktop commands at this marker,
	// also those that cleared HOSTEL_LEASE.
	step("end desktop commands", func() error {
		if _, err := os.Stat(h.slotDir(slot)); err != nil {
			return nil
		}
		return os.WriteFile(h.leaseEndedFile(slot), nil, 0o644)
	})
	step("stop processes", func() error { return h.stopLeaseProcesses(slot, req.LeaseID) })
	step("stop screen tool", func() error { return h.stopTool(slot, req) })
	step("restore authorized_keys", h.restoreAuthorizedKeys)
	step("check", func() error { return h.checkClean(slot, req.LeaseID, req.Ports) })
	step("delete slot", func() error {
		// Markers of jobs whose supervisor was killed.
		if marker := devboxTaskMarker(slot, "*"); marker != "" {
			left, _ := filepath.Glob(marker)
			for _, f := range left {
				if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("remove a devbox task marker: %w", err)
				}
			}
		}
		return os.RemoveAll(h.slotDir(slot))
	})
	step("check tools", func() error { return h.check(req.Check) })
	return resp, nil
}

// stopLeaseProcesses stops the job process groups and all processes of the
// lease (leaseProcesses): SIGTERM, wait, then SIGKILL.
func (h *helper) stopLeaseProcesses(slot, leaseID string) error {
	var groups []supervisorInfo
	des, _ := os.ReadDir(h.jobsDir(slot))
	for _, de := range des {
		if sup, err := h.readSupervisor(filepath.Join(h.jobsDir(slot), de.Name())); err == nil && alive(sup.PGID, sup.CommandStart) {
			groups = append(groups, sup)
		}
	}
	pids, err := h.leaseProcesses(slot, leaseID)
	if err != nil {
		return err
	}
	if len(groups) == 0 && len(pids) == 0 {
		return nil
	}
	for _, g := range groups {
		stopGroup(g.PGID, g.Group, false)
	}
	for _, p := range pids {
		signalProcess(p, false)
	}
	deadline := time.Now().Add(stopWait)
	for time.Now().Before(deadline) {
		if left, err := h.leaseProcesses(slot, leaseID); err == nil && len(left) == 0 {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, g := range groups {
		stopGroup(g.PGID, g.Group, true)
	}
	left, err := h.leaseProcesses(slot, leaseID)
	if err != nil {
		return err
	}
	for _, p := range left {
		signalProcess(p, true)
	}
	return nil
}

// stopTool runs the stop script of the screen tool that started for the
// lease.
func (h *helper) stopTool(slot string, req protocol.CleanupRequest) error {
	if req.Tool == "" {
		return nil
	}
	// A repeated cleanup after "delete slot" finds no slot directory; stop
	// then runs in an empty one and must do nothing.
	if err := os.MkdirAll(h.slotDir(slot), 0o755); err != nil {
		return err
	}
	t, err := h.toolInfo(req.Tool)
	if err != nil {
		return err
	}
	resp, err := h.script(req.Tool, config.StepStop, slot, nil, map[string]string{
		"HOSTEL_LEASE_ID":     req.LeaseID,
		"HOSTEL_TOOL_OPTIONS": strings.Join(req.Options, ","),
		"HOSTEL_TOOLS":        toolsEnv(req.Tool, req.Options),
		"HOSTEL_EXCLUSIVE":    exclusiveEnv(req.Exclusive),
	}, t.ScriptTimeout(config.StepStop))
	if err != nil {
		return err
	}
	return scriptError(resp)
}

func scriptError(resp protocol.RunScriptResponse) error {
	tail := strings.TrimSpace(resp.OutputTail)
	if len(tail) > 300 {
		tail = "…" + tail[len(tail)-300:]
	}
	switch {
	case resp.TimedOut:
		return fmt.Errorf("timed out: %s", tail)
	case resp.ExitCode != 0:
		return fmt.Errorf("exit code %d: %s", resp.ExitCode, tail)
	}
	return nil
}

func (h *helper) restoreAuthorizedKeys() error {
	base, err := os.ReadFile(h.authorizedKeysBase())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	path := h.authorizedKeysPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(path, base, 0o600)
}

// checkClean checks that no lease process remains and that the ports are free.
// The room's SSH server removes its listeners after the connections close, so
// the port check retries for up to 10 seconds.
func (h *helper) checkClean(slot, leaseID string, ports []int) error {
	left, err := h.leaseProcesses(slot, leaseID)
	if err != nil {
		return err
	}
	if len(left) > 0 {
		sort.Ints(left)
		return fmt.Errorf("processes of the lease still run: %v", left)
	}
	addrs := []string{"127.0.0.1"}
	if ln, err := net.Listen("tcp", "[::1]:0"); err == nil {
		ln.Close()
		addrs = append(addrs, "::1")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var busy []string
		for _, port := range ports {
			for _, a := range addrs {
				ln, err := net.Listen("tcp", net.JoinHostPort(a, strconv.Itoa(port)))
				if err != nil {
					busy = append(busy, net.JoinHostPort(a, strconv.Itoa(port)))
					continue
				}
				ln.Close()
			}
		}
		if len(busy) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ports still in use: %s", strings.Join(busy, ", "))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// check runs the check script of each tool in names.
func (h *helper) check(names []string) error {
	var failed []string
	for _, name := range names {
		if !config.ValidName(name) {
			failed = append(failed, fmt.Sprintf("invalid tool name %q", name))
			continue
		}
		t, err := h.toolInfo(name)
		if err != nil {
			failed = append(failed, name+": "+err.Error())
			continue
		}
		resp, err := h.script(name, config.StepCheck, "", nil, nil, t.ScriptTimeout(config.StepCheck))
		if err == nil {
			err = scriptError(resp)
		}
		if err != nil {
			failed = append(failed, name+": "+err.Error())
		}
	}
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, "; "))
	}
	return nil
}
