package server

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/store"
)

// detailsJobsTimeout bounds all job lists of one details request: a slow or
// offline room must not block the details.
const detailsJobsTimeout = 10 * time.Second

// handleRoomDetails is GET /v1/rooms/{name} (M10): the room, its machine, its
// leases with their jobs (from the room's helper; the server does not store
// jobs), the waiting leases that it can serve, and its last events.
func (s *Server) handleRoomDetails(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("name")
	rooms, err := s.store.Rooms(ctx)
	if err != nil {
		s.internalError(w, "read rooms", err)
		return
	}
	i := slices.IndexFunc(rooms, func(rm store.Room) bool { return rm.Name == name })
	if i < 0 {
		writeError(w, http.StatusNotFound, "", fmt.Sprintf("no room %q; \"hostel status\" lists the rooms", name))
		return
	}
	room := rooms[i]
	slots, err := s.store.Slots(ctx)
	if err != nil {
		s.internalError(w, "read slots", err)
		return
	}
	rts, err := s.store.RoomTools(ctx)
	if err != nil {
		s.internalError(w, "read room tools", err)
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
	out := protocol.RoomDetails{Room: roomStatus(room, slots, rts), Leases: []protocol.LeaseStatus{},
		Jobs: map[string][]protocol.JobInfo{}, Queue: []protocol.LeaseStatus{}, Events: []protocol.Event{}}
	m := s.config().Machine(room.Provider + "/" + room.Machine)
	if m != nil {
		ms := machineStatus(m)
		out.Machine = &ms
	}
	served := []string{}
	if m != nil {
		served = append([]string{m.OS}, toolNames(m)...)
	}
	var onRoom []store.Lease
	for _, l := range leases {
		switch {
		case l.RoomID == room.ID:
			out.Leases = append(out.Leases, leaseStatus(l, users))
			onRoom = append(onRoom, l)
		case l.State == protocol.LeaseQueued && slices.Contains(served, l.Platform):
			ls := leaseStatus(l, users)
			ls.EstimatedWaitSeconds = s.leaseWait(ctx, l)
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
	for _, l := range onRoom {
		if tok.Role == protocol.RoleAdmin || s.ownLease(r, tok, l) {
			visible[l.ID] = true
			if l.State == protocol.LeaseReady {
				ready = append(ready, l)
			}
		}
	}
	// The live use and the jobs come from the room at the same time.
	usage := make(chan *protocol.Usage, 1)
	go func() { usage <- s.roomUsage(ctx, room) }()
	jobs, jobsErr := s.roomJobs(ctx, ready)
	out.Jobs, out.JobsError = jobs, jobsErr
	out.Usage = <-usage
	events, err := s.store.RoomEvents(ctx, room.ID, 10)
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

// roomJobs asks the room's helper for the jobs of each lease's slot, in
// parallel and under one deadline.
func (s *Server) roomJobs(ctx context.Context, leases []store.Lease) (map[string][]protocol.JobInfo, string) {
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

// leaseJobs asks the room's helper for the jobs of a lease's slot.
func (s *Server) leaseJobs(ctx context.Context, l store.Lease) ([]protocol.JobInfo, error) {
	room, _, err := s.openSlot(ctx, l.RoomID, l.SlotName)
	if err != nil {
		return nil, err
	}
	defer room.Conn.Close()
	var list struct {
		Jobs []protocol.JobInfo `json:"jobs"`
	}
	if err := room.Helper(ctx, []string{"job", "list", "--slot", l.SlotName}, nil, &list); err != nil {
		return nil, err
	}
	return list.Jobs, nil
}

// roomUsage asks the room's helper for its live use (M10b); nil when the room
// does not run or does not answer within detailsJobsTimeout.
func (s *Server) roomUsage(ctx context.Context, r store.Room) *protocol.Usage {
	if r.State != protocol.RoomRunning {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, detailsJobsTimeout)
	defer cancel()
	room, _, err := s.openRoom(ctx, r.ID)
	if err != nil {
		return nil
	}
	defer room.Conn.Close()
	var u protocol.Usage
	if err := room.Helper(ctx, []string{"usage"}, nil, &u); err != nil {
		return nil
	}
	return &u
}
