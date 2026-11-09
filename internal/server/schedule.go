package server

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/machineops"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/scheduler"
	"github.com/csansoon/hostel/internal/store"
)

// toolFiles returns the files of a tool directory that a machine with this OS
// gets: tool.toml and the scripts for the OS (design, section 8).
func toolFiles(t *config.Tool, osName string) []string {
	if t.Dir == "" || !t.AvailableOn(osName) {
		return nil
	}
	files := []string{config.ToolFile}
	for _, f := range t.Scripts[osName] {
		files = append(files, f)
	}
	sort.Strings(files)
	return files
}

// toolUploads hashes each tool directory for each OS.
func toolUploads(cfg *config.Server) (map[string]map[string]machineops.Tool, error) {
	out := map[string]map[string]machineops.Tool{}
	for name, t := range cfg.Tools {
		out[name] = map[string]machineops.Tool{}
		for _, osName := range config.OSNames {
			up, err := machineops.ToolUpload(t.Dir, toolFiles(t, osName), osName)
			if err != nil {
				return nil, err
			}
			out[name][osName] = up
		}
	}
	return out, nil
}

// machineReady returns what a machine is ready for: each tool of its machine whose
// defined tools (the tool and its needs) the machine has installed and checked
// with the current files. A mise tool is ready on each provisioned machine.
func machineReady(lc *liveConfig, rts []store.MachineTool, r store.Machine, m *config.Template) map[string]bool {
	have := map[string]bool{}
	for _, rt := range rts {
		if rt.MachineID == r.ID && rt.State == store.ToolReady && rt.ScriptsSHA256 == installHash(lc, m, rt.Tool) {
			have[rt.Tool] = true
		}
	}
	ready := map[string]bool{}
	for _, mt := range m.ToolList {
		ok := true
		for _, n := range lc.cfg.ToolClosure(mt.Name) {
			ok = ok && have[n]
		}
		ready[mt.Name] = ok
	}
	return ready
}

// setWaiting keeps the waiting line of the last scheduler pass, for the
// "waiting behind" of the lease answers.
func (s *Server) setWaiting(ws []scheduler.Waiting) {
	m := map[string]scheduler.Waiting{}
	for _, w := range ws {
		m[w.LeaseID] = w
	}
	s.waitingMu.Lock()
	s.waiting = m
	s.waitingMu.Unlock()
}

func (s *Server) waitingOf(id string) scheduler.Waiting {
	s.waitingMu.Lock()
	defer s.waitingMu.Unlock()
	return s.waiting[id]
}

func (s *Server) schedProviders() []scheduler.Provider {
	var out []scheduler.Provider
	for _, name := range s.config().ProviderOrder {
		p := s.config().Providers[name]
		sp := scheduler.Provider{Name: name}
		if p.MaxMachines != nil {
			sp.MaxMachines = *p.MaxMachines
		}
		for _, l := range p.Limits {
			var keys []string
			for _, mn := range l.Templates {
				keys = append(keys, name+"/"+mn)
			}
			sp.Limits = append(sp.Limits, scheduler.Limit{Templates: keys, MaxMachines: l.MaxMachines})
		}
		out = append(out, sp)
	}
	return out
}

// schedule runs one scheduler pass and applies its assignments.
func (s *Server) schedule(ctx context.Context) error {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	leases, err := s.store.ActiveLeases(ctx)
	if err != nil {
		return err
	}
	view, err := s.loadToolView(ctx)
	if err != nil {
		return err
	}
	lc, machines := view.lc, view.machines
	var queued []scheduler.Lease
	byID := map[string]store.Lease{}
	for _, l := range leases {
		if l.State != protocol.LeaseQueued {
			continue
		}
		byID[l.ID] = l
		queued = append(queued, scheduler.Lease{ID: l.ID, Priority: l.Priority, ManualPosition: l.ManualPosition, CreatedAt: l.CreatedAt,
			Request: leaseRequest(lc.cfg, l)})
	}
	if len(queued) == 0 {
		s.setWaiting(nil)
		return nil
	}
	rooms, err := s.store.Rooms(ctx)
	if err != nil {
		return err
	}
	machineByID := map[int64]store.Machine{}
	var smachines []scheduler.Machine
	for _, r := range machines {
		machineByID[r.ID] = r
		m := lc.cfg.Template(r.Provider + "/" + r.Template)
		if m == nil {
			continue
		}
		// A machine whose machine settings changed gets no new lease; it
		// provisions again when it is free (refreshMachines).
		stale := r.ConfigSHA256 != "" && r.ConfigSHA256 != templateHash(lc, m)
		sr := scheduler.Machine{ID: r.ID, Template: m.Key(), State: r.State, Drained: r.Drained || stale, CreatedAt: r.CreatedAt,
			Tools: view.machineHaves(m, r.ID), Ready: machineReady(lc, view.rts, r, m)}
		for _, sl := range rooms {
			if sl.MachineID == r.ID {
				sr.Rooms = append(sr.Rooms, scheduler.Room{Name: sl.Name, State: sl.State, PoolPorts: len(roomConfig(m, sl.Name).Ports)})
			}
		}
		smachines = append(smachines, sr)
	}
	now := time.Now()
	templates := view.schedTemplates()
	out := scheduler.Schedule(scheduler.Input{Now: now, Templates: templates,
		Providers: s.schedProviders(), Machines: smachines, Leases: queued, Full: s.full.current(lc.cfg.LimitRetry.D())})
	s.setWaiting(out.Waiting)
	for _, id := range out.Evict {
		s.log.Info("evict an idle machine to make space", "machine_id", id)
		if _, err := s.store.SetMachineState(ctx, id, []string{protocol.MachineRunning, protocol.MachineStopped}, protocol.MachineDestroying,
			"evicted to make space for a new machine", now); err != nil {
			return err
		}
	}
	for _, a := range out.Assignments {
		l := byID[a.LeaseID]
		m := lc.cfg.Template(a.Template)
		// The ports come first: a lease whose tunnels cannot be placed fails
		// before any machine is resumed or created for it.
		ports, err := tunnelPorts(l.Tunnels, roomConfig(m, a.Rooms[0]).Ports)
		if err != nil {
			if _, err := s.store.EndLease(ctx, l.ID, []string{protocol.LeaseQueued}, protocol.LeaseFailed, err.Error(), now); err != nil {
				return err
			}
			continue
		}
		var machine store.Machine
		if a.Action != scheduler.CreateMachine {
			machine = machineByID[a.MachineID]
			// Another lease on the machine can use a fixed machine port (machines with
			// several rooms). The lease waits; it does not get a port twice.
			if busy, err := s.fixedPortInUse(ctx, machine.ID, l); err != nil {
				return err
			} else if busy != 0 {
				if err := s.store.SetQueue(ctx, l.ID, max(l.Position, 1), fmt.Sprintf("machine port %d is in use by another lease on the machine", busy)); err != nil {
					return err
				}
				continue
			}
		}
		switch a.Action {
		case scheduler.ResumeMachine:
			ok, err := s.store.SetMachineState(ctx, machine.ID, []string{protocol.MachineStopped}, protocol.MachineResuming, "", now)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			s.store.SetWarmUntil(ctx, machine.ID, time.Time{})
		case scheduler.CreateMachine:
			// The machine row exists before the provider call, with the name that
			// the provider gets (design, section 7, ownership).
			machine, err = s.store.InsertMachine(ctx, store.Machine{Provider: m.Provider, Template: m.Name, State: protocol.MachineCreating, OS: m.OS},
				s.config().ServerID, roomNames(m), now)
			if err != nil {
				return err
			}
		}
		ok, err := s.store.Assign(ctx, l.ID, machine, a.Rooms, ports, now)
		if err != nil {
			return err
		}
		if ok {
			s.kickScheduler()
		}
	}
	for _, w := range out.Waiting {
		reason := w.Reason
		if reason == scheduler.ReasonNoRoom {
			reason = brokenReason(scheduler.Candidates(templates, leaseRequest(lc.cfg, byID[w.LeaseID])), machines, reason)
		}
		if err := s.store.SetQueue(ctx, w.LeaseID, w.Position, reason); err != nil {
			return err
		}
	}
	return nil
}

func roomConfig(m *config.Template, name string) config.Room {
	for _, sl := range m.Rooms {
		if sl.Name == name {
			return sl
		}
	}
	return config.Room{}
}

// tunnelPorts gives each tunnel its machine port: a fixed port stays; the others
// get the lowest free pool ports, in name order.
func tunnelPorts(tunnels []protocol.TunnelRequest, pool []int) (map[string]int, error) {
	out := map[string]int{}
	used := map[int]bool{}
	var names []string
	for _, t := range tunnels {
		if t.MachinePort != 0 {
			out[t.Name] = t.MachinePort
			used[t.MachinePort] = true
		} else {
			names = append(names, t.Name)
		}
	}
	sort.Strings(names)
	free := slices.Clone(pool)
	sort.Ints(free)
	for _, n := range names {
		for len(free) > 0 && used[free[0]] {
			free = free[1:]
		}
		if len(free) == 0 {
			return nil, fmt.Errorf("tunnel %s: no free pool port, because fixed machine ports use pool ports; use other fixed ports", n)
		}
		out[n] = free[0]
		used[free[0]] = true
		free = free[1:]
	}
	return out, nil
}

// brokenReason tells a waiting guest why: if every machine of its candidate
// machines is broken, the reason names the machine's broken reason (R7); if all
// of them are offline, it says that the lease waits for one of them.
func brokenReason(cands []string, machines []store.Machine, fallback string) string {
	reason := ""
	var offline []string
	for _, r := range machines {
		if !slices.Contains(cands, r.Provider+"/"+r.Template) || r.State == protocol.MachineDestroyed || r.State == protocol.MachineRetired {
			continue
		}
		if r.State != protocol.MachineBroken {
			return fallback
		}
		if r.BrokenReason == reasonUnreachable {
			offline = append(offline, r.Name)
		} else if reason == "" {
			reason = "machine broken: " + r.BrokenReason
		}
	}
	switch {
	case reason != "":
		return reason
	case len(offline) > 0:
		return "every machine with these tools is offline (" + strings.Join(offline, ", ") + "); the lease waits until one is back"
	}
	return fallback
}

// fixedPortInUse returns a fixed machine port of the lease that another active
// lease on the machine uses, or 0.
func (s *Server) fixedPortInUse(ctx context.Context, machineID int64, l store.Lease) (int, error) {
	others, err := s.store.MachineTunnelPorts(ctx, machineID, l.ID)
	if err != nil {
		return 0, err
	}
	for _, t := range l.Tunnels {
		if t.MachinePort != 0 && others[t.MachinePort] {
			return t.MachinePort, nil
		}
	}
	return 0, nil
}
