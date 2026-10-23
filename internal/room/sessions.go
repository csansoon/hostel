package room

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Cleanup finds lease processes by HOSTEL_LEASE in their environment and by
// their session. A process started with "&" or nohup stays in the session of the
// command that started it, also when its environment is hidden (macOS) or
// changed. Each guest command and each job supervisor has its own session; the
// helper records the guest command sessions in slots/<slot>/sessions as
// "<sid> <start time of the session leader>". A process that starts a new
// session (for example the iOS runner of agent-device) is found as a
// descendant while its parent runs; its session is then recorded too, so later
// scans find it after its parent ended.

func (h *helper) recordSession(slot string) error {
	sid, err := currentSession()
	if err != nil {
		return nil // no sessions on this OS
	}
	return h.addSession(slot, sid)
}

// addSession records a session. When its leader has ended, the start time is
// empty: a later process with that PID is a different leader.
func (h *helper) addSession(slot string, sid int) error {
	start, _ := processStart(sid)
	f, err := os.OpenFile(filepath.Join(h.slotDir(slot), "sessions"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("record the session: %w", err)
	}
	_, err = fmt.Fprintf(f, "%d %s\n", sid, start)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// leaseSessions returns the sessions of the slot's guest commands and job
// supervisors. A session ID is not reused while a process is in the session.
// When the leader still runs, its start time must match, because the number
// can belong to a new session after all processes of the old one ended.
func (h *helper) leaseSessions(slot string) map[int]bool {
	sids := map[int]bool{}
	add := func(sid int, start string) {
		if cur, err := processStart(sid); err == nil && cur != start {
			return
		}
		sids[sid] = true
	}
	if f, err := os.Open(filepath.Join(h.slotDir(slot), "sessions")); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			sidText, start, ok := strings.Cut(sc.Text(), " ")
			if sid, err := strconv.Atoi(sidText); ok && err == nil && sid > 1 {
				add(sid, start)
			}
		}
		f.Close()
	}
	des, _ := os.ReadDir(h.jobsDir(slot))
	for _, de := range des {
		if sup, err := h.readSupervisor(filepath.Join(h.jobsDir(slot), de.Name())); err == nil && sup.SupervisorPID > 1 {
			add(sup.SupervisorPID, sup.SupervisorStart)
		}
	}
	return sids
}

// leaseProcesses returns the room user's processes of the lease.
func (h *helper) leaseProcesses(slot, leaseID string) ([]int, error) {
	pids, err := envProcesses(leaseID)
	if err != nil {
		return nil, err
	}
	sids := h.leaseSessions(slot)
	more, err := sessionProcesses(sids)
	if err != nil {
		return nil, err
	}
	for _, p := range more {
		if !slices.Contains(pids, p) {
			pids = append(pids, p)
		}
	}
	below, err := descendants(pids)
	if err != nil {
		return nil, err
	}
	for _, p := range below {
		pids = append(pids, p)
		if sid, err := processSession(p); err == nil && !sids[sid] {
			sids[sid] = true
			if err := h.addSession(slot, sid); err != nil {
				return nil, err
			}
		}
	}
	return pids, nil
}
