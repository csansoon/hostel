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

// platformInfo reads the devices and the script timeouts of an installed
// platform directory. An OS platform can have no platform.toml.
func (h *helper) platformInfo(name string) (config.Platform, error) {
	p := config.Platform{Name: name}
	data, err := os.ReadFile(filepath.Join(h.platformDir(name), "platform.toml"))
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	err = toml.NewDecoder(bytes.NewReader(data)).Decode(&p)
	return p, err
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
	if !config.ValidName(req.Platform) || req.LeaseID == "" {
		return resp, errors.New("cleanup needs lease_id and platform")
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
	step("reset", func() error { return h.resetSlot(slot, req) })
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
	step("health", func() error { return h.health(req.Health) })
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

func (h *helper) resetSlot(slot string, req protocol.CleanupRequest) error {
	// A repeated cleanup after "delete slot" finds no slot directory; reset
	// then runs in an empty one and must do nothing.
	if err := os.MkdirAll(h.slotDir(slot), 0o755); err != nil {
		return err
	}
	p, err := h.platformInfo(req.Platform)
	if err != nil {
		return err
	}
	resp, err := h.script(req.Platform, config.StepReset, slot, nil, map[string]string{
		"HOSTEL_PLATFORM":         req.Platform,
		"HOSTEL_PLATFORM_DEVICES": strings.Join(p.Devices, ","),
		"HOSTEL_LEASE_ID":         req.LeaseID,
		"HOSTEL_DEVICES":          strings.Join(req.Devices, ","),
		"HOSTEL_EXCLUSIVE":        exclusiveEnv(req.Exclusive),
	}, p.ScriptTimeout(config.StepReset))
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

// health runs the health script of each platform in names.
func (h *helper) health(names []string) error {
	var failed []string
	for _, name := range names {
		if !config.ValidName(name) {
			failed = append(failed, fmt.Sprintf("invalid platform name %q", name))
			continue
		}
		p, err := h.platformInfo(name)
		if err != nil {
			failed = append(failed, name+": "+err.Error())
			continue
		}
		resp, err := h.script(name, config.StepHealth, "", nil, map[string]string{
			"HOSTEL_PLATFORM": name, "HOSTEL_PLATFORM_DEVICES": strings.Join(p.Devices, ","),
		}, p.ScriptTimeout(config.StepHealth))
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
