package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/roomops"
	"github.com/csansoon/hostel/internal/store"
)

// leaseStepTimeout bounds one lease step without its scripts.
const leaseStepTimeout = 10 * time.Minute

// roomWait is the poll interval of a lease that waits for its room.
const roomWait = 500 * time.Millisecond

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
		s.leaseStepFailed(ctx, l, err)
	}
}

// leaseStepFailed applies the failure rules of design section 6 to a lease
// before ready.
func (s *Server) leaseStepFailed(ctx context.Context, l store.Lease, stepErr error) {
	now := time.Now()
	from := []string{l.State}
	// Read the lease again: the step can have set began_at. A release or an
	// expiry during the step changed the state: the cleaning step owns the
	// lease now, so this step does not break the room or clean up.
	cur, err := s.store.Lease(ctx, l.ID)
	if err != nil || cur.State != l.State {
		return
	}
	l = cur
	r, err := s.store.Room(ctx, l.RoomID)
	if err != nil {
		s.store.Requeue(ctx, l.ID, from, stepErr.Error(), true, maxAttempts, protocol.SlotFree, "", now)
		return
	}
	m, err := s.machineOf(r)
	if err != nil {
		s.store.Requeue(ctx, l.ID, from, stepErr.Error(), true, maxAttempts, protocol.SlotFree, "", now)
		return
	}
	began := !l.BeganAt.IsZero()
	if errors.Is(stepErr, roomops.ErrUnreachable) {
		if m.OnDemand() {
			// An attempt, so a machine with a bad SSH setup cannot create
			// rooms without limit.
			s.store.SetRoomState(ctx, r.ID, []string{r.State}, protocol.RoomDestroying, reasonUnreachable, now)
			s.store.Requeue(ctx, l.ID, from, "room unreachable", true, maxAttempts, protocol.SlotFree, "", now)
			return
		}
		s.store.SetRoomState(ctx, r.ID, []string{r.State}, protocol.RoomBroken, reasonUnreachable, now)
		slotState, data := protocol.SlotFree, ""
		if began {
			slotState, data = protocol.SlotDirty, s.cleanupData(ctx, l)
		}
		s.store.Requeue(ctx, l.ID, from, "room unreachable", false, maxAttempts, slotState, data, now)
		return
	}
	// A script, upload, or helper failure: clean the slot if the lease began,
	// mark the room broken, and requeue the lease as an attempt.
	slotState, data := protocol.SlotFree, ""
	if began {
		if err := s.cleanupLease(ctx, l); err != nil {
			s.log.Warn("cleanup after a failed lease step", "lease_id", l.ID, "err", err)
			slotState, data = protocol.SlotBroken, s.cleanupData(ctx, l)
		}
	}
	s.store.SetRoomState(ctx, r.ID, []string{r.State}, protocol.RoomBroken, stepErr.Error(), now)
	s.store.Requeue(ctx, l.ID, from, stepErr.Error(), true, maxAttempts, slotState, data, now)
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
	m := s.config().Machine(r.Provider + "/" + r.Machine)
	if m == nil {
		return nil, fmt.Errorf("machine %s/%s is not in the configuration", r.Provider, r.Machine)
	}
	return m, nil
}

// openRoom connects to a room and returns it with the probe data from the
// database.
func (s *Server) openRoom(ctx context.Context, roomID int64) (*roomops.Room, store.Room, error) {
	r, err := s.store.Room(ctx, roomID)
	if err != nil {
		return nil, r, err
	}
	conn, err := s.connect(ctx, r)
	if err != nil {
		return nil, r, err
	}
	return &roomops.Room{Conn: conn, OS: r.OS, Arch: r.Arch, Home: r.Home, Shell: r.Shell}, r, nil
}

// openSlot connects to a room for the work of one slot: as the slot's own
// user, in its home directory, if the slot has one; else as openRoom does.
func (s *Server) openSlot(ctx context.Context, roomID int64, slot string) (*roomops.Room, store.Room, error) {
	r, err := s.store.Room(ctx, roomID)
	if err != nil {
		return nil, r, err
	}
	home, err := s.slotHome(ctx, r, slot)
	if err != nil {
		return nil, r, err
	}
	conn, err := s.connectAs(ctx, r, slotUser(s.config().Machine(r.Provider+"/"+r.Machine), slot))
	if err != nil {
		return nil, r, err
	}
	return &roomops.Room{Conn: conn, OS: r.OS, Arch: r.Arch, Home: home, Shell: r.Shell}, r, nil
}

// readyFor reports whether a room has installed and checked each tool of a
// lease and their needs.
func (s *Server) readyFor(ctx context.Context, r store.Room, m *config.Machine, tools []string) (bool, error) {
	rts, err := s.store.RoomTools(ctx)
	if err != nil {
		return false, err
	}
	ready := roomReady(s.live.Load(), rts, r, m)
	for _, name := range toolNamesOf(tools) {
		if !ready[name] {
			return false, nil
		}
	}
	return true, nil
}

func (s *Server) stepAssigned(ctx context.Context, l store.Lease) error {
	r, err := s.store.Room(ctx, l.RoomID)
	if err != nil {
		return err
	}
	m, err := s.machineOf(r)
	if err != nil {
		return err
	}
	next := protocol.LeaseProvisioning
	if r.State == protocol.RoomRunning {
		if ready, err := s.readyFor(ctx, r, m, l.Tools); err != nil {
			return err
		} else if ready {
			next = protocol.LeasePreparing
		}
	}
	_, err = s.store.MoveLease(ctx, l.ID, []string{protocol.LeaseAssigned}, next, time.Now())
	return err
}

// stepProvisioning waits while the room is in creation or resume, then runs the
// install scripts of the lease's tools and their needs if the room is not yet
// ready for them.
func (s *Server) stepProvisioning(ctx context.Context, l store.Lease) error {
	for {
		r, err := s.store.Room(ctx, l.RoomID)
		if err != nil {
			return err
		}
		switch r.State {
		case protocol.RoomRunning:
			return s.provisionTool(ctx, l)
		case protocol.RoomCreating, protocol.RoomProvisioning, protocol.RoomResuming:
		default:
			// The room worker requeues the leases of a failed room; this is
			// only a guard if it has not done so.
			_, err := s.store.Requeue(ctx, l.ID, []string{protocol.LeaseProvisioning}, "room "+r.State,
				false, maxAttempts, protocol.SlotFree, "", time.Now())
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(roomWait):
		}
		if cur, err := s.store.Lease(ctx, l.ID); err != nil || cur.State != protocol.LeaseProvisioning {
			return err
		}
	}
}

func (s *Server) provisionTool(ctx context.Context, l store.Lease) error {
	// One configuration for the whole step: the scripts that run and the
	// hashes that the room records must be of the same tool version.
	lc := s.live.Load()
	timeout := leaseStepTimeout
	var closure []string
	for _, t := range leaseClosure(lc.cfg, l.Tools) {
		closure = append(closure, t.Name)
		timeout += t.ScriptTimeout(config.StepInstall) + t.ScriptTimeout(config.StepCheck)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
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
	installed, err := s.installedTools(ctx, r.ID)
	if err != nil {
		return err
	}
	synced := false
	for _, t := range lc.cfg.DefinedTools(m) {
		up := lc.uploads[t.Name][m.OS]
		if !slices.Contains(closure, t.Name) || installed[t.Name] == up.Hash {
			continue
		}
		if !synced {
			upload, err := s.roomUpload(lc, room, m)
			if err != nil {
				return err
			}
			if _, err := room.Sync(ctx, upload); err != nil {
				return err
			}
			synced = true
		}
		if err := s.installTool(ctx, room, r.ID, m.OS, t, up); err != nil {
			return err
		}
	}
	_, err = s.store.MoveLease(ctx, l.ID, []string{protocol.LeaseProvisioning}, protocol.LeasePreparing, time.Now())
	return err
}

// stepPreparing checks the helper and scripts by hash, writes the lease data
// on the room, and runs the start script of each screen tool of the lease, in
// the order of needs (design, section 8). It records each screen tool that
// started, so that cleanup stops only those.
func (s *Server) stepPreparing(ctx context.Context, l store.Lease) error {
	lc := s.live.Load()
	timeout := leaseStepTimeout
	for _, t := range leaseClosure(lc.cfg, l.Tools) {
		timeout += t.ScriptTimeout(config.StepStart)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := s.store.SetSlots(ctx, l.ID, protocol.SlotPreparing); err != nil {
		return err
	}
	room, r, err := s.openSlot(ctx, l.RoomID, l.SlotName)
	if err != nil {
		return err
	}
	defer room.Conn.Close()
	m, err := s.machineOf(r)
	if err != nil {
		return err
	}
	upload, err := s.roomUpload(lc, room, m)
	if err != nil {
		return err
	}
	if _, err := room.Sync(ctx, upload); err != nil {
		return err
	}
	view, err := s.loadToolView(ctx)
	if err != nil {
		return err
	}
	starts := planStarts(lc.cfg, l, view.roomHaves(m, r.ID))
	begin := protocol.BeginRequest{LeaseID: l.ID, Tools: leaseToolEntries(l.Tools, starts), Started: starts,
		Exclusive: leaseRequest(lc.cfg, l).Exclusive}
	if err := room.Helper(ctx, []string{"begin", "--slot", l.SlotName}, begin, nil); err != nil {
		return err
	}
	if err := s.store.SetBegan(ctx, l.ID, time.Now()); err != nil {
		return err
	}
	// A repeated step (after a restart) keeps what started before: the
	// start scripts are idempotent, and cleanup must stop those tools.
	started := slices.Clone(l.Started)
	for _, st := range starts {
		t := lc.cfg.Tools[st.Name]
		if t == nil || t.Script(m.OS, config.StepStart) == "" {
			continue
		}
		timeout := t.ScriptTimeout(config.StepStart)
		env := map[string]string{"HOSTEL_LEASE_ID": l.ID, "HOSTEL_TOOL_OPTIONS": strings.Join(st.Options, ",")}
		resp, err := room.RunScript(ctx, t.Name, config.StepStart, l.SlotName,
			protocol.RunScriptRequest{Env: env, TimeoutSeconds: int(timeout / time.Second)})
		if err != nil {
			return err
		}
		s.store.AddEvent(ctx, time.Now(), l.ID, r.ID, "script",
			fmt.Sprintf("%s start exited with code %d\n%s", t.Name, resp.ExitCode, resp.OutputTail))
		if resp.TimedOut {
			return fmt.Errorf("tool %s: start script timed out after %s", t.Name, timeout)
		}
		if resp.ExitCode != 0 {
			return fmt.Errorf("tool %s: start script exited with code %d", t.Name, resp.ExitCode)
		}
		st.Facts = resp.Facts
		started = slices.DeleteFunc(started, func(o protocol.StartedTool) bool { return o.Name == st.Name })
		started = append(started, st)
		if err := s.store.SetStarted(ctx, l.ID, started); err != nil {
			return err
		}
	}
	now := time.Now()
	ok, err := s.store.MarkReady(ctx, l.ID, now, now.Add(m.MaxLease.D()))
	if err != nil || !ok {
		return err
	}
	s.extendDeadline(ctx, r, m, now)
	return s.store.SetSlots(ctx, l.ID, protocol.SlotLeased)
}

// extendDeadline moves the provider deadline of a machine that cannot stop to
// ready_at + max_lease + warm_running + 10 minutes, if that is later.
func (s *Server) extendDeadline(ctx context.Context, r store.Room, m *config.Machine, readyAt time.Time) {
	p := s.providerMap()[r.Provider]
	if !p.Capabilities(r.Machine).CanExtend {
		return
	}
	deadline := readyAt.Add(m.MaxLease.D() + m.WarmRunning.D() + 10*time.Minute)
	if !r.ProviderDeadline.IsZero() && !deadline.After(r.ProviderDeadline) {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, providerCallTimeout)
	defer cancel()
	if err := p.Extend(cctx, s.ref(r), deadline); err != nil {
		s.log.Warn("extend the provider deadline", "room_id", r.ID, "err", err)
		return
	}
	s.store.SetProviderDeadline(ctx, r.ID, deadline)
}

// stepCleaning runs the helper cleanup (design, section 17). A lease that never
// called begin only frees its slot.
func (s *Server) stepCleaning(ctx context.Context, l store.Lease) error {
	s.gateway.CloseLease(l.ID)
	if l.BeganAt.IsZero() {
		_, err := s.store.FinishCleaning(ctx, l.ID, protocol.SlotFree, "", time.Now())
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, leaseStepTimeout+s.cleanupScriptsTimeout())
	defer cancel()
	if err := s.store.SetSlots(ctx, l.ID, protocol.SlotCleaning); err != nil {
		return err
	}
	err := s.cleanupLease(ctx, l)
	defer s.kickScheduler()
	if err == nil {
		_, err = s.store.FinishCleaning(ctx, l.ID, protocol.SlotFree, "", time.Now())
		return err
	}
	if ctx.Err() != nil {
		return err
	}
	s.log.Warn("cleanup failed", "lease_id", l.ID, "room_id", l.RoomID, "err", err)
	r, rerr := s.store.Room(ctx, l.RoomID)
	if rerr != nil {
		return rerr
	}
	m, merr := s.machineOf(r)
	if merr != nil {
		return merr
	}
	data := s.cleanupData(ctx, l)
	if errors.Is(err, roomops.ErrUnreachable) {
		// The cleanup runs when the room is reachable again (static), or the
		// room goes (on-demand).
		if m.OnDemand() {
			s.store.SetRoomState(ctx, r.ID, []string{r.State}, protocol.RoomDestroying, reasonUnreachable, time.Now())
			_, err := s.store.FinishCleaning(ctx, l.ID, protocol.SlotFree, "", time.Now())
			return err
		}
		s.store.SetRoomState(ctx, r.ID, []string{r.State}, protocol.RoomBroken, reasonUnreachable, time.Now())
		_, err := s.store.FinishCleaning(ctx, l.ID, protocol.SlotDirty, data, time.Now())
		return err
	}
	if _, ferr := s.store.FinishCleaning(ctx, l.ID, protocol.SlotBroken, data, time.Now()); ferr != nil {
		return ferr
	}
	_, serr := s.store.SetRoomState(ctx, l.RoomID, []string{r.State}, protocol.RoomBroken, "cleanup failed: "+err.Error(), time.Now())
	return serr
}

func (s *Server) cleanupScriptsTimeout() time.Duration {
	var d time.Duration
	for _, t := range s.config().Tools {
		d += t.ScriptTimeout(config.StepCheck) + t.ScriptTimeout(config.StepStop)
	}
	return d
}

// cleanupRequest is the input of the helper cleanup: from the database, so a
// repeated cleanup does not need lease.json.
func (s *Server) cleanupRequest(ctx context.Context, l store.Lease) protocol.CleanupRequest {
	req := protocol.CleanupRequest{LeaseID: l.ID, Tools: leaseToolEntries(l.Tools, l.Started), Started: nonNilStarted(l.Started),
		Exclusive: leaseRequest(s.config(), l).Exclusive}
	if r, err := s.store.Room(ctx, l.RoomID); err == nil {
		if m, err := s.machineOf(r); err == nil {
			req.Ports = append(req.Ports, slotConfig(m, l.SlotName).Ports...)
		}
	}
	if fixed, err := s.store.LeaseTunnels(ctx, l.ID); err == nil {
		for _, p := range fixed {
			req.Ports = append(req.Ports, p)
		}
	}
	return req
}

func (s *Server) cleanupData(ctx context.Context, l store.Lease) string {
	data, _ := json.Marshal(s.cleanupRequest(ctx, l))
	return string(data)
}

func (s *Server) cleanupLease(ctx context.Context, l store.Lease) error {
	room, _, err := s.openSlot(ctx, l.RoomID, l.SlotName)
	if err != nil {
		return err
	}
	defer room.Conn.Close()
	return s.runCleanup(ctx, room, l.RoomID, l.SlotName, s.cleanupRequest(ctx, l))
}

// recordChecks writes the results of the check scripts of a cleanup to the
// room's inventory.
func (s *Server) recordChecks(ctx context.Context, roomID int64, checks []protocol.ToolCheck) error {
	r, err := s.store.Room(ctx, roomID)
	if err != nil {
		return err
	}
	m, err := s.machineOf(r)
	if err != nil {
		return err
	}
	for _, c := range checks {
		t := s.config().Tools[c.Tool]
		if t == nil {
			continue
		}
		row := store.RoomTool{RoomID: roomID, Tool: c.Tool, ScriptsSHA256: s.toolUploads()[c.Tool][m.OS].Hash, State: store.ToolReady,
			Option: c.Option, Env: envNames(t), CheckedAt: time.Now()}
		if c.Error != "" {
			row.State, row.Option = store.ToolFailed, ""
		}
		if err := s.store.SetRoomTool(ctx, row); err != nil {
			return err
		}
	}
	return nil
}

// runCleanup sets the tools for check at the time of the cleanup: a stored
// request (of a dirty slot) can be older than the room's last provision.
func (s *Server) runCleanup(ctx context.Context, room *roomops.Room, roomID int64, slot string, req protocol.CleanupRequest) error {
	check, _, err := s.checkTools(ctx, roomID)
	if err != nil {
		return fmt.Errorf("find the tools to check: %w", err)
	}
	r, err := s.store.Room(ctx, roomID)
	if err != nil {
		return err
	}
	// Check checks the machine, whose install ran as the machine user: a
	// slot with its own user runs it as the machine user after the cleanup.
	ownUser := slotUser(s.config().Machine(r.Provider+"/"+r.Machine), slot) != ""
	if !ownUser {
		req.Check = check
	}
	var resp protocol.CleanupResponse
	if err := room.Helper(ctx, []string{"cleanup", "--slot", slot}, req, &resp); err != nil {
		return err
	}
	if err := s.recordChecks(ctx, roomID, resp.Checks); err != nil {
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
	if ownUser && len(check) > 0 {
		machine, _, err := s.openRoom(ctx, roomID)
		if err != nil {
			return err
		}
		defer machine.Conn.Close()
		if err := s.runChecks(ctx, machine, roomID); err != nil {
			return fmt.Errorf("check: %w", err)
		}
	}
	return nil
}
