package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/roomops"
	"github.com/csansoon/hostel/internal/store"
)

// leaseStepTimeout bounds one lease step without its scripts.
const leaseStepTimeout = 10 * time.Minute

// runLease drives a lease through its transitional states. Each step reads the
// row, does its I/O, and writes the result with a compare-and-set; if another
// writer changed the lease (for example a release), the next read sees it.
func (s *Server) runLease(ctx context.Context, id string) {
	for ctx.Err() == nil {
		l, err := s.store.Lease(ctx, id)
		if err != nil {
			s.log.Error("read lease", "lease_id", id, "err", err)
			return
		}
		switch l.State {
		case protocol.LeaseAssigned:
			err = s.stepAssigned(ctx, l)
		case protocol.LeaseProvisioning:
			err = s.stepProvisioning(ctx, l)
		case protocol.LeasePreparing:
			err = s.stepPreparing(ctx, l)
		case protocol.LeaseCleaning:
			err = s.stepCleaning(ctx, l)
		default:
			return
		}
		if err == nil || ctx.Err() != nil {
			continue
		}
		s.log.Warn("lease step failed", "lease_id", id, "state", l.State, "err", err)
		if l.State == protocol.LeaseCleaning {
			return
		}
		if _, err := s.store.EndLease(ctx, id, []string{l.State}, protocol.LeaseFailed, err.Error(), time.Now()); err != nil {
			s.log.Error("end lease", "lease_id", id, "err", err)
			return
		}
	}
}

// endLease ends a lease by guest or admin request: a lease that is not yet
// ready is cancelled, a ready lease is released.
func (s *Server) endLease(ctx context.Context, id, reason string) error {
	l, err := s.store.Lease(ctx, id)
	if err != nil {
		return err
	}
	endState := protocol.LeaseCancelled
	if l.State == protocol.LeaseReady {
		endState = protocol.LeaseReleased
	}
	from := []string{protocol.LeaseQueued, protocol.LeaseAssigned, protocol.LeaseProvisioning, protocol.LeasePreparing, protocol.LeaseReady}
	if _, err := s.store.EndLease(ctx, id, from, endState, reason, time.Now()); err != nil {
		return err
	}
	s.gateway.CloseLease(id)
	s.kickScheduler()
	return nil
}

func (s *Server) machineOf(r store.Room) (*config.Machine, error) {
	m := s.cfg.Machine(r.Provider + "/" + r.Machine)
	if m == nil {
		return nil, fmt.Errorf("machine %s/%s is not in the configuration", r.Provider, r.Machine)
	}
	return m, nil
}

// openRoom connects to the room of a lease and returns it with the probe data
// from the database.
func (s *Server) openRoom(ctx context.Context, roomID int64) (*roomops.Room, store.Room, error) {
	r, err := s.store.Room(ctx, roomID)
	if err != nil {
		return nil, r, err
	}
	conn, err := s.connect(ctx, r)
	if err != nil {
		return nil, r, err
	}
	return &roomops.Room{Conn: conn, OS: r.OS, Arch: r.Arch, Home: r.Home}, r, nil
}

func (s *Server) stepAssigned(ctx context.Context, l store.Lease) error {
	r, err := s.store.Room(ctx, l.RoomID)
	if err != nil {
		return err
	}
	ready, err := s.readyPlatforms(ctx, r.ID)
	if err != nil {
		return err
	}
	next := protocol.LeaseProvisioning
	if have, ok := ready[l.Platform]; ok && have == s.uploads[l.Platform][r.OS].Hash {
		next = protocol.LeasePreparing
	}
	_, err = s.store.MoveLease(ctx, l.ID, []string{protocol.LeaseAssigned}, next, time.Now())
	return err
}

// stepProvisioning runs the platform provision script on a running room that is
// not yet ready for the platform.
func (s *Server) stepProvisioning(ctx context.Context, l store.Lease) error {
	p := s.cfg.Platforms[l.Platform]
	ctx, cancel := context.WithTimeout(ctx, leaseStepTimeout+p.ScriptTimeout(config.StepProvision))
	defer cancel()
	room, r, err := s.openRoom(ctx, l.RoomID)
	if err != nil {
		return err
	}
	defer room.Conn.Close()
	m, err := s.machineOf(r)
	if err != nil {
		return err
	}
	upload, err := s.roomUpload(room, m)
	if err != nil {
		return err
	}
	if _, err := room.Sync(ctx, upload); err != nil {
		return err
	}
	up := s.uploads[l.Platform][r.OS]
	if err := s.runProvision(ctx, room, r.ID, p, up); err != nil {
		return err
	}
	if err := s.store.SetRoomPlatform(ctx, r.ID, l.Platform, up.Hash, time.Now()); err != nil {
		return err
	}
	_, err = s.store.MoveLease(ctx, l.ID, []string{protocol.LeaseProvisioning}, protocol.LeasePreparing, time.Now())
	return err
}

// stepPreparing checks the helper and scripts by hash, writes the lease data
// on the room, and runs the platform prepare script.
func (s *Server) stepPreparing(ctx context.Context, l store.Lease) error {
	p := s.cfg.Platforms[l.Platform]
	ctx, cancel := context.WithTimeout(ctx, leaseStepTimeout+p.ScriptTimeout(config.StepPrepare))
	defer cancel()
	if err := s.store.SetSlots(ctx, l.ID, protocol.SlotPreparing); err != nil {
		return err
	}
	room, r, err := s.openRoom(ctx, l.RoomID)
	if err != nil {
		return err
	}
	defer room.Conn.Close()
	m, err := s.machineOf(r)
	if err != nil {
		return err
	}
	upload, err := s.roomUpload(room, m)
	if err != nil {
		return err
	}
	if _, err := room.Sync(ctx, upload); err != nil {
		return err
	}
	if err := room.Helper(ctx, []string{"begin", "--slot", l.SlotName},
		protocol.BeginRequest{LeaseID: l.ID, Platform: l.Platform, Devices: l.Devices}, nil); err != nil {
		return err
	}
	if err := s.store.SetBegan(ctx, l.ID, time.Now()); err != nil {
		return err
	}
	if s.uploads[l.Platform][r.OS].Hash != "" {
		timeout := p.ScriptTimeout(config.StepPrepare)
		env := platformEnv(p)
		env["HOSTEL_LEASE_ID"] = l.ID
		env["HOSTEL_DEVICES"] = strings.Join(l.Devices, ",")
		resp, err := room.RunScript(ctx, p.Name, config.StepPrepare, l.SlotName,
			protocol.RunScriptRequest{Env: env, TimeoutSeconds: int(timeout / time.Second)})
		if err != nil {
			return err
		}
		s.store.AddEvent(ctx, time.Now(), l.ID, r.ID, "script",
			fmt.Sprintf("%s prepare exited with code %d\n%s", p.Name, resp.ExitCode, resp.OutputTail))
		if resp.TimedOut {
			return fmt.Errorf("platform %s: prepare script timed out after %s", p.Name, timeout)
		}
		if resp.ExitCode != 0 {
			return fmt.Errorf("platform %s: prepare script exited with code %d", p.Name, resp.ExitCode)
		}
	}
	now := time.Now()
	ok, err := s.store.MarkReady(ctx, l.ID, now, now.Add(m.MaxLease.D()))
	if err != nil || !ok {
		return err
	}
	return s.store.SetSlots(ctx, l.ID, protocol.SlotLeased)
}

// stepCleaning runs the helper cleanup (design, section 17). A lease that never
// called begin only frees its slot.
func (s *Server) stepCleaning(ctx context.Context, l store.Lease) error {
	s.gateway.CloseLease(l.ID)
	if l.BeganAt.IsZero() {
		_, err := s.store.FinishCleaning(ctx, l.ID, protocol.SlotFree, time.Now())
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, leaseStepTimeout+s.healthTimeout())
	defer cancel()
	if err := s.store.SetSlots(ctx, l.ID, protocol.SlotCleaning); err != nil {
		return err
	}
	err := s.cleanup(ctx, l)
	if err == nil {
		_, err = s.store.FinishCleaning(ctx, l.ID, protocol.SlotFree, time.Now())
		s.kickScheduler()
		return err
	}
	if ctx.Err() != nil {
		return err
	}
	s.log.Warn("cleanup failed", "lease_id", l.ID, "room_id", l.RoomID, "err", err)
	if _, ferr := s.store.FinishCleaning(ctx, l.ID, protocol.SlotBroken, time.Now()); ferr != nil {
		return ferr
	}
	_, serr := s.store.SetRoomState(ctx, l.RoomID, []string{protocol.RoomRunning}, protocol.RoomBroken,
		"cleanup failed: "+err.Error(), time.Now())
	return serr
}

func (s *Server) healthTimeout() time.Duration {
	var d time.Duration
	for _, p := range s.cfg.Platforms {
		d += p.ScriptTimeout(config.StepHealth) + p.ScriptTimeout(config.StepReset)
	}
	return d
}

func (s *Server) cleanup(ctx context.Context, l store.Lease) error {
	room, r, err := s.openRoom(ctx, l.RoomID)
	if err != nil {
		return err
	}
	defer room.Conn.Close()
	m, err := s.machineOf(r)
	if err != nil {
		return err
	}
	fixed, err := s.store.LeaseTunnels(ctx, l.ID)
	if err != nil {
		return err
	}
	ports := append([]int(nil), slotConfig(m, l.SlotName).Ports...)
	for _, port := range fixed {
		ports = append(ports, port)
	}
	var resp protocol.CleanupResponse
	err = room.Helper(ctx, []string{"cleanup", "--slot", l.SlotName},
		protocol.CleanupRequest{LeaseID: l.ID, Platform: l.Platform, Devices: l.Devices, Ports: ports}, &resp)
	if err != nil {
		return err
	}
	var failed []string
	for _, st := range resp.Steps {
		if st.Error != "" {
			failed = append(failed, st.Name+": "+st.Error)
		}
	}
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, "; "))
	}
	return nil
}
