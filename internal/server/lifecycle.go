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

// medianDuration returns the median of the last 50 lease durations of a
// platform, or the fallback when there are none.
func (s *Server) medianDuration(ctx context.Context, platform string, fallback time.Duration) time.Duration {
	ds, err := s.store.RecentDurations(ctx, platform, 50)
	if err != nil || len(ds) == 0 {
		return fallback
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2]
}

// estimatedWait is position × median lease duration ÷ slots on candidate
// machines (design, section 5). It is an estimate.
func (s *Server) estimatedWait(ctx context.Context, platform string, position, slots int) time.Duration {
	if position <= 0 {
		return 0
	}
	if slots <= 0 {
		slots = 1
	}
	return time.Duration(position) * s.medianDuration(ctx, platform, 15*time.Minute) / time.Duration(slots)
}
