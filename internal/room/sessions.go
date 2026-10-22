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
// "<sid> <start time of the session leader>".

func (h *helper) recordSession(slot string) error {
	sid, err := currentSession()
	if err != nil {
		return nil // no sessions on this OS
	}
	start, err := processStart(sid)
	if err != nil {
		return fmt.Errorf("record the session: %w", err)
	}
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
	more, err := sessionProcesses(h.leaseSessions(slot))
	if err != nil {
		return nil, err
	}
	for _, p := range more {
		if !slices.Contains(pids, p) {
			pids = append(pids, p)
		}
	}
	return pids, nil
}
