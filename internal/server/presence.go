package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/csansoon/hostel/internal/machineops"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/store"
)

// presenceTimeout bounds one presence check: an SSH connection to the machine
// (through the gateway for a machine that connects to the server).
const presenceTimeout = 10 * time.Second

// checkPresence starts a presence check of each idle static machine, and of
// each static machine that is offline (broken: unreachable), every
// intervals.presence (M10b). A machine that is switched off is then offline
// before a lease tries it, and the scheduler does not give it leases; when
// it answers again, it provisions again and is vacant without an admin.
func (s *Server) checkPresence(ctx context.Context) error {
	now := time.Now()
	if now.Sub(s.lastPresence) < s.config().Intervals.Presence.D() {
		return nil
	}
	s.lastPresence = now
	machines, err := s.store.Machines(ctx)
	if err != nil {
		return err
	}
	leases, err := s.store.ActiveLeases(ctx)
	if err != nil {
		return err
	}
	busy := map[int64]bool{}
	for _, l := range leases {
		busy[l.MachineID] = true
	}
	for _, r := range machines {
		m := s.config().Template(r.Provider + "/" + r.Template)
		if m == nil || m.OnDemand() || busy[r.ID] {
			continue
		}
		offline := r.State == protocol.MachineBroken && r.BrokenReason == reasonUnreachable
		if r.State != protocol.MachineRunning && !offline {
			continue
		}
		id := r.ID
		s.workers.start(ctx, fmt.Sprintf("presence:%d", id), func(ctx context.Context) { s.presence(ctx, id) }, s.kickScheduler)
	}
	return nil
}

// presence checks one machine: a running machine that does not answer goes
// offline; an offline machine that answers provisions again.
func (s *Server) presence(ctx context.Context, id int64) {
	r, err := s.store.Machine(ctx, id)
	if err != nil {
		return
	}
	unreachable, err := s.unreachable(ctx, r)
	now := time.Now()
	if errors.Is(err, machineops.ErrHostKeyChanged) {
		// The machine answers, but with another host key: broken until an
		// admin accepts the new key (repair). Only an idle or offline machine.
		if busy, berr := s.machineBusy(ctx, r.ID); berr == nil && !busy {
			s.store.SetMachineState(ctx, r.ID, []string{protocol.MachineRunning, protocol.MachineBroken}, protocol.MachineBroken, reasonHostKeyChanged, now)
		}
		return
	}
	if err != nil {
		// Another error is not a presence matter: the next machine step
		// reports it.
		return
	}
	switch {
	case !unreachable:
		if err := s.store.SetLastSeen(ctx, r.ID, now); err != nil {
			s.log.Warn("presence: save the last seen time", "machine", r.Name, "err", err)
		}
		if r.State == protocol.MachineBroken && r.BrokenReason == reasonUnreachable {
			s.store.SetMachineState(ctx, r.ID, []string{protocol.MachineBroken}, protocol.MachineProvisioning, "", now)
		}
	case r.State == protocol.MachineRunning:
		// Only an idle machine: a lease that started meanwhile finds out by
		// itself (its step fails as unreachable).
		if busy, err := s.machineBusy(ctx, r.ID); err != nil || busy {
			return
		}
		if moved, _ := s.store.SetMachineState(ctx, r.ID, []string{protocol.MachineRunning}, protocol.MachineBroken, reasonUnreachable, now); moved {
			s.log.Info("presence: machine offline", "machine", r.Name)
		}
	}
}

// unreachable makes an SSH connection to the machine with the server key and
// closes it. It must log in: OpenSSH (9.8 and later, PerSourcePenalties)
// penalizes the source of connections that close without authentication, and
// then refuses the server's own connections.
func (s *Server) unreachable(ctx context.Context, r store.Machine) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, presenceTimeout)
	defer cancel()
	conn, err := s.connect(ctx, r)
	if errors.Is(err, machineops.ErrUnreachable) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	conn.Close()
	return false, nil
}

// machineBusy reports whether a lease uses the machine.
func (s *Server) machineBusy(ctx context.Context, id int64) (bool, error) {
	leases, err := s.store.ActiveLeases(ctx)
	if err != nil {
		return false, err
	}
	for _, l := range leases {
		if l.MachineID == id {
			return true, nil
		}
	}
	return false, nil
}
