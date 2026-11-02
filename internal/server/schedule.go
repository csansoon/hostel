package server

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sort"
	"time"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/roomops"
	"github.com/csansoon/hostel/internal/scheduler"
	"github.com/csansoon/hostel/internal/store"
)

// platformFiles returns the files of a platform directory that a room with
// this OS gets. An OS platform without scripts for this OS gets nothing.
func platformFiles(p *config.Platform, osName string) []string {
	if p.Dir == "" || (p.Kind == config.KindOS && len(p.Scripts[osName]) == 0) {
		return nil
	}
	var files []string
	if fileExists(p.Dir + "/platform.toml") {
		files = append(files, "platform.toml")
	}
	for _, f := range p.Scripts[osName] {
		files = append(files, f)
	}
	sort.Strings(files)
	return files
}

// platformUploads hashes each platform directory for each OS.
func platformUploads(cfg *config.Server) (map[string]map[string]roomops.Platform, error) {
	out := map[string]map[string]roomops.Platform{}
	for name, p := range cfg.Platforms {
		out[name] = map[string]roomops.Platform{}
		for _, osName := range config.OSNames {
			up, err := roomops.PlatformUpload(p.Dir, platformFiles(p, osName), osName)
			if err != nil {
				return nil, err
			}
			out[name][osName] = up
		}
	}
	return out, nil
}

func (s *Server) schedMachines() []scheduler.Machine {
	var out []scheduler.Machine
	lc := s.live.Load()
	for _, m := range lc.cfg.Machines() {
		pool := -1
		for _, sl := range m.Slots {
			if pool < 0 || len(sl.Ports) < pool {
				pool = len(sl.Ports)
			}
		}
		out = append(out, scheduler.Machine{
			Key: m.Key(), Provider: m.Provider, OnDemand: m.OnDemand(), OS: m.OS, Platforms: m.Platforms,
			CanResume: lc.providers[m.Provider].Capabilities(m.Name).CanStop,
			Labels:    m.Labels, PoolPorts: pool, Slots: slotNames(m), MaxRooms: *m.MaxRooms,
			MaxLease: m.MaxLease.D(), MaxLifetime: m.MaxLifetime.D(),
		})
	}
	return out
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
	var queued []scheduler.Lease
	byID := map[string]store.Lease{}
	for _, l := range leases {
		if l.State != protocol.LeaseQueued {
			continue
		}
		p := s.config().Platforms[l.Platform]
		if p == nil {
			continue // removed while the lease was queued; it waits for the platform
		}
		byID[l.ID] = l
		queued = append(queued, scheduler.Lease{ID: l.ID, Priority: l.Priority, ManualPosition: l.ManualPosition, CreatedAt: l.CreatedAt,
			Request: scheduler.Request{Platform: l.Platform, PlatformOS: p.Kind == config.KindOS, Labels: l.RequiredLabels,
				PoolTunnels: poolTunnels(l.Tunnels), Fresh: l.Fresh, Exclusive: exclusive(p, l.RequiredLabels)}})
	}
	if len(queued) == 0 {
		return nil
	}
	rooms, err := s.store.Rooms(ctx)
	if err != nil {
		return err
	}
	slots, err := s.store.Slots(ctx)
	if err != nil {
		return err
	}
	rps, err := s.store.RoomPlatforms(ctx)
	if err != nil {
		return err
	}
	roomByID := map[int64]store.Room{}
	var srooms []scheduler.Room
	lc := s.live.Load()
	for _, r := range rooms {
		roomByID[r.ID] = r
		m := lc.cfg.Machine(r.Provider + "/" + r.Machine)
		if m == nil {
			continue
		}
		// A room whose machine settings changed gets no new lease; it
		// provisions again when it is free (refreshRooms).
		stale := r.ConfigSHA256 != "" && r.ConfigSHA256 != machineHash(lc, m)
		sr := scheduler.Room{ID: r.ID, Machine: m.Key(), State: r.State, Drained: r.Drained || stale, CreatedAt: r.CreatedAt, Ready: map[string]bool{}}
		for _, rp := range rps {
			if rp.RoomID == r.ID && rp.ScriptsSHA256 == s.platformUploads()[rp.Platform][m.OS].Hash {
				sr.Ready[rp.Platform] = true
			}
		}
		for _, sl := range slots {
			if sl.RoomID == r.ID {
				sr.Slots = append(sr.Slots, scheduler.Slot{Name: sl.Name, State: sl.State, PoolPorts: len(slotConfig(m, sl.Name).Ports)})
			}
		}
		srooms = append(srooms, sr)
	}
	now := time.Now()
	out := scheduler.Schedule(scheduler.Input{Now: now, Machines: s.schedMachines(),
		Providers: s.schedProviders(), Rooms: srooms, Leases: queued, Full: s.full.current(s.config().LimitRetry.D())})
	for _, id := range out.Evict {
		s.log.Info("evict an idle room to make space", "room_id", id)
		if _, err := s.store.SetRoomState(ctx, id, []string{protocol.RoomRunning, protocol.RoomStopped}, protocol.RoomDestroying,
			"evicted to make space for a new room", now); err != nil {
			return err
		}
	}
	for _, a := range out.Assignments {
		l := byID[a.LeaseID]
		m := s.config().Machine(a.Machine)
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
			reason = s.brokenReason(byID[w.LeaseID], rooms, reason)
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

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// brokenReason tells a waiting guest why: if every room of its candidate
// machines is broken, the reason names the room's broken reason (R7).
func (s *Server) brokenReason(l store.Lease, rooms []store.Room, fallback string) string {
	p := s.config().Platforms[l.Platform]
	if p == nil {
		return fmt.Sprintf("the platform %s was removed from the server", l.Platform)
	}
	cands := scheduler.Candidates(s.schedMachines(), scheduler.Request{Platform: l.Platform, PlatformOS: p.Kind == config.KindOS,
		Labels: l.RequiredLabels, PoolTunnels: poolTunnels(l.Tunnels), Fresh: l.Fresh})
	reason := ""
	for _, r := range rooms {
		if !slices.Contains(cands, r.Provider+"/"+r.Machine) {
			continue
		}
		if r.State != protocol.RoomBroken {
			return fallback
		}
		if reason == "" {
			reason = "room broken: " + r.BrokenReason
		}
	}
	if reason == "" {
		return fallback
	}
	return reason
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
