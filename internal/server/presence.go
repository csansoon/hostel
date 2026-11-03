package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/roomops"
	"github.com/csansoon/hostel/internal/store"
)

// presenceTimeout bounds one presence check: an SSH connection to the room
// (through the gateway for a machine that connects to the server).
const presenceTimeout = 10 * time.Second

// checkPresence starts a presence check of each idle static room, and of
// each static room that is offline (broken: unreachable), every
// intervals.presence (M10b). A machine that is switched off is then offline
// before a lease tries it, and the scheduler does not give it leases; when
// it answers again, it provisions again and is vacant without an admin.
func (s *Server) checkPresence(ctx context.Context) error {
	now := time.Now()
	if now.Sub(s.lastPresence) < s.config().Intervals.Presence.D() {
		return nil
	}
	s.lastPresence = now
	rooms, err := s.store.Rooms(ctx)
	if err != nil {
		return err
	}
	leases, err := s.store.ActiveLeases(ctx)
	if err != nil {
		return err
	}
	busy := map[int64]bool{}
	for _, l := range leases {
		busy[l.RoomID] = true
	}
	for _, r := range rooms {
		m := s.config().Machine(r.Provider + "/" + r.Machine)
		if m == nil || m.OnDemand() || busy[r.ID] {
			continue
		}
		offline := r.State == protocol.RoomBroken && r.BrokenReason == reasonUnreachable
		if r.State != protocol.RoomRunning && !offline {
			continue
		}
		id := r.ID
		s.workers.start(ctx, fmt.Sprintf("presence:%d", id), func(ctx context.Context) { s.presence(ctx, id) }, s.kickScheduler)
	}
	return nil
}

// presence checks one room: a running room that does not answer goes
// offline; an offline room that answers provisions again.
func (s *Server) presence(ctx context.Context, id int64) {
	r, err := s.store.Room(ctx, id)
	if err != nil {
		return
	}
	unreachable, err := s.unreachable(ctx, r)
	now := time.Now()
	if errors.Is(err, roomops.ErrHostKeyChanged) {
		// The machine answers, but with another host key: broken until an
		// admin accepts the new key (repair). Only an idle or offline room.
		if busy, berr := s.roomBusy(ctx, r.ID); berr == nil && !busy {
			s.store.SetRoomState(ctx, r.ID, []string{protocol.RoomRunning, protocol.RoomBroken}, protocol.RoomBroken, reasonHostKeyChanged, now)
		}
		return
	}
	if err != nil {
		// Another error is not a presence matter: the next room step
		// reports it.
		return
	}
	switch {
	case !unreachable:
		if err := s.store.SetLastSeen(ctx, r.ID, now); err != nil {
			s.log.Warn("presence: save the last seen time", "room", r.Name, "err", err)
		}
		if r.State == protocol.RoomBroken && r.BrokenReason == reasonUnreachable {
			s.store.SetRoomState(ctx, r.ID, []string{protocol.RoomBroken}, protocol.RoomProvisioning, "", now)
		}
	case r.State == protocol.RoomRunning:
		// Only an idle room: a lease that started meanwhile finds out by
		// itself (its step fails as unreachable).
		if busy, err := s.roomBusy(ctx, r.ID); err != nil || busy {
			return
		}
		if moved, _ := s.store.SetRoomState(ctx, r.ID, []string{protocol.RoomRunning}, protocol.RoomBroken, reasonUnreachable, now); moved {
			s.log.Info("presence: room offline", "room", r.Name)
		}
	}
}

// unreachable makes an SSH connection to the room with the server key and
// closes it. It must log in: OpenSSH (9.8 and later, PerSourcePenalties)
// penalizes the source of connections that close without authentication, and
// then refuses the server's own connections.
func (s *Server) unreachable(ctx context.Context, r store.Room) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, presenceTimeout)
	defer cancel()
	conn, err := s.connect(ctx, r)
	if errors.Is(err, roomops.ErrUnreachable) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	conn.Close()
	return false, nil
}

// roomBusy reports whether a lease uses the room.
func (s *Server) roomBusy(ctx context.Context, id int64) (bool, error) {
	leases, err := s.store.ActiveLeases(ctx)
	if err != nil {
		return false, err
	}
	for _, l := range leases {
		if l.RoomID == id {
			return true, nil
		}
	}
	return false, nil
}
