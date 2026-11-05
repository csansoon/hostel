package server

import (
	"context"
	"sort"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/store"
)

// Reasons for the end of a lease (end_reason).
const (
	reasonIdle        = "idle: no guest activity for the idle timeout"
	reasonMaxLease    = "maximum lease duration reached"
	reasonPollTimeout = "poll timeout: the guest stopped polling the queued lease"
)

// expire ends ready leases after idle_timeout without activity or at
// expires_at, and cancels leases before ready that nobody polls (design,
// section 16).
func (s *Server) expire(ctx context.Context, leases []store.Lease) error {
	now := time.Now()
	for _, l := range leases {
		var endState, reason string
		switch l.State {
		case protocol.LeaseReady:
			switch {
			case !l.ExpiresAt.IsZero() && now.After(l.ExpiresAt):
				endState, reason = protocol.LeaseExpired, reasonMaxLease
			case s.gateway.CmdConns(l.ID) == 0 && now.Sub(l.LastActivityAt) > s.config().IdleTimeout.D():
				endState, reason = protocol.LeaseExpired, reasonIdle
			}
		case protocol.LeaseQueued, protocol.LeaseAssigned, protocol.LeaseProvisioning, protocol.LeasePreparing:
			if now.Sub(l.LastPollAt) > s.config().PollTimeout.D() {
				endState, reason = protocol.LeaseCancelled, reasonPollTimeout
			}
		}
		if endState == "" {
			continue
		}
		ok, err := s.store.EndLease(ctx, l.ID, []string{l.State}, endState, reason, now)
		if err != nil {
			return err
		}
		if ok {
			s.log.Info("lease ended", "lease_id", l.ID, "state", endState, "reason", reason)
			s.gateway.CloseLease(l.ID)
		}
	}
	return nil
}

// medianDuration returns the median of the last 50 lease durations on the
// machines (or, with a tool, of the leases that asked for it), or the
// fallback when there are none.
func (s *Server) medianDuration(ctx context.Context, machines []string, tool string, fallback time.Duration) time.Duration {
	ds, err := s.store.RecentDurations(ctx, machines, tool, 50)
	if err != nil || len(ds) == 0 {
		return fallback
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2]
}

// medianCreation returns the median of the creation times of the last 20
// rooms of the machines, or 5 minutes when there are none.
func (s *Server) medianCreation(ctx context.Context, machines []string) time.Duration {
	ds, err := s.store.RecentCreationTimes(ctx, machines, 20)
	if err != nil || len(ds) == 0 {
		return 5 * time.Minute
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2]
}

// estimatedWait estimates the wait of a lease with behind leases before it
// on its candidate machines (design, section 5): the leases × the median lease
// duration on the machines ÷ their slots. When the lease needs a new room
// (newRoom), it waits for the leases before it and the creation of the room,
// not for a lease of its own position. It is an estimate.
func (s *Server) estimatedWait(ctx context.Context, machines []string, behind, slots int, newRoom bool) time.Duration {
	if slots <= 0 {
		slots = 1
	}
	leases := behind
	if !newRoom {
		leases++
	}
	wait := time.Duration(leases) * s.medianDuration(ctx, machines, "", 15*time.Minute) / time.Duration(slots)
	if newRoom {
		wait += s.medianCreation(ctx, machines)
	}
	return wait
}
