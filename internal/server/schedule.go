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

// loadPlatformUploads hashes each platform directory for each OS once at start.
// Configuration changes, including platform scripts, need a restart.
func (s *Server) loadPlatformUploads() error {
	s.uploads = map[string]map[string]roomops.Platform{}
	for name, p := range s.cfg.Platforms {
		s.uploads[name] = map[string]roomops.Platform{}
		for _, osName := range config.OSNames {
			up, err := roomops.PlatformUpload(p.Dir, platformFiles(p, osName))
			if err != nil {
				return err
			}
			s.uploads[name][osName] = up
		}
	}
	return nil
}

func (s *Server) schedMachines() []scheduler.Machine {
	var out []scheduler.Machine
	for _, m := range s.cfg.Machines() {
		pool := -1
		for _, sl := range m.Slots {
			if pool < 0 || len(sl.Ports) < pool {
				pool = len(sl.Ports)
			}
		}
		out = append(out, scheduler.Machine{
			Key: m.Key(), Provider: m.Provider, OnDemand: m.OnDemand(), OS: m.OS, Platforms: m.Platforms,
			CanResume: s.providers[m.Provider].Capabilities(m.Name).CanStop,
			Labels:    m.Labels, PoolPorts: pool, Slots: slotNames(m), MaxRooms: *m.MaxRooms,
			MaxLease: m.MaxLease.D(), MaxLifetime: m.MaxLifetime.D(),
		})
	}
	return out
}

func (s *Server) schedProviders() []scheduler.Provider {
	var out []scheduler.Provider
	for _, name := range s.cfg.ProviderOrder {
		p := s.cfg.Providers[name]
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
		byID[l.ID] = l
		p := s.cfg.Platforms[l.Platform]
		queued = append(queued, scheduler.Lease{ID: l.ID, Priority: l.Priority, CreatedAt: l.CreatedAt,
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
	for _, r := range rooms {
		roomByID[r.ID] = r
		m := s.cfg.Machine(r.Provider + "/" + r.Machine)
		if m == nil {
			continue
		}
		sr := scheduler.Room{ID: r.ID, Machine: m.Key(), State: r.State, Drained: r.Drained, CreatedAt: r.CreatedAt, Ready: map[string]bool{}}
		for _, rp := range rps {
			if rp.RoomID == r.ID && rp.ScriptsSHA256 == s.uploads[rp.Platform][m.OS].Hash {
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
	out := scheduler.Schedule(scheduler.Input{Now: now, Aging: s.cfg.Aging.D(), Machines: s.schedMachines(),
		Providers: s.schedProviders(), Rooms: srooms, Leases: queued, Full: s.full.current(s.cfg.LimitRetry.D())})
	for _, id := range out.Evict {
		s.log.Info("evict an idle room to make space", "room_id", id)
		if _, err := s.store.SetRoomState(ctx, id, []string{protocol.RoomRunning, protocol.RoomStopped}, protocol.RoomDestroying,
			"evicted to make space for a new room", now); err != nil {
			return err
		}
	}
	for _, a := range out.Assignments {
		l := byID[a.LeaseID]
		m := s.cfg.Machine(a.Machine)
		var room store.Room
		switch a.Action {
		case scheduler.UseRoom, scheduler.ProvisionRoom:
			room = roomByID[a.RoomID]
		case scheduler.ResumeRoom:
			room = roomByID[a.RoomID]
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
			var err error
			room, err = s.store.InsertRoom(ctx, store.Room{Provider: m.Provider, Machine: m.Name, State: protocol.RoomCreating, OS: m.OS},
				s.cfg.ServerID, slotNames(m), now)
			if err != nil {
				return err
			}
		}
		ports, err := tunnelPorts(l.Tunnels, slotConfig(m, a.Slots[0]).Ports)
		if err != nil {
			if _, err := s.store.EndLease(ctx, l.ID, []string{protocol.LeaseQueued}, protocol.LeaseFailed, err.Error(), now); err != nil {
				return err
			}
			continue
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
	p := s.cfg.Platforms[l.Platform]
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
