package server

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/provider"
	"github.com/csansoon/hostel/internal/scheduler"
	"github.com/csansoon/hostel/internal/store"
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
	interval := s.config().Intervals.Scheduler.D()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := s.pass(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("loop pass", "err", err)
		}
		if d := s.config().Intervals.Scheduler.D(); d != interval {
			interval = d
			ticker.Reset(d)
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
	if now := time.Now(); now.Sub(s.lastReconcile) >= s.config().Intervals.Reconcile.D() {
		s.lastReconcile = now
		if err := s.reconcile(ctx); err != nil {
			s.log.Error("reconcile", "err", err)
		}
	}
	if err := s.checkPresence(ctx); err != nil {
		return fmt.Errorf("presence: %w", err)
	}
	if err := s.refreshMachines(ctx); err != nil {
		return fmt.Errorf("machines after a configuration change: %w", err)
	}
	if err := s.warm(ctx); err != nil {
		return fmt.Errorf("warm policy: %w", err)
	}
	if err := s.schedule(ctx); err != nil {
		return fmt.Errorf("schedule: %w", err)
	}
	machines, err := s.store.Machines(ctx)
	if err != nil {
		return err
	}
	for _, r := range machines {
		switch r.State {
		case protocol.MachineCreating, protocol.MachineProvisioning, protocol.MachineResuming, protocol.MachineStopping, protocol.MachineDestroying:
			id := r.ID
			s.workers.start(ctx, fmt.Sprintf("machine:%d", id), func(ctx context.Context) { s.runMachine(ctx, id) }, s.kickScheduler)
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

var machineNameRE = regexp.MustCompile(`^hostel-([a-z0-9]+)-([0-9]+)$`)

// reconcile (design, section 7) compares each on-demand provider's machines with
// the database (static machines that were unreachable come back through the
// presence check, presence.go): unknown machines
// with this server's prefix are destroyed; database machines that the provider
// lost are marked destroyed. It never acts on machines without the prefix.
func (s *Server) reconcile(ctx context.Context) error {
	// One configuration for the whole pass.
	lc := s.live.Load()
	machines, err := s.store.Machines(ctx)
	if err != nil {
		return err
	}
	prefix := "hostel-" + lc.cfg.ServerID + "-"
	for _, name := range lc.cfg.ProviderOrder {
		switch lc.cfg.Providers[name].Kind {
		case config.ProviderStatic:
			continue
		case config.ProviderAzure:
			s.reconcilePower(ctx, name, lc.providers[name], machines)
			continue
		}
		p := lc.providers[name]
		lctx, cancel := context.WithTimeout(ctx, providerCallTimeout)
		listed, err := p.List(lctx, prefix)
		cancel()
		if err != nil {
			s.log.Warn("reconcile: list machines", "provider", name, "err", err)
			continue
		}
		seen := map[int64]bool{}
		for _, ref := range listed {
			m := machineNameRE.FindStringSubmatch(ref.Name)
			if m == nil || m[1] != lc.cfg.ServerID {
				continue
			}
			id, _ := strconv.ParseInt(m[2], 10, 64)
			seen[id] = true
			r, err := s.store.Machine(ctx, id)
			if err == nil && r.State != protocol.MachineDestroyed && r.Provider == name {
				// A second provider machine with the name, for example from a
				// create call of a killed server that ended late. Provider IDs
				// are set only at creation.
				if r.State == protocol.MachineCreating || ref.ID == "" || r.ProviderRef == "" || ref.ID == r.ProviderRef {
					continue
				}
				s.log.Warn("reconcile: destroy a second provider machine of a machine", "provider", name, "machine", ref.Name, "id", ref.ID)
			} else {
				s.log.Warn("reconcile: destroy a machine that the database does not track", "provider", name, "machine", ref.Name)
			}
			dctx, cancel := context.WithTimeout(ctx, providerCallTimeout)
			if err := p.Destroy(dctx, ref); err != nil && !errors.Is(err, provider.ErrNotFound) {
				s.log.Warn("reconcile: destroy", "machine", ref.Name, "err", err)
			}
			cancel()
		}
		for _, r := range machines {
			if r.Provider != name || seen[r.ID] || r.State == protocol.MachineCreating {
				continue
			}
			s.log.Warn("reconcile: the provider lost a machine", "machine_id", r.ID, "machine", r.Name)
			s.markLost(ctx, r, "the provider no longer lists it")
		}
	}
	return nil
}

// warm applies the warm policy (design, section 17): an idle on-demand machine
// stops (or is destroyed) after warm_running; a stopped machine is destroyed after
// warm_stopped; idle machines that are broken or have too little lifetime left
// are destroyed.
func (s *Server) warm(ctx context.Context) error {
	machines, err := s.store.Machines(ctx)
	if err != nil {
		return err
	}
	idle, err := s.idleMachines(ctx, machines)
	if err != nil {
		return err
	}
	rooms, err := s.store.Rooms(ctx)
	if err != nil {
		return err
	}
	// A machine is busy while a room has an active lease. A broken room keeps the
	// ID of its ended lease, so lease_id alone does not count.
	busy := map[int64]bool{}
	for _, sl := range rooms {
		switch sl.State {
		case protocol.RoomAssigned, protocol.RoomPreparing, protocol.RoomLeased, protocol.RoomCleaning:
			busy[sl.MachineID] = true
		}
	}
	dirty := map[int64]bool{}
	for _, sl := range rooms {
		if sl.State == protocol.RoomDirty {
			dirty[sl.MachineID] = true
		}
	}
	leases, err := s.store.ActiveLeases(ctx)
	if err != nil {
		return err
	}
	// waitsFor reports whether a queued lease can use m, by the rules of the
	// scheduler (tools, pool ports, fresh).
	var view *toolView
	waitsFor := func(m *config.Template) bool {
		if view == nil {
			if view, err = s.loadToolView(ctx); err != nil {
				view = nil
				return false
			}
		}
		templates := view.schedTemplates()
		return slices.ContainsFunc(leases, func(l store.Lease) bool {
			return l.State == protocol.LeaseQueued && slices.Contains(scheduler.Candidates(templates, leaseRequest(view.lc.cfg, l)), m.Key())
		})
	}
	now := time.Now()
	for _, r := range machines {
		m := s.config().Template(r.Provider + "/" + r.Template)
		if m != nil && m.PowerManaged() {
			// A deallocated machine with a dirty room takes no lease until the
			// cleanup ran on it: start it when a lease waits for it. (If it
			// does not answer, it stays broken; see warmPowerManaged.)
			if r.State == protocol.MachineStopped && dirty[r.ID] && !r.Drained && waitsFor(m) {
				s.schedMu.Lock()
				s.store.SetMachineState(ctx, r.ID, []string{protocol.MachineStopped}, protocol.MachineResuming, "started to clean a dirty room", now)
				s.schedMu.Unlock()
				continue
			}
			s.warmPowerManaged(ctx, r, m, busy[r.ID], now)
			continue
		}
		if m == nil || !m.OnDemand() {
			continue
		}
		switch {
		case r.State == protocol.MachineBroken && !busy[r.ID]:
			s.store.SetMachineState(ctx, r.ID, []string{protocol.MachineBroken}, protocol.MachineDestroying, r.BrokenReason, now)
		case r.State == protocol.MachineRunning && idle[r.ID] && r.Drained && !m.Reused():
			s.store.SetMachineState(ctx, r.ID, []string{protocol.MachineRunning}, protocol.MachineDestroying, "reuse = false: destroyed after its lease", now)
		case r.State == protocol.MachineRunning && !idle[r.ID]:
			if !r.WarmUntil.IsZero() {
				s.store.SetWarmUntil(ctx, r.ID, time.Time{})
			}
		case r.State == protocol.MachineRunning && m.MaxLifetime.D() > 0 &&
			r.CreatedAt.Add(m.MaxLifetime.D()).Sub(now) < m.MaxLease.D()+5*time.Minute:
			s.store.SetMachineState(ctx, r.ID, []string{protocol.MachineRunning}, protocol.MachineDestroying, "too little lifetime left for a lease", now)
		case r.State == protocol.MachineRunning && r.WarmUntil.IsZero():
			s.store.SetWarmUntil(ctx, r.ID, now.Add(m.WarmRunning.D()))
		case r.State == protocol.MachineRunning && now.After(r.WarmUntil):
			to := protocol.MachineDestroying
			if s.providerMap()[r.Provider].Capabilities(r.Template).CanStop {
				to = protocol.MachineStopping
			}
			s.store.SetMachineState(ctx, r.ID, []string{protocol.MachineRunning}, to, "idle for warm_running", now)
		case r.State == protocol.MachineStopped && !r.WarmUntil.IsZero() && now.After(r.WarmUntil):
			s.store.SetMachineState(ctx, r.ID, []string{protocol.MachineStopped}, protocol.MachineDestroying, "stopped for warm_stopped", now)
		}
	}
	return nil
}

// warmPowerManaged deallocates a machine that Hostel did not create (azure)
// after warm_running without a lease: a running one, and a broken one (it
// takes no lease and still costs money; a failed stop is tried again this
// way). A machine broken for another reason than unreachable or a failed stop
// stays broken after the deallocation, until a repair. It never destroys the
// machine.
func (s *Server) warmPowerManaged(ctx context.Context, r store.Machine, m *config.Template, busy bool, now time.Time) {
	if r.State != protocol.MachineRunning && r.State != protocol.MachineBroken {
		return
	}
	reason := "idle for warm_running"
	if r.State == protocol.MachineBroken && !powerRetryStop(r.BrokenReason) {
		if powerDeallocated(r.BrokenReason) {
			return
		}
		reason = reasonKeepBroken + r.BrokenReason
	}
	switch {
	case busy:
		if !r.WarmUntil.IsZero() {
			s.store.SetWarmUntil(ctx, r.ID, time.Time{})
		}
	case r.WarmUntil.IsZero():
		s.store.SetWarmUntil(ctx, r.ID, now.Add(m.WarmRunning.D()))
	case now.After(r.WarmUntil) && r.State == protocol.MachineBroken && unreachableReason(r.BrokenReason):
		// The reconcile knows whether it runs (then it deallocates it, and it
		// stays broken) or is deallocated already (then it is stopped).
	case now.After(r.WarmUntil):
		if ok, _ := s.store.SetMachineState(ctx, r.ID, []string{r.State}, protocol.MachineStopping, reason, now); ok {
			s.store.SetWarmUntil(ctx, r.ID, time.Time{})
		}
	}
}

// reconcilePower follows the power state of the machines of a provider that
// Hostel did not create (azure), when someone changed it outside Hostel: a
// machine that runs is probed, and a deallocated one without a lease is
// stopped. Rows in a transitional state belong to their worker.
func (s *Server) reconcilePower(ctx context.Context, name string, p provider.Provider, machines []store.Machine) {
	// The reconcile runs in the loop pass: a short deadline.
	lctx, cancel := context.WithTimeout(ctx, time.Minute)
	listed, err := p.List(lctx, "")
	cancel()
	if err != nil {
		s.log.Warn("reconcile: read the power state", "provider", name, "err", err)
		return
	}
	power := map[string]string{}
	for _, ref := range listed {
		power[ref.Template] = ref.State
	}
	rooms, err := s.store.Rooms(ctx)
	if err != nil {
		return
	}
	busy := map[int64]bool{}
	for _, sl := range rooms {
		switch sl.State {
		case protocol.RoomAssigned, protocol.RoomPreparing, protocol.RoomLeased, protocol.RoomCleaning:
			busy[sl.MachineID] = true
		}
	}
	// A configuration change checks the machine states under schedMu (a
	// removed azure machine must be deallocated).
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	now := time.Now()
	for _, r := range machines {
		if r.Provider != name {
			continue
		}
		broken := r.State == protocol.MachineBroken
		m := s.config().Template(r.Provider + "/" + r.Template)
		switch state := power[r.Template]; {
		case state == provider.PowerRunning && (r.State == protocol.MachineStopped || broken && strings.HasPrefix(r.BrokenReason, reasonStartFailed)):
			s.store.SetMachineState(ctx, r.ID, []string{r.State}, protocol.MachineProvisioning, "", now)
		case state == provider.PowerRunning && broken && unreachableReason(r.BrokenReason) && !busy[r.ID] && m != nil &&
			!r.WarmUntil.IsZero() && now.After(r.WarmUntil) && now.After(r.StateChangedAt.Add(m.WarmRunning.D())):
			// It runs but did not answer for warm_running (counted from when
			// it became broken, not from the start of its idle time: a
			// reboot is not a hang): deallocated, and broken until a repair
			// (else each waiting lease would start it).
			if ok, _ := s.store.SetMachineState(ctx, r.ID, []string{r.State}, protocol.MachineStopping, reasonKeepBroken+reasonUnreachable, now); ok {
				s.store.SetWarmUntil(ctx, r.ID, time.Time{})
			}
		case state == provider.PowerRunning && broken && strings.HasPrefix(r.BrokenReason, reasonDeallocated):
			// Started outside Hostel while broken: warm_running deallocates it again.
			s.store.SetMachineState(ctx, r.ID, []string{r.State}, protocol.MachineBroken, strings.TrimPrefix(r.BrokenReason, reasonDeallocated), now)
		case state == provider.PowerStopped && !busy[r.ID] && (r.State == protocol.MachineRunning || broken && powerRecoverable(r.BrokenReason)):
			if ok, _ := s.store.SetMachineState(ctx, r.ID, []string{r.State}, protocol.MachineStopped, "deallocated outside Hostel", now); ok {
				s.store.SetWarmUntil(ctx, r.ID, time.Time{})
			}
		case state == provider.PowerOff && !busy[r.ID] && (r.State == protocol.MachineStopped || r.State == protocol.MachineRunning || broken) &&
			!strings.Contains(r.BrokenReason, reasonStopFailed): // a failed stop is tried again after warm_running
			// Stopped but not deallocated (for example shut down inside the
			// OS): Azure still bills it.
			reason := "stopped in Azure but not deallocated"
			if broken && !powerRecoverable(r.BrokenReason) {
				reason = reasonKeepBroken + strings.TrimPrefix(r.BrokenReason, reasonDeallocated)
			}
			s.store.SetMachineState(ctx, r.ID, []string{r.State}, protocol.MachineStopping, reason, now)
		}
	}
}
