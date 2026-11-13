package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/machineops"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/store"
)

// leaseStepTimeout bounds one lease step without its scripts.
const leaseStepTimeout = 10 * time.Minute

// machineWait is the poll interval of a lease that waits for its machine.
const machineWait = 500 * time.Millisecond

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
	// lease now, so this step does not break the machine or clean up.
	cur, err := s.store.Lease(ctx, l.ID)
	if err != nil || cur.State != l.State {
		return
	}
	l = cur
	r, err := s.store.Machine(ctx, l.MachineID)
	if err != nil {
		s.store.Requeue(ctx, l.ID, from, stepErr.Error(), true, maxAttempts, protocol.RoomFree, "", now)
		return
	}
	m, err := s.templateOf(r)
	if err != nil {
		s.store.Requeue(ctx, l.ID, from, stepErr.Error(), true, maxAttempts, protocol.RoomFree, "", now)
		return
	}
	began := !l.BeganAt.IsZero()
	if errors.Is(stepErr, machineops.ErrUnreachable) {
		if m.OnDemand() {
			// An attempt, so a machine with a bad SSH setup cannot create
			// machines without limit.
			s.store.SetMachineState(ctx, r.ID, []string{r.State}, protocol.MachineDestroying, reasonUnreachable, now)
			s.store.Requeue(ctx, l.ID, from, "machine unreachable", true, maxAttempts, protocol.RoomFree, "", now)
			return
		}
		s.store.SetMachineState(ctx, r.ID, []string{r.State}, protocol.MachineBroken, reasonUnreachable, now)
		roomState, data := protocol.RoomFree, ""
		if began {
			roomState, data = protocol.RoomDirty, s.cleanupData(ctx, l)
		}
		s.store.Requeue(ctx, l.ID, from, "machine unreachable", false, maxAttempts, roomState, data, now)
		return
	}
	// A script, upload, or helper failure: clean the room if the lease began,
	// mark the machine broken, and requeue the lease as an attempt.
	roomState, data := protocol.RoomFree, ""
	if began {
		if err := s.cleanupLease(ctx, l); err != nil {
			s.log.Warn("cleanup after a failed lease step", "lease_id", l.ID, "err", err)
			roomState, data = protocol.RoomBroken, s.cleanupData(ctx, l)
		}
	}
	s.store.SetMachineState(ctx, r.ID, []string{r.State}, protocol.MachineBroken, stepErr.Error(), now)
	s.store.Requeue(ctx, l.ID, from, stepErr.Error(), true, maxAttempts, roomState, data, now)
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

func (s *Server) templateOf(r store.Machine) (*config.Template, error) {
	m := s.config().Template(r.Provider + "/" + r.Template)
	if m == nil {
		return nil, fmt.Errorf("machine %s/%s is not in the configuration", r.Provider, r.Template)
	}
	return m, nil
}

// openMachine connects to a machine and returns it with the probe data from the
// database.
func (s *Server) openMachine(ctx context.Context, machineID int64) (*machineops.Machine, store.Machine, error) {
	r, err := s.store.Machine(ctx, machineID)
	if err != nil {
		return nil, r, err
	}
	conn, err := s.connect(ctx, r)
	if err != nil {
		return nil, r, err
	}
	return &machineops.Machine{Conn: conn, OS: r.OS, Arch: r.Arch, Home: r.Home, Shell: r.Shell}, r, nil
}

// openRoom connects to a machine for the work of one room: as the room's own
// user, in its home directory, if the room has one; else as openMachine does.
func (s *Server) openRoom(ctx context.Context, machineID int64, room string) (*machineops.Machine, store.Machine, error) {
	r, err := s.store.Machine(ctx, machineID)
	if err != nil {
		return nil, r, err
	}
	home, err := s.roomHome(ctx, r, room)
	if err != nil {
		return nil, r, err
	}
	user, err := s.roomUserOf(ctx, r, room)
	if err != nil {
		return nil, r, err
	}
	conn, err := s.connectAs(ctx, r, user)
	if err != nil {
		return nil, r, err
	}
	machine := &machineops.Machine{Conn: conn, OS: r.OS, Arch: r.Arch, Home: home, Shell: r.Shell}
	if user != "" && roomUser(s.config().Template(r.Provider+"/"+r.Template), room) == "" {
		machine.SharedHome = r.Home
	}
	return machine, r, nil
}

// readyFor reports whether a machine has installed and checked each tool of a
// lease and their needs.
func (s *Server) readyFor(ctx context.Context, r store.Machine, m *config.Template, tools []string) (bool, error) {
	rts, err := s.store.MachineTools(ctx)
	if err != nil {
		return false, err
	}
	ready := machineReady(s.live.Load(), rts, r, m)
	for _, name := range toolNamesOf(tools) {
		if !ready[name] {
			return false, nil
		}
	}
	return true, nil
}

func (s *Server) stepAssigned(ctx context.Context, l store.Lease) error {
	r, err := s.store.Machine(ctx, l.MachineID)
	if err != nil {
		return err
	}
	m, err := s.templateOf(r)
	if err != nil {
		return err
	}
	next := protocol.LeaseProvisioning
	if r.State == protocol.MachineRunning {
		if ready, err := s.readyFor(ctx, r, m, l.Tools); err != nil {
			return err
		} else if ready {
			next = protocol.LeasePreparing
		}
	}
	_, err = s.store.MoveLease(ctx, l.ID, []string{protocol.LeaseAssigned}, next, time.Now())
	return err
}

// stepProvisioning waits while the machine is in creation or resume, then runs the
// install scripts of the lease's tools and their needs if the machine is not yet
// ready for them.
func (s *Server) stepProvisioning(ctx context.Context, l store.Lease) error {
	for {
		r, err := s.store.Machine(ctx, l.MachineID)
		if err != nil {
			return err
		}
		switch r.State {
		case protocol.MachineRunning:
			return s.provisionTool(ctx, l)
		case protocol.MachineCreating, protocol.MachineProvisioning, protocol.MachineResuming:
		default:
			// The machine worker requeues the leases of a failed machine; this is
			// only a guard if it has not done so.
			_, err := s.store.Requeue(ctx, l.ID, []string{protocol.LeaseProvisioning}, "machine "+r.State,
				false, maxAttempts, protocol.RoomFree, "", time.Now())
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(machineWait):
		}
		if cur, err := s.store.Lease(ctx, l.ID); err != nil || cur.State != protocol.LeaseProvisioning {
			return err
		}
	}
}

func (s *Server) provisionTool(ctx context.Context, l store.Lease) error {
	// One configuration for the whole step: the scripts that run and the
	// hashes that the machine records must be of the same tool version.
	lc := s.live.Load()
	timeout := leaseStepTimeout
	var closure []string
	for _, t := range leaseClosure(lc.cfg, l.Tools) {
		closure = append(closure, t.Name)
		timeout += t.ScriptTimeout(config.StepInstall) + t.ScriptTimeout(config.StepCheck)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	machine, r, err := s.openMachine(ctx, l.MachineID)
	if err != nil {
		return err
	}
	defer machine.Conn.Close()
	m, err := s.templateOf(r)
	if err != nil {
		return err
	}
	installed, err := s.installedTools(ctx, r.ID)
	if err != nil {
		return err
	}
	synced := false
	for _, t := range lc.cfg.DefinedTools(m) {
		sum := installHash(lc, m, t.Name)
		if !slices.Contains(closure, t.Name) || installed[t.Name] == sum {
			continue
		}
		if !synced {
			upload, err := s.machineUpload(lc, machine, m)
			if err != nil {
				return err
			}
			if _, err := s.syncMachine(ctx, l.MachineID, machine, upload); err != nil {
				return err
			}
			synced = true
		}
		if err := s.installTool(ctx, machine, r.ID, m, t, sum); err != nil {
			return err
		}
	}
	_, err = s.store.MoveLease(ctx, l.ID, []string{protocol.LeaseProvisioning}, protocol.LeasePreparing, time.Now())
	return err
}

// stepPreparing checks the helper and scripts by hash, writes the lease data
// on the machine, and runs the start script of each screen tool of the lease, in
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
	if err := s.store.SetRooms(ctx, l.ID, protocol.RoomPreparing); err != nil {
		return err
	}
	if err := s.ensureLeaseUser(ctx, l); err != nil {
		return err
	}
	machine, r, err := s.openRoom(ctx, l.MachineID, l.RoomName)
	if err != nil {
		return err
	}
	defer machine.Conn.Close()
	m, err := s.templateOf(r)
	if err != nil {
		return err
	}
	upload, err := s.machineUpload(lc, machine, m)
	if err != nil {
		return err
	}
	if _, err := s.syncMachine(ctx, l.MachineID, machine, upload); err != nil {
		return err
	}
	view, err := s.loadToolView(ctx)
	if err != nil {
		return err
	}
	starts := planStarts(lc.cfg, l, view.machineHaves(m, r.ID))
	begin := protocol.BeginRequest{LeaseID: l.ID, Tools: leaseToolEntries(l.Tools, starts), Started: starts,
		Exclusive: leaseRequest(lc.cfg, l).Exclusive}
	if err := machine.Helper(ctx, []string{"begin", "--room", l.RoomName}, begin, nil); err != nil {
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
		// Recorded first: a start that fails can leave processes, and cleanup
		// must stop them (stop scripts are idempotent).
		started = withStarted(started, st)
		if err := s.store.SetStarted(ctx, l.ID, started); err != nil {
			return err
		}
		timeout := t.ScriptTimeout(config.StepStart)
		env := map[string]string{"HOSTEL_LEASE_ID": l.ID, "HOSTEL_TOOL_OPTIONS": strings.Join(st.Options, ",")}
		resp, err := machine.RunScript(ctx, t.Name, config.StepStart, l.RoomName,
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
		started = withStarted(started, st)
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
	return s.store.SetRooms(ctx, l.ID, protocol.RoomLeased)
}

// withStarted puts st last in started, in place of its earlier entry.
func withStarted(started []protocol.StartedTool, st protocol.StartedTool) []protocol.StartedTool {
	out := slices.DeleteFunc(slices.Clone(started), func(o protocol.StartedTool) bool { return o.Name == st.Name })
	return append(out, st)
}

// extendDeadline moves the provider deadline of a machine that cannot stop to
// ready_at + max_lease + warm_running + 10 minutes, if that is later.
func (s *Server) extendDeadline(ctx context.Context, r store.Machine, m *config.Template, readyAt time.Time) {
	p := s.providerMap()[r.Provider]
	if !p.Capabilities(r.Template).CanExtend {
		return
	}
	deadline := readyAt.Add(m.MaxLease.D() + m.WarmRunning.D() + 10*time.Minute)
	if !r.ProviderDeadline.IsZero() && !deadline.After(r.ProviderDeadline) {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, providerCallTimeout)
	defer cancel()
	if err := p.Extend(cctx, s.ref(r), deadline); err != nil {
		s.log.Warn("extend the provider deadline", "machine_id", r.ID, "err", err)
		return
	}
	s.store.SetProviderDeadline(ctx, r.ID, deadline)
}

// stepCleaning runs the helper cleanup (design, section 17). A lease that never
// called begin only frees its room.
func (s *Server) stepCleaning(ctx context.Context, l store.Lease) error {
	s.gateway.CloseLease(l.ID)
	if l.BeganAt.IsZero() {
		// A lease user that preparing created before begin goes now.
		if u, err := s.store.RoomLeaseUser(ctx, l.MachineID, l.RoomName); err == nil && u != "" {
			if err := s.deleteLeaseUsers(ctx, l.MachineID, l.RoomName); err != nil {
				return err
			}
		}
		_, err := s.store.FinishCleaning(ctx, l.ID, protocol.RoomFree, "", time.Now())
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, leaseStepTimeout+s.cleanupScriptsTimeout())
	defer cancel()
	if err := s.store.SetRooms(ctx, l.ID, protocol.RoomCleaning); err != nil {
		return err
	}
	err := s.cleanupLease(ctx, l)
	defer s.kickScheduler()
	if err == nil {
		_, err = s.store.FinishCleaning(ctx, l.ID, protocol.RoomFree, "", time.Now())
		return err
	}
	if ctx.Err() != nil {
		return err
	}
	s.log.Warn("cleanup failed", "lease_id", l.ID, "machine_id", l.MachineID, "err", err)
	r, rerr := s.store.Machine(ctx, l.MachineID)
	if rerr != nil {
		return rerr
	}
	m, merr := s.templateOf(r)
	if merr != nil {
		return merr
	}
	data := s.cleanupData(ctx, l)
	if errors.Is(err, machineops.ErrUnreachable) {
		// The cleanup runs when the machine is reachable again (static), or the
		// machine goes (on-demand).
		if m.OnDemand() {
			s.store.SetMachineState(ctx, r.ID, []string{r.State}, protocol.MachineDestroying, reasonUnreachable, time.Now())
			_, err := s.store.FinishCleaning(ctx, l.ID, protocol.RoomFree, "", time.Now())
			return err
		}
		s.store.SetMachineState(ctx, r.ID, []string{r.State}, protocol.MachineBroken, reasonUnreachable, time.Now())
		_, err := s.store.FinishCleaning(ctx, l.ID, protocol.RoomDirty, data, time.Now())
		return err
	}
	if _, ferr := s.store.FinishCleaning(ctx, l.ID, protocol.RoomBroken, data, time.Now()); ferr != nil {
		return ferr
	}
	_, serr := s.store.SetMachineState(ctx, l.MachineID, []string{r.State}, protocol.MachineBroken, "cleanup failed: "+err.Error(), time.Now())
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
	if r, err := s.store.Machine(ctx, l.MachineID); err == nil {
		if m, err := s.templateOf(r); err == nil {
			req.Ports = append(req.Ports, roomConfig(m, l.RoomName).Ports...)
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
	machine, _, err := s.openRoom(ctx, l.MachineID, l.RoomName)
	if err != nil {
		return err
	}
	defer machine.Conn.Close()
	return s.runCleanup(ctx, machine, l.MachineID, l.RoomName, s.cleanupRequest(ctx, l))
}

// recordChecks writes the results of the check scripts of a cleanup to the
// machine's inventory, with the install hashes of the tools that the machine had
// when the cleanup began (sums, from checkTools): a tool version that an admin
// pushed during the cleanup is not installed yet.
func (s *Server) recordChecks(ctx context.Context, machineID int64, checks []protocol.ToolCheck, sums map[string]string) error {
	r, err := s.store.Machine(ctx, machineID)
	if err != nil {
		return err
	}
	m, err := s.templateOf(r)
	if err != nil {
		return err
	}
	for _, c := range checks {
		t := s.config().Tools[c.Tool]
		sum, ok := sums[c.Tool]
		if t == nil || !ok {
			continue
		}
		row := store.MachineTool{MachineID: machineID, Tool: c.Tool, ScriptsSHA256: sum, State: store.ToolReady,
			Option: c.Option, Env: envNames(t), CheckedAt: time.Now()}
		if msg := noTemplateOption(s.config(), m, t, c.Option); msg != "" && c.Error == "" {
			s.store.AddEvent(ctx, time.Now(), "", machineID, "script", msg)
			c.Error = msg
		}
		if c.Error != "" {
			row.State, row.Option = store.ToolFailed, ""
		}
		if err := s.store.SetMachineTool(ctx, row); err != nil {
			return err
		}
	}
	return nil
}

// runCleanup sets the tools for check at the time of the cleanup: a stored
// request (of a dirty room) can be older than the machine's last provision.
func (s *Server) runCleanup(ctx context.Context, machine *machineops.Machine, machineID int64, room string, req protocol.CleanupRequest) error {
	check, _, err := s.checkTools(ctx, machineID)
	if err != nil {
		return fmt.Errorf("find the tools to check: %w", err)
	}
	r, err := s.store.Machine(ctx, machineID)
	if err != nil {
		return err
	}
	// Check checks the machine, whose install ran as the machine user: a
	// room with its own user runs it as the machine user after the cleanup.
	user, err := s.roomUserOf(ctx, r, room)
	if err != nil {
		return err
	}
	ownUser := user != ""
	leaseUser := ownUser && roomUser(s.config().Template(r.Provider+"/"+r.Template), room) == ""
	if !ownUser {
		req.Check = sortedKeys(check)
	}
	var resp protocol.CleanupResponse
	err = machine.Helper(ctx, []string{"cleanup", "--room", room}, req, &resp)
	if errors.Is(err, machineops.ErrNoHelper) {
		// A lease of a server before M10g: its machine still has the old binary.
		if err = s.syncMachineHelper(ctx, machineID, machine); err == nil {
			err = machine.Helper(ctx, []string{"cleanup", "--room", room}, req, &resp)
		}
	}
	if err != nil {
		return err
	}
	if err := s.recordChecks(ctx, machineID, resp.Checks, check); err != nil {
		return err
	}
	if len(resp.Removed) > 0 {
		if err := s.store.AddEvent(ctx, time.Now(), req.LeaseID, machineID, "cleanup",
			"removed persistence of the lease: "+strings.Join(resp.Removed, ", ")); err != nil {
			s.log.Error("record removed persistence", "lease_id", req.LeaseID, "err", err)
		}
	}
	var failed []string
	for _, st := range resp.Steps {
		// The deletion of a lease user ends all its processes and removes all
		// its files; only the tools' stop scripts must have worked.
		if st.Error != "" && (!leaseUser || st.Name == "stop screen tools") {
			failed = append(failed, st.Name+": "+st.Error)
		}
	}
	if err := s.deleteLeaseUsers(ctx, machineID, room); err != nil {
		failed = append(failed, err.Error())
	}
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, "; "))
	}
	if ownUser && len(check) > 0 {
		template, _, err := s.openMachine(ctx, machineID)
		if err != nil {
			return err
		}
		defer template.Conn.Close()
		if err := s.runChecks(ctx, template, machineID); err != nil {
			return fmt.Errorf("check: %w", err)
		}
	}
	return nil
}

// syncMachineHelper installs the server's helper through machine: a room with
// its own OS user has its own helper in its own home.
func (s *Server) syncMachineHelper(ctx context.Context, machineID int64, machine *machineops.Machine) error {
	r, err := s.store.Machine(ctx, machineID)
	if err != nil {
		return err
	}
	lc := s.live.Load()
	m := lc.cfg.Template(r.Provider + "/" + r.Template)
	if m == nil {
		return fmt.Errorf("install the helper: the machine %s has no entry in the configuration", r.Name)
	}
	upload, err := s.machineUpload(lc, machine, m)
	if err != nil {
		return err
	}
	_, err = s.syncMachine(ctx, machineID, machine, upload)
	return err
}

// syncMachine runs one upload at a time on a machine: uploads use fixed
// temporary paths, and provisioning can run beside a cleanup.
func (s *Server) syncMachine(ctx context.Context, machineID int64, machine *machineops.Machine, upload machineops.Upload) (string, error) {
	v, _ := s.syncLocks.LoadOrStore(machineID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	return machine.Sync(ctx, upload)
}
