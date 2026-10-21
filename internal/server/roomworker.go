package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/provider"
	"github.com/csansoon/hostel/internal/roomops"
	"github.com/csansoon/hostel/internal/store"
)

// maxAttempts: a lease fails at its third failed attempt (design, section 6).
const maxAttempts = 3

// providerCallTimeout bounds a provider call that is not provision.
const providerCallTimeout = 5 * time.Minute

// fullFlags are the machines with a provider "limit reached" error. A flag
// clears after limit_retry, or when a room of that provider is destroyed.
type fullFlags struct {
	mu sync.Mutex
	m  map[string]time.Time // machine key → time of the error
}

func (f *fullFlags) set(machine string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m == nil {
		f.m = map[string]time.Time{}
	}
	f.m[machine] = time.Now()
}

func (f *fullFlags) clearProvider(provider string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.m {
		if strings.HasPrefix(k, provider+"/") {
			delete(f.m, k)
		}
	}
}

func (f *fullFlags) current(retry time.Duration) map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]bool{}
	for k, t := range f.m {
		if time.Since(t) < retry {
			out[k] = true
		} else {
			delete(f.m, k)
		}
	}
	return out
}

func (s *Server) ref(r store.Room) provider.RoomRef {
	return provider.RoomRef{Name: r.Name, Machine: r.Machine, ID: r.ProviderRef}
}

// runRoom drives a room through its transitional states. Each step is safe to
// repeat after a restart. When a step does not change the state (for example a
// failed destroy), the worker ends; the next loop pass starts it again, so the
// retry follows intervals.scheduler and is never a tight loop.
func (s *Server) runRoom(ctx context.Context, id int64) {
	for ctx.Err() == nil {
		r, err := s.store.Room(ctx, id)
		if err != nil {
			s.log.Error("read room", "room_id", id, "err", err)
			return
		}
		m := s.cfg.Machine(r.Provider + "/" + r.Machine)
		if m == nil {
			return
		}
		switch r.State {
		case protocol.RoomCreating:
			s.stepCreating(ctx, r, m)
		case protocol.RoomProvisioning:
			s.stepRoomProvisioning(ctx, r, m)
		case protocol.RoomResuming:
			s.stepResuming(ctx, r)
		case protocol.RoomStopping:
			s.stepStopping(ctx, r, m)
		case protocol.RoomDestroying:
			s.stepDestroying(ctx, r)
		default:
			return
		}
		s.kickScheduler()
		if after, err := s.store.Room(ctx, id); err != nil || after.State == r.State {
			return
		}
	}
}

// creationDeadline is the provider-side bound of a new room (design, section 7,
// deadlines): long enough for creation, the platform scripts, a full lease, and
// the warm time.
func (s *Server) creationDeadline(m *config.Machine) time.Time {
	d := 10 * time.Minute
	if p := s.cfg.Providers[m.Provider]; p.Timeouts != nil {
		d = p.Timeouts.Provision.D()
	}
	var scripts time.Duration
	for _, name := range m.Platforms {
		p := s.cfg.Platforms[name]
		scripts = max(scripts, p.ScriptTimeout(config.StepProvision)+p.ScriptTimeout(config.StepPrepare))
	}
	return time.Now().Add(d + scripts + m.MaxLease.D() + m.WarmRunning.D() + 10*time.Minute)
}

// stepCreating calls the provider (idempotent by name). Other errors are retried
// with backoff, three attempts in total.
func (s *Server) stepCreating(ctx context.Context, r store.Room, m *config.Machine) {
	p := s.providers[r.Provider]
	deadline := s.creationDeadline(m)
	req := provider.ProvisionRequest{Name: r.Name, Machine: r.Machine, Spec: m.Spec, Deadline: deadline,
		ServerKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.serverKey.PublicKey())))}
	var ref provider.RoomRef
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		ref, err = p.Provision(ctx, req)
		if err == nil || errors.Is(err, provider.ErrLimit) || ctx.Err() != nil {
			break
		}
		s.log.Warn("provider provision failed", "room_id", r.ID, "attempt", attempt, "err", err)
		s.store.AddEvent(ctx, time.Now(), "", r.ID, "provider", fmt.Sprintf("provision attempt %d failed: %v", attempt, err))
		if attempt < maxAttempts {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
	}
	if ctx.Err() != nil {
		return
	}
	if err == nil {
		if err := s.store.SetProviderRef(ctx, r.ID, ref.ID, deadline); err != nil {
			s.log.Error("record the provider ID", "room_id", r.ID, "err", err)
			return
		}
		s.store.SetRoomState(ctx, r.ID, []string{protocol.RoomCreating}, protocol.RoomProvisioning, "", time.Now())
		return
	}
	// The room never existed at the provider: remove anything with its name.
	dctx, cancel := context.WithTimeout(ctx, providerCallTimeout)
	if derr := p.Destroy(dctx, s.ref(r)); derr != nil && !errors.Is(derr, provider.ErrNotFound) {
		s.log.Warn("destroy a failed room by name", "room_id", r.ID, "err", derr)
	}
	cancel()
	s.store.SetRoomState(ctx, r.ID, []string{protocol.RoomCreating}, protocol.RoomDestroyed, err.Error(), time.Now())
	leases := s.roomLeases(ctx, r.ID)
	if errors.Is(err, provider.ErrLimit) {
		s.full.set(m.Key())
		for _, l := range leases {
			s.store.Requeue(ctx, l.ID, []string{protocol.LeaseAssigned, protocol.LeaseProvisioning}, "provider limit reached",
				false, maxAttempts, protocol.SlotFree, "", time.Now())
		}
		// Limits that Hostel cannot count (for example vCPUs): make space with
		// an idle Hostel room of this provider, at once.
		s.evictIdle(ctx, r.Provider)
		return
	}
	for _, l := range leases {
		s.store.FailLease(ctx, l.ID, []string{protocol.LeaseAssigned, protocol.LeaseProvisioning},
			"the provider failed to create a room: "+err.Error(), protocol.SlotFree, "", time.Now())
	}
}

// roomLeases returns the active leases on a room.
func (s *Server) roomLeases(ctx context.Context, roomID int64) []store.Lease {
	leases, err := s.store.ActiveLeases(ctx)
	if err != nil {
		return nil
	}
	var out []store.Lease
	for _, l := range leases {
		if l.RoomID == roomID {
			out = append(out, l)
		}
	}
	return out
}

// evictIdle destroys the oldest idle Hostel room of a provider.
func (s *Server) evictIdle(ctx context.Context, providerName string) {
	rooms, err := s.store.Rooms(ctx)
	if err != nil {
		return
	}
	idle, err := s.idleRooms(ctx, rooms)
	if err != nil {
		return
	}
	for _, r := range rooms {
		if r.Provider == providerName && idle[r.ID] {
			s.store.SetRoomState(ctx, r.ID, []string{protocol.RoomRunning, protocol.RoomStopped}, protocol.RoomDestroying,
				"evicted after a provider limit error", time.Now())
			return
		}
	}
}

// idleRooms returns the on-demand rooms with no lease: running with all slots
// free, or stopped.
func (s *Server) idleRooms(ctx context.Context, rooms []store.Room) (map[int64]bool, error) {
	slots, err := s.store.Slots(ctx)
	if err != nil {
		return nil, err
	}
	busy := map[int64]bool{}
	for _, sl := range slots {
		if sl.State != protocol.SlotFree {
			busy[sl.RoomID] = true
		}
	}
	out := map[int64]bool{}
	for _, r := range rooms {
		m := s.cfg.Machine(r.Provider + "/" + r.Machine)
		if m == nil || !m.OnDemand() || busy[r.ID] {
			continue
		}
		if r.State == protocol.RoomStopped || r.State == protocol.RoomRunning {
			out[r.ID] = true
		}
	}
	return out, nil
}

// stepRoomProvisioning probes and uploads, cleans dirty slots, runs the health
// scripts, and sets the room to running. Static rooms also run the provision
// script of each platform; on-demand rooms do that when a lease first uses a
// platform.
func (s *Server) stepRoomProvisioning(ctx context.Context, r store.Room, m *config.Machine) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout+miseInstallTimeout+s.maxProvisionTimeout()+s.healthTimeout())
	defer cancel()
	err := s.doProvision(ctx, r, m)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		s.log.Warn("room provisioning failed", "room_id", r.ID, "err", err)
		s.roomFailed(ctx, r, m, err)
		return
	}
	if ok, _ := s.store.SetRoomState(ctx, r.ID, []string{protocol.RoomProvisioning}, protocol.RoomRunning, "", time.Now()); ok {
		s.store.SetWarmUntil(ctx, r.ID, time.Time{})
	}
}

// roomFailed applies the failure rules of design section 6 to a room that
// failed outside a lease step: a static room is broken (and recovers by itself
// when it was only unreachable); an on-demand room is destroyed. The leases
// that wait for it go back to the queue.
func (s *Server) roomFailed(ctx context.Context, r store.Room, m *config.Machine, err error) {
	reason := err.Error()
	unreachable := errors.Is(err, roomops.ErrUnreachable)
	switch {
	case unreachable:
		reason = reasonUnreachable
	case errors.Is(err, roomops.ErrHostKeyChanged):
		reason = "host key changed"
	}
	to := protocol.RoomBroken
	if m.OnDemand() {
		to = protocol.RoomDestroying
	}
	s.store.SetRoomState(ctx, r.ID, []string{r.State}, to, reason, time.Now())
	// An unreachable static room is not an attempt; the other failures are.
	attempt := !(unreachable && !m.OnDemand())
	for _, l := range s.roomLeases(ctx, r.ID) {
		s.store.Requeue(ctx, l.ID, []string{protocol.LeaseAssigned, protocol.LeaseProvisioning}, "room failed: "+reason,
			attempt, maxAttempts, protocol.SlotFree, "", time.Now())
	}
}

func (s *Server) stepResuming(ctx context.Context, r store.Room) {
	cctx, cancel := context.WithTimeout(ctx, providerCallTimeout)
	err := s.providers[r.Provider].Resume(cctx, s.ref(r))
	cancel()
	switch {
	case ctx.Err() != nil:
	case err == nil:
		s.store.SetRoomState(ctx, r.ID, []string{protocol.RoomResuming}, protocol.RoomProvisioning, "", time.Now())
	case errors.Is(err, provider.ErrNotFound):
		s.markLost(ctx, r, "the provider no longer has the room")
	default:
		s.log.Warn("resume failed", "room_id", r.ID, "err", err)
		m, _ := s.machineOf(r)
		s.roomFailed(ctx, r, m, err)
	}
}

func (s *Server) stepStopping(ctx context.Context, r store.Room, m *config.Machine) {
	cctx, cancel := context.WithTimeout(ctx, providerCallTimeout)
	err := s.providers[r.Provider].Stop(cctx, s.ref(r))
	cancel()
	switch {
	case ctx.Err() != nil:
	case err == nil:
		if ok, _ := s.store.SetRoomState(ctx, r.ID, []string{protocol.RoomStopping}, protocol.RoomStopped, "", time.Now()); ok {
			s.store.SetWarmUntil(ctx, r.ID, time.Now().Add(m.WarmStopped.D()))
		}
	case errors.Is(err, provider.ErrNotFound):
		s.markLost(ctx, r, "the provider no longer has the room")
	default:
		s.log.Warn("stop failed; destroy the room", "room_id", r.ID, "err", err)
		s.store.SetRoomState(ctx, r.ID, []string{protocol.RoomStopping}, protocol.RoomDestroying, "stop failed: "+err.Error(), time.Now())
	}
}

func (s *Server) stepDestroying(ctx context.Context, r store.Room) {
	cctx, cancel := context.WithTimeout(ctx, providerCallTimeout)
	err := s.providers[r.Provider].Destroy(cctx, s.ref(r))
	cancel()
	if ctx.Err() != nil {
		return
	}
	if err != nil && !errors.Is(err, provider.ErrNotFound) {
		s.log.Warn("destroy failed; the next loop pass tries again", "room_id", r.ID, "err", err)
		s.store.AddEvent(ctx, time.Now(), "", r.ID, "provider", "destroy failed: "+err.Error())
		return
	}
	s.markLost(ctx, r, "")
}

// markLost sets a room to destroyed and ends or requeues its leases: ready and
// cleaning leases fail with reason (cleanup is not possible); leases before
// ready go back to the queue. A destroyed Hostel room clears the full flags of
// its provider.
func (s *Server) markLost(ctx context.Context, r store.Room, reason string) {
	from := []string{r.State}
	if r.State == protocol.RoomDestroyed {
		return
	}
	if ok, err := s.store.SetRoomState(ctx, r.ID, from, protocol.RoomDestroyed, reason, time.Now()); err != nil || !ok {
		return
	}
	s.full.clearProvider(r.Provider)
	failReason := "room lost"
	if reason != "" {
		failReason += ": " + reason
	}
	for _, l := range s.roomLeases(ctx, r.ID) {
		switch l.State {
		case protocol.LeaseReady, protocol.LeaseCleaning:
			s.store.FailLease(ctx, l.ID, []string{l.State}, failReason, protocol.SlotFree, "", time.Now())
			s.gateway.CloseLease(l.ID)
		default:
			s.store.Requeue(ctx, l.ID, []string{protocol.LeaseAssigned, protocol.LeaseProvisioning, protocol.LeasePreparing},
				failReason, false, maxAttempts, protocol.SlotFree, "", time.Now())
		}
	}
}

// cleanDirtySlots runs the cleanup of each dirty slot of a room (design,
// section 4, "Dirty slot").
func (s *Server) cleanDirtySlots(ctx context.Context, room *roomops.Room, roomID int64) error {
	slots, err := s.store.Slots(ctx)
	if err != nil {
		return err
	}
	for _, sl := range slots {
		if sl.RoomID != roomID || sl.State != protocol.SlotDirty {
			continue
		}
		var req protocol.CleanupRequest
		if err := json.Unmarshal([]byte(sl.CleanupData), &req); err != nil {
			return fmt.Errorf("slot %s: cleanup data: %w", sl.Name, err)
		}
		if err := s.runCleanup(ctx, room, sl.Name, req); err != nil {
			return fmt.Errorf("cleanup of slot %s: %w", sl.Name, err)
		}
		if err := s.store.SetSlot(ctx, roomID, sl.Name, protocol.SlotFree, "", ""); err != nil {
			return err
		}
	}
	return nil
}
