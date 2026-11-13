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
	"github.com/csansoon/hostel/internal/machineops"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/provider"
	"github.com/csansoon/hostel/internal/store"
)

// maxAttempts: a lease fails at its third failed attempt (design, section 6).
const maxAttempts = 3

// providerCallTimeout bounds a provider call that is not provision.
const providerCallTimeout = 5 * time.Minute

// fullFlags are the machines with a provider "limit reached" error. A flag
// clears after limit_retry, or when a machine of that provider is destroyed.
type fullFlags struct {
	mu sync.Mutex
	m  map[string]time.Time // machine key → time of the error
}

func (f *fullFlags) set(template string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m == nil {
		f.m = map[string]time.Time{}
	}
	f.m[template] = time.Now()
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

func (s *Server) ref(r store.Machine) provider.MachineRef {
	return provider.MachineRef{Name: r.Name, Template: r.Template, ID: r.ProviderRef}
}

// runMachine drives a machine through its transitional states. Each step is safe to
// repeat after a restart. When a step does not change the state (for example a
// failed destroy), the worker ends; the next loop pass starts it again, so the
// retry follows intervals.scheduler and is never a tight loop.
func (s *Server) runMachine(ctx context.Context, id int64) {
	for ctx.Err() == nil {
		r, err := s.store.Machine(ctx, id)
		if err != nil {
			s.log.Error("read machine", "machine_id", id, "err", err)
			return
		}
		m := s.config().Template(r.Provider + "/" + r.Template)
		if m == nil {
			// A machine removed from the configuration: its on-demand machine is
			// destroyed (refreshMachines).
			if r.State == protocol.MachineDestroying && s.providerMap()[r.Provider] != nil {
				s.stepDestroying(ctx, r)
				s.kickScheduler()
			}
			return
		}
		switch r.State {
		case protocol.MachineCreating:
			s.stepCreating(ctx, r, m)
		case protocol.MachineProvisioning:
			s.stepMachineProvisioning(ctx, r, m)
		case protocol.MachineResuming:
			s.stepResuming(ctx, r)
		case protocol.MachineStopping:
			s.stepStopping(ctx, r, m)
		case protocol.MachineDestroying:
			s.stepDestroying(ctx, r)
		default:
			return
		}
		s.kickScheduler()
		if after, err := s.store.Machine(ctx, id); err != nil || after.State == r.State {
			return
		}
	}
}

// creationDeadline is the provider-side bound of a new machine (design, section 7,
// deadlines): long enough for creation, the tool scripts, a full lease, and
// the warm time.
func (s *Server) creationDeadline(m *config.Template) time.Time {
	d := 10 * time.Minute
	if p := s.config().Providers[m.Provider]; p.Timeouts != nil {
		d = p.Timeouts.Provision.D()
	}
	var scripts time.Duration
	for _, t := range s.config().DefinedTools(m) {
		scripts += t.ScriptTimeout(config.StepInstall) + t.ScriptTimeout(config.StepCheck) + t.ScriptTimeout(config.StepStart)
	}
	return time.Now().Add(d + scripts + m.MaxLease.D() + m.WarmRunning.D() + 10*time.Minute)
}

// stepCreating calls the provider (idempotent by name). Other errors are retried
// with backoff, three attempts in total.
func (s *Server) stepCreating(ctx context.Context, r store.Machine, m *config.Template) {
	p := s.providerMap()[r.Provider]
	deadline := s.creationDeadline(m)
	req := provider.ProvisionRequest{Name: r.Name, Template: r.Template, Spec: m.Spec, Deadline: deadline,
		ServerKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.serverKey.PublicKey())))}
	var ref provider.MachineRef
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		ref, err = p.Provision(ctx, req)
		if err == nil || errors.Is(err, provider.ErrLimit) || ctx.Err() != nil {
			break
		}
		s.log.Warn("provider provision failed", "machine_id", r.ID, "attempt", attempt, "err", err)
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
			s.log.Error("record the provider ID", "machine_id", r.ID, "err", err)
			return
		}
		s.store.SetMachineState(ctx, r.ID, []string{protocol.MachineCreating}, protocol.MachineProvisioning, "", time.Now())
		return
	}
	// The machine never existed at the provider: remove anything with its name.
	dctx, cancel := context.WithTimeout(ctx, providerCallTimeout)
	if derr := p.Destroy(dctx, s.ref(r)); derr != nil && !errors.Is(derr, provider.ErrNotFound) {
		s.log.Warn("destroy a failed machine by name", "machine_id", r.ID, "err", derr)
	}
	cancel()
	s.store.SetMachineState(ctx, r.ID, []string{protocol.MachineCreating}, protocol.MachineDestroyed, err.Error(), time.Now())
	leases := s.machineLeases(ctx, r.ID)
	if errors.Is(err, provider.ErrLimit) {
		s.full.set(m.Key())
		for _, l := range leases {
			s.store.Requeue(ctx, l.ID, []string{protocol.LeaseAssigned, protocol.LeaseProvisioning}, "provider limit reached",
				false, maxAttempts, protocol.RoomFree, "", time.Now())
		}
		// Limits that Hostel cannot count (for example vCPUs): make space with
		// an idle Hostel machine of this provider, at once.
		s.evictIdle(ctx, r.Provider)
		return
	}
	for _, l := range leases {
		s.store.FailLease(ctx, l.ID, []string{protocol.LeaseAssigned, protocol.LeaseProvisioning},
			"the provider failed to create a machine: "+err.Error(), protocol.RoomFree, "", time.Now())
	}
}

// machineLeases returns the active leases on a machine.
func (s *Server) machineLeases(ctx context.Context, machineID int64) []store.Lease {
	leases, err := s.store.ActiveLeases(ctx)
	if err != nil {
		return nil
	}
	var out []store.Lease
	for _, l := range leases {
		if l.MachineID == machineID {
			out = append(out, l)
		}
	}
	return out
}

// evictIdle destroys the oldest idle Hostel machine of a provider.
func (s *Server) evictIdle(ctx context.Context, providerName string) {
	machines, err := s.store.Machines(ctx)
	if err != nil {
		return
	}
	idle, err := s.idleMachines(ctx, machines)
	if err != nil {
		return
	}
	for _, r := range machines {
		if r.Provider == providerName && idle[r.ID] {
			s.store.SetMachineState(ctx, r.ID, []string{protocol.MachineRunning, protocol.MachineStopped}, protocol.MachineDestroying,
				"evicted after a provider limit error", time.Now())
			return
		}
	}
}

// idleMachines returns the on-demand machines with no lease: running with all rooms
// free, or stopped.
func (s *Server) idleMachines(ctx context.Context, machines []store.Machine) (map[int64]bool, error) {
	rooms, err := s.store.Rooms(ctx)
	if err != nil {
		return nil, err
	}
	busy := map[int64]bool{}
	for _, sl := range rooms {
		if sl.State != protocol.RoomFree {
			busy[sl.MachineID] = true
		}
	}
	out := map[int64]bool{}
	for _, r := range machines {
		m := s.config().Template(r.Provider + "/" + r.Template)
		if m == nil || !m.OnDemand() || busy[r.ID] {
			continue
		}
		if r.State == protocol.MachineStopped || r.State == protocol.MachineRunning {
			out[r.ID] = true
		}
	}
	return out, nil
}

// stepMachineProvisioning probes and uploads, cleans dirty rooms, installs and
// checks the tools, and sets the machine to running. On-demand machines install a
// screen tool when a lease first uses it.
func (s *Server) stepMachineProvisioning(ctx context.Context, r store.Machine, m *config.Template) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout+miseInstallTimeout+s.maxProvisionTimeout()+s.cleanupScriptsTimeout())
	defer cancel()
	hash := templateHash(s.live.Load(), m)
	err := s.doProvision(ctx, r, m)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		s.log.Warn("machine provisioning failed", "machine_id", r.ID, "err", err)
		s.machineFailed(ctx, r, m, err)
		return
	}
	if ok, _ := s.store.SetMachineState(ctx, r.ID, []string{protocol.MachineProvisioning}, protocol.MachineRunning, "", time.Now()); ok {
		s.store.SetWarmUntil(ctx, r.ID, time.Time{})
		s.store.SetConfigSHA256(ctx, r.ID, hash)
		s.store.SetMachineReady(ctx, r.ID, time.Now())
	}
}

// machineFailed applies the failure rules of design section 6 to a machine that
// failed outside a lease step: a static machine is broken (and recovers by itself
// when it was only unreachable); an on-demand machine is destroyed. The leases
// that wait for it go back to the queue.
func (s *Server) machineFailed(ctx context.Context, r store.Machine, m *config.Template, err error) {
	reason := err.Error()
	unreachable := errors.Is(err, machineops.ErrUnreachable)
	switch {
	case unreachable:
		reason = reasonUnreachable
	case errors.Is(err, machineops.ErrHostKeyChanged):
		reason = reasonHostKeyChanged
	}
	to := protocol.MachineBroken
	if m.OnDemand() {
		to = protocol.MachineDestroying
	}
	s.store.SetMachineState(ctx, r.ID, []string{r.State}, to, reason, time.Now())
	// An unreachable static machine is not an attempt; the other failures are.
	attempt := !(unreachable && !m.OnDemand())
	for _, l := range s.machineLeases(ctx, r.ID) {
		s.store.Requeue(ctx, l.ID, []string{protocol.LeaseAssigned, protocol.LeaseProvisioning}, "machine failed: "+reason,
			attempt, maxAttempts, protocol.RoomFree, "", time.Now())
	}
}

func (s *Server) stepResuming(ctx context.Context, r store.Machine) {
	cctx, cancel := context.WithTimeout(ctx, providerCallTimeout)
	err := s.providerMap()[r.Provider].Resume(cctx, s.ref(r))
	cancel()
	switch {
	case ctx.Err() != nil:
	case err == nil:
		s.store.SetMachineState(ctx, r.ID, []string{protocol.MachineResuming}, protocol.MachineProvisioning, "", time.Now())
	case errors.Is(err, provider.ErrNotFound):
		s.markLost(ctx, r, "the provider no longer has the machine")
	default:
		s.log.Warn("resume failed", "machine_id", r.ID, "err", err)
		m, _ := s.templateOf(r)
		s.machineFailed(ctx, r, m, err)
	}
}

func (s *Server) stepStopping(ctx context.Context, r store.Machine, m *config.Template) {
	cctx, cancel := context.WithTimeout(ctx, providerCallTimeout)
	err := s.providerMap()[r.Provider].Stop(cctx, s.ref(r))
	cancel()
	switch {
	case ctx.Err() != nil:
	case err == nil:
		if ok, _ := s.store.SetMachineState(ctx, r.ID, []string{protocol.MachineStopping}, protocol.MachineStopped, "", time.Now()); ok {
			s.store.SetWarmUntil(ctx, r.ID, time.Now().Add(m.WarmStopped.D()))
		}
	case errors.Is(err, provider.ErrNotFound):
		s.markLost(ctx, r, "the provider no longer has the machine")
	default:
		s.log.Warn("stop failed; destroy the machine", "machine_id", r.ID, "err", err)
		s.store.SetMachineState(ctx, r.ID, []string{protocol.MachineStopping}, protocol.MachineDestroying, "stop failed: "+err.Error(), time.Now())
	}
}

func (s *Server) stepDestroying(ctx context.Context, r store.Machine) {
	cctx, cancel := context.WithTimeout(ctx, providerCallTimeout)
	err := s.providerMap()[r.Provider].Destroy(cctx, s.ref(r))
	cancel()
	if ctx.Err() != nil {
		return
	}
	if err != nil && !errors.Is(err, provider.ErrNotFound) {
		s.log.Warn("destroy failed; the next loop pass tries again", "machine_id", r.ID, "err", err)
		s.store.AddEvent(ctx, time.Now(), "", r.ID, "provider", "destroy failed: "+err.Error())
		return
	}
	s.markLost(ctx, r, "")
}

// markLost sets a machine to destroyed and ends or requeues its leases: ready and
// cleaning leases fail with reason (cleanup is not possible); leases before
// ready go back to the queue. A destroyed Hostel machine clears the full flags of
// its provider.
func (s *Server) markLost(ctx context.Context, r store.Machine, reason string) {
	from := []string{r.State}
	if r.State == protocol.MachineDestroyed {
		return
	}
	if ok, err := s.store.SetMachineState(ctx, r.ID, from, protocol.MachineDestroyed, reason, time.Now()); err != nil || !ok {
		return
	}
	s.full.clearProvider(r.Provider)
	failReason := "machine lost"
	if reason != "" {
		failReason += ": " + reason
	}
	for _, l := range s.machineLeases(ctx, r.ID) {
		switch l.State {
		case protocol.LeaseReady, protocol.LeaseCleaning:
			s.store.FailLease(ctx, l.ID, []string{l.State}, failReason, protocol.RoomFree, "", time.Now())
			s.gateway.CloseLease(l.ID)
		default:
			s.store.Requeue(ctx, l.ID, []string{protocol.LeaseAssigned, protocol.LeaseProvisioning, protocol.LeasePreparing},
				failReason, false, maxAttempts, protocol.RoomFree, "", time.Now())
		}
	}
}

// cleanDirtyRoom runs the cleanup of a room through machine, or as the room's own
// user.
func (s *Server) cleanDirtyRoom(ctx context.Context, machine *machineops.Machine, machineID int64, room string, req protocol.CleanupRequest) error {
	r, err := s.store.Machine(ctx, machineID)
	if err != nil {
		return err
	}
	if u, err := s.roomUserOf(ctx, r, room); err != nil {
		return err
	} else if u != "" {
		sr, _, err := s.openRoom(ctx, machineID, room)
		if err != nil {
			return err
		}
		defer sr.Conn.Close()
		machine = sr
	}
	return s.runCleanup(ctx, machine, machineID, room, req)
}

// cleanDirtyRooms runs the cleanup of each dirty room of a machine (design,
// section 4, "Dirty room").
func (s *Server) cleanDirtyRooms(ctx context.Context, machine *machineops.Machine, machineID int64) error {
	rooms, err := s.store.Rooms(ctx)
	if err != nil {
		return err
	}
	for _, sl := range rooms {
		if sl.MachineID != machineID || sl.State != protocol.RoomDirty {
			continue
		}
		var req protocol.CleanupRequest
		if err := json.Unmarshal([]byte(sl.CleanupData), &req); err != nil {
			return fmt.Errorf("room %s: cleanup data: %w", sl.Name, err)
		}
		if err := s.cleanDirtyRoom(ctx, machine, machineID, sl.Name, req); err != nil {
			return fmt.Errorf("cleanup of room %s: %w", sl.Name, err)
		}
		if err := s.store.SetRoom(ctx, machineID, sl.Name, protocol.RoomFree, "", ""); err != nil {
			return err
		}
	}
	return nil
}
