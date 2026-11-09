package server

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/scheduler"
	"github.com/csansoon/hostel/internal/store"
)

// detailsJobsTimeout bounds all job lists of one details request: a slow or
// offline machine must not block the details.
const detailsJobsTimeout = 10 * time.Second

// handleMachineDetails is GET /v1/machines/{name} (M10): the machine, its template, its
// leases with their jobs (from the machine's helper; the server does not store
// jobs), the waiting leases that it can serve, and its last events.
func (s *Server) handleMachineDetails(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("name")
	machines, err := s.store.Machines(ctx)
	if err != nil {
		s.internalError(w, "read machines", err)
		return
	}
	i := slices.IndexFunc(machines, func(rm store.Machine) bool { return rm.Name == name })
	if i < 0 {
		writeError(w, http.StatusNotFound, "", fmt.Sprintf("no machine %q; \"hostel status\" lists the machines", name))
		return
	}
	machine := machines[i]
	rooms, err := s.store.Rooms(ctx)
	if err != nil {
		s.internalError(w, "read rooms", err)
		return
	}
	rts, err := s.store.MachineTools(ctx)
	if err != nil {
		s.internalError(w, "read machine tools", err)
		return
	}
	leases, err := s.store.ActiveLeases(ctx)
	if err != nil {
		s.internalError(w, "read leases", err)
		return
	}
	users, err := s.store.TokenUserNames(ctx)
	if err != nil {
		s.internalError(w, "read users", err)
		return
	}
	out := protocol.MachineDetails{Machine: machineStatus(s.config(), machine, rooms, rts), Leases: []protocol.LeaseStatus{},
		Jobs: map[string][]protocol.JobInfo{}, Queue: []protocol.LeaseStatus{}, Events: []protocol.Event{}}
	m := s.config().Template(machine.Provider + "/" + machine.Template)
	if m != nil {
		ms := templateStatus(m)
		out.Template = &ms
	}
	view, err := s.loadToolView(ctx)
	if err != nil {
		s.internalError(w, "read machines", err)
		return
	}
	templates := view.schedTemplates()
	var onMachine []store.Lease
	for _, l := range leases {
		switch {
		case l.MachineID == machine.ID:
			out.Leases = append(out.Leases, leaseStatus(l, users))
			onMachine = append(onMachine, l)
		case l.State == protocol.LeaseQueued && m != nil && slices.Contains(scheduler.Candidates(templates, leaseRequest(view.lc.cfg, l)), m.Key()):
			ls := leaseStatus(l, users)
			ls.EstimatedWaitSeconds, _ = s.leaseWait(ctx, l)
			out.Queue = append(out.Queue, ls)
		}
	}
	// A lease that the scheduler has not placed yet (position 0) comes last.
	slices.SortStableFunc(out.Queue, func(a, b protocol.LeaseStatus) int {
		if (a.Position == 0) != (b.Position == 0) {
			if a.Position == 0 {
				return 1
			}
			return -1
		}
		return a.Position - b.Position
	})
	// Job commands and event messages can hold secrets: a member sees them
	// only for the member's own leases.
	tok := requestToken(r)
	visible := map[string]bool{}
	var ready []store.Lease
	for _, l := range onMachine {
		if tok.Role == protocol.RoleAdmin || s.ownLease(r, tok, l) {
			visible[l.ID] = true
			if l.State == protocol.LeaseReady {
				ready = append(ready, l)
			}
		}
	}
	// The live use and the jobs come from the machine at the same time.
	usage := make(chan *protocol.Usage, 1)
	go func() { usage <- s.machineUsage(ctx, machine) }()
	jobs, jobsErr := s.machineJobs(ctx, ready)
	out.Jobs, out.JobsError = jobs, jobsErr
	out.Usage = <-usage
	events, err := s.store.MachineEvents(ctx, machine.ID, 10)
	if err != nil {
		s.internalError(w, "read events", err)
		return
	}
	for _, e := range events {
		if tok.Role != protocol.RoleAdmin && !visible[e.LeaseID] && !s.ownEvent(r, tok, e) {
			continue
		}
		out.Events = append(out.Events, protocol.Event{Time: e.Time, LeaseID: e.LeaseID, Kind: e.Kind, Message: e.Message})
	}
	writeJSON(w, http.StatusOK, out)
}

// ownEvent reports whether an event belongs to a lease of the token's user
// (also a lease that has ended).
func (s *Server) ownEvent(r *http.Request, tok store.Token, e store.Event) bool {
	if e.LeaseID == "" {
		return false
	}
	l, err := s.store.Lease(r.Context(), e.LeaseID)
	return err == nil && s.ownLease(r, tok, l)
}

// machineJobs asks the machine's helper for the jobs of each lease's room, in
// parallel and under one deadline.
func (s *Server) machineJobs(ctx context.Context, leases []store.Lease) (map[string][]protocol.JobInfo, string) {
	ctx, cancel := context.WithTimeout(ctx, detailsJobsTimeout)
	defer cancel()
	out := map[string][]protocol.JobInfo{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	for _, l := range leases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			jobs, err := s.leaseJobs(ctx, l)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			out[l.ID] = jobs
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return out, "jobs unknown: " + firstErr.Error()
	}
	return out, ""
}

// leaseJobs asks the machine's helper for the jobs of a lease's room.
func (s *Server) leaseJobs(ctx context.Context, l store.Lease) ([]protocol.JobInfo, error) {
	machine, _, err := s.openRoom(ctx, l.MachineID, l.RoomName)
	if err != nil {
		return nil, err
	}
	defer machine.Conn.Close()
	var list struct {
		Jobs []protocol.JobInfo `json:"jobs"`
	}
	if err := machine.Helper(ctx, []string{"job", "list", "--room", l.RoomName}, nil, &list); err != nil {
		return nil, err
	}
	return list.Jobs, nil
}

// machineUsage asks the machine's helper for its live use (M10b); nil when the machine
// does not run or does not answer within detailsJobsTimeout.
func (s *Server) machineUsage(ctx context.Context, r store.Machine) *protocol.Usage {
	if r.State != protocol.MachineRunning {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, detailsJobsTimeout)
	defer cancel()
	machine, _, err := s.openMachine(ctx, r.ID)
	if err != nil {
		return nil
	}
	defer machine.Conn.Close()
	var u protocol.Usage
	if err := machine.Helper(ctx, []string{"usage"}, nil, &u); err != nil {
		return nil
	}
	return &u
}

// usageMaxAge is how long a sample of the machines' use stays valid: the app
// polls GET /v1/usage, and each sample opens SSH to each running machine.
const usageMaxAge = 8 * time.Second

// usageParallel bounds the SSH connections of one sample.
const usageParallel = 8

// usageSampleTimeout bounds one sample, below the app's request timeout.
const usageSampleTimeout = 20 * time.Second

// usageCache holds the last sample. The server samples only while a client
// asks. busy has room for one: clients wait for the sample of another client,
// and each one can leave when its request ends.
type usageCache struct {
	busy     chan struct{}
	at       time.Time
	machines map[string]protocol.Usage
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	select {
	case s.usage.busy <- struct{}{}:
		defer func() { <-s.usage.busy }()
	case <-r.Context().Done():
		return
	}
	if time.Since(s.usage.at) >= usageMaxAge {
		// Not the context of this request: others share this sample, also
		// when this client leaves.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), usageSampleTimeout)
		defer cancel()
		machines, err := s.store.Machines(ctx)
		if err != nil {
			s.internalError(w, "read machines", err)
			return
		}
		s.usage.machines, s.usage.at = s.sampleUsage(ctx, machines), time.Now()
	}
	writeJSON(w, http.StatusOK, protocol.UsageList{Machines: s.usage.machines})
}

// sampleUsage asks each running machine for its use; each read has the
// deadline of machineUsage. A machine that does not answer has no entry.
func (s *Server) sampleUsage(ctx context.Context, machines []store.Machine) map[string]protocol.Usage {
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out = map[string]protocol.Usage{}
		sem = make(chan struct{}, usageParallel)
	)
	for _, m := range machines {
		if m.State != protocol.MachineRunning {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if u := s.machineUsage(ctx, m); u != nil {
				mu.Lock()
				out[m.Name] = *u
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return out
}
