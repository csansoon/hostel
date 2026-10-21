package server

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
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
	rooms, err := s.store.Rooms(ctx)
	if err != nil {
		return err
	}
	for _, r := range rooms {
		if r.State == protocol.RoomProvisioning {
			id := r.ID
			s.workers.start(ctx, fmt.Sprintf("room:%d", id), func(ctx context.Context) { s.provisionRoom(ctx, id) }, s.kickScheduler)
		}
	}
	if err := s.schedule(ctx); err != nil {
		return fmt.Errorf("schedule: %w", err)
	}
	leases, err := s.store.ActiveLeases(ctx)
	if err != nil {
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

// reconcile probes static rooms that are broken because they were unreachable;
// the room worker sets them to running when the probe succeeds (design, section 7).
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
	return nil
}
