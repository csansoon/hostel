package room

import (
	"bufio"
	"fmt"
	"io"
	"maps"
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
// descendant while its parent runs. Each scan, also the one at the start of
// each guest command and job, records its session, so later scans find it
// after its parent ended.

func (h *helper) recordSession(slot string) error {
	sid, err := currentSession()
	if err != nil {
		return nil // no sessions on this OS
	}
	return h.addSession(slot, sid)
}

// trackSessions records the sessions of the lease's descendants at the start
// of a guest command or job. A failure does not stop the command, so it is
// only a warning.
func (h *helper) trackSessions(slot, leaseID string, warn io.Writer) {
	if _, err := currentSession(); err != nil {
		return // no sessions on this OS
	}
	if _, err := h.leaseProcesses(slot, leaseID); err != nil {
		fmt.Fprintf(warn, "hostel: warning: find the lease processes: %v; cleanup can miss a process that this lease started\n", err)
	}
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
func (h *helper) leaseSessions(slot string) (map[int]bool, error) {
	recorded := map[int][]string{}
	if f, err := os.Open(filepath.Join(h.slotDir(slot), "sessions")); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			sidText, start, ok := strings.Cut(sc.Text(), " ")
			if sid, err := strconv.Atoi(sidText); ok && err == nil && sid > 1 {
				recorded[sid] = append(recorded[sid], start)
			}
		}
		f.Close()
	}
	des, _ := os.ReadDir(h.jobsDir(slot))
	for _, de := range des {
		if sup, err := h.readSupervisor(filepath.Join(h.jobsDir(slot), de.Name())); err == nil && sup.SupervisorPID > 1 {
			recorded[sup.SupervisorPID] = append(recorded[sup.SupervisorPID], sup.SupervisorStart)
		}
	}
	sids := map[int]bool{}
	if len(recorded) == 0 {
		return sids, nil
	}
	// A failed lookup is an error, not "the leader has ended": that would skip
	// the start time check and could match another lease's session.
	starts, err := processStarts(slices.Collect(maps.Keys(recorded)))
	if err != nil {
		return nil, fmt.Errorf("read the process start times: %w", err)
	}
	for sid, recs := range recorded {
		cur, running := starts[sid]
		if !running || slices.Contains(recs, cur) {
			sids[sid] = true
		}
	}
	return sids, nil
}

// leaseProcesses returns the room user's processes of the lease.
func (h *helper) leaseProcesses(slot, leaseID string) ([]int, error) {
	pids, err := envProcesses(leaseID)
	if err != nil {
		return nil, err
	}
	sids, err := h.leaseSessions(slot)
	if err != nil {
		return nil, err
	}
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
