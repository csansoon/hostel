package server

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/provider"
)

// workers makes sure that each transitional row has exactly one worker
// goroutine. A worker does its steps with compare-and-set writes, so a new
// worker after a restart continues safely.
type workers struct {
	mu      sync.Mutex
	running map[string]bool
	wg      sync.WaitGroup
}

func (w *workers) start(ctx context.Context, key string, fn func(context.Context), done func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	if w.running == nil {
		w.running = map[string]bool{}
	}
	if w.running[key] {
		return
	}
	w.running[key] = true
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		fn(ctx)
		w.mu.Lock()
		delete(w.running, key)
		w.mu.Unlock()
		done()
	}()
}

// kickScheduler asks for a loop pass now.
func (s *Server) kickScheduler() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// loop runs a pass at start, after each kick, and every intervals.scheduler.
func (s *Server) loop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.Intervals.Scheduler.D())
	defer ticker.Stop()
	for {
		if err := s.pass(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("loop pass", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.kick:
		}
	}
}

func (s *Server) pass(ctx context.Context) error {
	if now := time.Now(); now.Sub(s.lastReconcile) >= s.cfg.Intervals.Reconcile.D() {
		s.lastReconcile = now
		if err := s.reconcile(ctx); err != nil {
			s.log.Error("reconcile", "err", err)
		}
	}
	if err := s.warm(ctx); err != nil {
		return fmt.Errorf("warm policy: %w", err)
	}
	if err := s.schedule(ctx); err != nil {
		return fmt.Errorf("schedule: %w", err)
	}
	rooms, err := s.store.Rooms(ctx)
	if err != nil {
		return err
	}
	for _, r := range rooms {
		switch r.State {
		case protocol.RoomCreating, protocol.RoomProvisioning, protocol.RoomResuming, protocol.RoomStopping, protocol.RoomDestroying:
			id := r.ID
			s.workers.start(ctx, fmt.Sprintf("room:%d", id), func(ctx context.Context) { s.runRoom(ctx, id) }, s.kickScheduler)
		}
	}
	leases, err := s.store.ActiveLeases(ctx)
	if err != nil {
		return err
	}
	if err := s.expire(ctx, leases); err != nil {
		return fmt.Errorf("expire: %w", err)
	}
	if leases, err = s.store.ActiveLeases(ctx); err != nil {
		return err
	}
	for _, l := range leases {
		switch l.State {
		case protocol.LeaseAssigned, protocol.LeaseProvisioning, protocol.LeasePreparing, protocol.LeaseCleaning:
			id := l.ID
			s.workers.start(ctx, "lease:"+id, func(ctx context.Context) { s.runLease(ctx, id) }, s.kickScheduler)
		}
	}
	return nil
}

var roomNameRE = regexp.MustCompile(`^hostel-([a-z0-9]+)-([0-9]+)$`)

// reconcile (design, section 7) probes static rooms that were unreachable, and
// compares each on-demand provider's rooms with the database: unknown rooms
// with this server's prefix are destroyed; database rooms that the provider
// lost are marked destroyed. It never acts on rooms without the prefix.
func (s *Server) reconcile(ctx context.Context) error {
	rooms, err := s.store.Rooms(ctx)
	if err != nil {
		return err
	}
	for _, r := range rooms {
		p := s.cfg.Providers[r.Provider]
		if p != nil && p.Kind == config.ProviderStatic && r.State == protocol.RoomBroken && r.BrokenReason == reasonUnreachable {
			if _, err := s.store.SetRoomState(ctx, r.ID, []string{protocol.RoomBroken}, protocol.RoomProvisioning, "", time.Now()); err != nil {
				return err
			}
		}
	}
	prefix := "hostel-" + s.cfg.ServerID + "-"
	for _, name := range s.cfg.ProviderOrder {
		if s.cfg.Providers[name].Kind == config.ProviderStatic {
			continue
		}
		p := s.providers[name]
		lctx, cancel := context.WithTimeout(ctx, providerCallTimeout)
		listed, err := p.List(lctx, prefix)
		cancel()
		if err != nil {
			s.log.Warn("reconcile: list rooms", "provider", name, "err", err)
			continue
		}
		seen := map[int64]bool{}
		for _, ref := range listed {
			m := roomNameRE.FindStringSubmatch(ref.Name)
			if m == nil || m[1] != s.cfg.ServerID {
				continue
			}
			id, _ := strconv.ParseInt(m[2], 10, 64)
			seen[id] = true
			r, err := s.store.Room(ctx, id)
			if err == nil && r.State != protocol.RoomDestroyed && r.Provider == name {
				// A second provider room with the name, for example from a
				// create call of a killed server that ended late. Provider IDs
				// are set only at creation.
				if r.State == protocol.RoomCreating || ref.ID == "" || r.ProviderRef == "" || ref.ID == r.ProviderRef {
					continue
				}
				s.log.Warn("reconcile: destroy a second provider room of a room", "provider", name, "room", ref.Name, "id", ref.ID)
			} else {
				s.log.Warn("reconcile: destroy a room that the database does not track", "provider", name, "room", ref.Name)
			}
			dctx, cancel := context.WithTimeout(ctx, providerCallTimeout)
			if err := p.Destroy(dctx, ref); err != nil && !errors.Is(err, provider.ErrNotFound) {
				s.log.Warn("reconcile: destroy", "room", ref.Name, "err", err)
			}
			cancel()
		}
		for _, r := range rooms {
			if r.Provider != name || seen[r.ID] || r.State == protocol.RoomCreating {
				continue
			}
			s.log.Warn("reconcile: the provider lost a room", "room_id", r.ID, "room", r.Name)
			s.markLost(ctx, r, "the provider no longer lists it")
		}
	}
	return nil
}

// warm applies the warm policy (design, section 17): an idle on-demand room
// stops (or is destroyed) after warm_running; a stopped room is destroyed after
// warm_stopped; idle rooms that are broken or have too little lifetime left
// are destroyed.
func (s *Server) warm(ctx context.Context) error {
	rooms, err := s.store.Rooms(ctx)
	if err != nil {
		return err
	}
	idle, err := s.idleRooms(ctx, rooms)
	if err != nil {
		return err
	}
	slots, err := s.store.Slots(ctx)
	if err != nil {
		return err
	}
	// A room is busy while a slot has an active lease. A broken slot keeps the
	// ID of its ended lease, so lease_id alone does not count.
	busy := map[int64]bool{}
	for _, sl := range slots {
		switch sl.State {
		case protocol.SlotAssigned, protocol.SlotPreparing, protocol.SlotLeased, protocol.SlotCleaning:
			busy[sl.RoomID] = true
		}
	}
	now := time.Now()
	for _, r := range rooms {
		m := s.cfg.Machine(r.Provider + "/" + r.Machine)
		if m == nil || !m.OnDemand() {
			continue
		}
		switch {
		case r.State == protocol.RoomBroken && !busy[r.ID]:
			s.store.SetRoomState(ctx, r.ID, []string{protocol.RoomBroken}, protocol.RoomDestroying, r.BrokenReason, now)
		case r.State == protocol.RoomRunning && !idle[r.ID]:
			if !r.WarmUntil.IsZero() {
				s.store.SetWarmUntil(ctx, r.ID, time.Time{})
			}
		case r.State == protocol.RoomRunning && m.MaxLifetime.D() > 0 &&
			r.CreatedAt.Add(m.MaxLifetime.D()).Sub(now) < m.MaxLease.D()+5*time.Minute:
			s.store.SetRoomState(ctx, r.ID, []string{protocol.RoomRunning}, protocol.RoomDestroying, "too little lifetime left for a lease", now)
		case r.State == protocol.RoomRunning && r.WarmUntil.IsZero():
			s.store.SetWarmUntil(ctx, r.ID, now.Add(m.WarmRunning.D()))
		case r.State == protocol.RoomRunning && now.After(r.WarmUntil):
			to := protocol.RoomDestroying
			if s.providers[r.Provider].Capabilities(r.Machine).CanStop {
				to = protocol.RoomStopping
			}
			s.store.SetRoomState(ctx, r.ID, []string{protocol.RoomRunning}, to, "idle for warm_running", now)
		case r.State == protocol.RoomStopped && !r.WarmUntil.IsZero() && now.After(r.WarmUntil):
			s.store.SetRoomState(ctx, r.ID, []string{protocol.RoomStopped}, protocol.RoomDestroying, "stopped for warm_stopped", now)
		}
	}
	return nil
}
