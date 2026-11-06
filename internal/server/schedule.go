package server

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/roomops"
	"github.com/csansoon/hostel/internal/scheduler"
	"github.com/csansoon/hostel/internal/store"
)

// toolFiles returns the files of a tool directory that a room with this OS
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
func toolUploads(cfg *config.Server) (map[string]map[string]roomops.Tool, error) {
	out := map[string]map[string]roomops.Tool{}
	for name, t := range cfg.Tools {
		out[name] = map[string]roomops.Tool{}
		for _, osName := range config.OSNames {
			up, err := roomops.ToolUpload(t.Dir, toolFiles(t, osName), osName)
			if err != nil {
				return nil, err
			}
			out[name][osName] = up
		}
	}
	return out, nil
}

// roomReady returns what a room is ready for: each tool of its machine whose
// defined tools (the tool and its needs) the room has installed and checked
// with the current files. A mise tool is ready on each provisioned room.
func roomReady(lc *liveConfig, rts []store.RoomTool, r store.Room, m *config.Machine) map[string]bool {
	have := map[string]bool{}
	for _, rt := range rts {
		if rt.RoomID == r.ID && rt.State == store.ToolReady && rt.ScriptsSHA256 == installHash(lc, m, rt.Tool) {
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
		if p.MaxRooms != nil {
			sp.MaxRooms = *p.MaxRooms
		}
		for _, l := range p.Limits {
			var keys []string
			for _, mn := range l.Machines {
				keys = append(keys, name+"/"+mn)
			}
			sp.Limits = append(sp.Limits, scheduler.Limit{Machines: keys, MaxRooms: l.MaxRooms})
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
	lc, rooms := view.lc, view.rooms
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
	slots, err := s.store.Slots(ctx)
	if err != nil {
		return err
	}
	roomByID := map[int64]store.Room{}
	var srooms []scheduler.Room
	for _, r := range rooms {
		roomByID[r.ID] = r
		m := lc.cfg.Machine(r.Provider + "/" + r.Machine)
		if m == nil {
			continue
		}
		// A room whose machine settings changed gets no new lease; it
		// provisions again when it is free (refreshRooms).
		stale := r.ConfigSHA256 != "" && r.ConfigSHA256 != machineHash(lc, m)
		sr := scheduler.Room{ID: r.ID, Machine: m.Key(), State: r.State, Drained: r.Drained || stale, CreatedAt: r.CreatedAt,
			Tools: view.roomHaves(m, r.ID), Ready: roomReady(lc, view.rts, r, m)}
		for _, sl := range slots {
			if sl.RoomID == r.ID {
				sr.Slots = append(sr.Slots, scheduler.Slot{Name: sl.Name, State: sl.State, PoolPorts: len(slotConfig(m, sl.Name).Ports)})
			}
		}
		srooms = append(srooms, sr)
	}
	now := time.Now()
	machines := view.schedMachines()
	out := scheduler.Schedule(scheduler.Input{Now: now, Machines: machines,
		Providers: s.schedProviders(), Rooms: srooms, Leases: queued, Full: s.full.current(lc.cfg.LimitRetry.D())})
	s.setWaiting(out.Waiting)
	for _, id := range out.Evict {
		s.log.Info("evict an idle room to make space", "room_id", id)
		if _, err := s.store.SetRoomState(ctx, id, []string{protocol.RoomRunning, protocol.RoomStopped}, protocol.RoomDestroying,
			"evicted to make space for a new room", now); err != nil {
			return err
		}
	}
	for _, a := range out.Assignments {
		l := byID[a.LeaseID]
		m := lc.cfg.Machine(a.Machine)
		// The ports come first: a lease whose tunnels cannot be placed fails
		// before any room is resumed or created for it.
		ports, err := tunnelPorts(l.Tunnels, slotConfig(m, a.Slots[0]).Ports)
		if err != nil {
			if _, err := s.store.EndLease(ctx, l.ID, []string{protocol.LeaseQueued}, protocol.LeaseFailed, err.Error(), now); err != nil {
				return err
			}
			continue
		}
		var room store.Room
		if a.Action != scheduler.CreateRoom {
			room = roomByID[a.RoomID]
			// Another lease on the room can use a fixed room port (rooms with
			// several slots). The lease waits; it does not get a port twice.
			if busy, err := s.fixedPortInUse(ctx, room.ID, l); err != nil {
				return err
			} else if busy != 0 {
				if err := s.store.SetQueue(ctx, l.ID, max(l.Position, 1), fmt.Sprintf("room port %d is in use by another lease on the room", busy)); err != nil {
					return err
				}
				continue
			}
		}
		switch a.Action {
		case scheduler.ResumeRoom:
			ok, err := s.store.SetRoomState(ctx, room.ID, []string{protocol.RoomStopped}, protocol.RoomResuming, "", now)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			s.store.SetWarmUntil(ctx, room.ID, time.Time{})
		case scheduler.CreateRoom:
			// The room row exists before the provider call, with the name that
			// the provider gets (design, section 7, ownership).
			room, err = s.store.InsertRoom(ctx, store.Room{Provider: m.Provider, Machine: m.Name, State: protocol.RoomCreating, OS: m.OS},
				s.config().ServerID, slotNames(m), now)
			if err != nil {
				return err
			}
		}
		ok, err := s.store.Assign(ctx, l.ID, room, a.Slots, ports, now)
		if err != nil {
			return err
		}
		if ok {
			s.kickScheduler()
		}
	}
	for _, w := range out.Waiting {
		reason := w.Reason
		if reason == scheduler.ReasonNoSlot {
			reason = brokenReason(scheduler.Candidates(machines, leaseRequest(lc.cfg, byID[w.LeaseID])), rooms, reason)
		}
		if err := s.store.SetQueue(ctx, w.LeaseID, w.Position, reason); err != nil {
			return err
		}
	}
	return nil
}

func slotConfig(m *config.Machine, name string) config.Slot {
	for _, sl := range m.Slots {
		if sl.Name == name {
			return sl
		}
	}
	return config.Slot{}
}

// tunnelPorts gives each tunnel its room port: a fixed port stays; the others
// get the lowest free pool ports, in name order.
func tunnelPorts(tunnels []protocol.TunnelRequest, pool []int) (map[string]int, error) {
	out := map[string]int{}
	used := map[int]bool{}
	var names []string
	for _, t := range tunnels {
		if t.RoomPort != 0 {
			out[t.Name] = t.RoomPort
			used[t.RoomPort] = true
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
			return nil, fmt.Errorf("tunnel %s: no free pool port, because fixed room ports use pool ports; use other fixed ports", n)
		}
		out[n] = free[0]
		used[free[0]] = true
		free = free[1:]
	}
	return out, nil
}

// brokenReason tells a waiting guest why: if every room of its candidate
// machines is broken, the reason names the room's broken reason (R7); if all
// of them are offline, it says that the lease waits for one of them.
func brokenReason(cands []string, rooms []store.Room, fallback string) string {
	reason := ""
	var offline []string
	for _, r := range rooms {
		if !slices.Contains(cands, r.Provider+"/"+r.Machine) || r.State == protocol.RoomDestroyed || r.State == protocol.RoomRetired {
			continue
		}
		if r.State != protocol.RoomBroken {
			return fallback
		}
		if r.BrokenReason == reasonUnreachable {
			offline = append(offline, r.Name)
		} else if reason == "" {
			reason = "room broken: " + r.BrokenReason
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

// fixedPortInUse returns a fixed room port of the lease that another active
// lease on the room uses, or 0.
func (s *Server) fixedPortInUse(ctx context.Context, roomID int64, l store.Lease) (int, error) {
	others, err := s.store.RoomTunnelPorts(ctx, roomID, l.ID)
	if err != nil {
		return 0, err
	}
	for _, t := range l.Tunnels {
		if t.RoomPort != 0 && others[t.RoomPort] {
			return t.RoomPort, nil
		}
	}
	return 0, nil
}
