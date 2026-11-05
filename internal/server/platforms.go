package server

import (
	"context"
	"net/http"
	"time"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/scheduler"
	"github.com/csansoon/hostel/internal/store"
)

// capacity counts the free slots and all slots of the machines that serve a
// request, and whether one of them can create a new room now.
func (s *Server) capacity(req scheduler.Request, rooms []store.Room, slots []store.Slot) (free, total int, newRoom bool) {
	cands := map[string]bool{}
	for _, k := range scheduler.Candidates(s.schedMachines(), req) {
		cands[k] = true
	}
	perMachine := map[string]int{}
	for _, r := range rooms {
		key := r.Provider + "/" + r.Machine
		if !cands[key] {
			continue
		}
		perMachine[key]++
		for _, sl := range slots {
			if sl.RoomID != r.ID {
				continue
			}
			total++
			if sl.State == protocol.SlotFree && r.State == protocol.RoomRunning && !r.Drained {
				free++
			}
		}
	}
	for key := range cands {
		m := s.config().Machine(key)
		if m.OnDemand() && perMachine[key] < *m.MaxRooms {
			newRoom = true
			total += len(m.Slots)
		}
	}
	return free, total, newRoom
}

func (s *Server) handlePlatforms(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rooms, err := s.store.Rooms(ctx)
	if err != nil {
		s.internalError(w, "read rooms", err)
		return
	}
	slots, err := s.store.Slots(ctx)
	if err != nil {
		s.internalError(w, "read slots", err)
		return
	}
	leases, err := s.store.ActiveLeases(ctx)
	if err != nil {
		s.internalError(w, "read leases", err)
		return
	}
	// Until GET /v1/tools (M10e phase 4), each defined tool and each OS is
	// one entry: what a lease can ask for.
	cfg := s.config()
	names := append(config.ToolNames(cfg.Tools), config.OSNames...)
	out := []protocol.Platform{}
	for _, name := range names {
		t := cfg.Tools[name]
		entry := protocol.Platform{Name: name, Kind: "os", Devices: []string{}}
		if t != nil {
			entry.Kind, entry.Description, entry.Devices, entry.MaxDevices = "tool", t.Description, nonNil(t.Options), t.MaxOptions
			if t.Screen {
				entry.Kind = "screen"
			}
		}
		req, _ := s.leaseRequest(store.Lease{Platform: name})
		machines := scheduler.Candidates(s.schedMachines(), req)
		if len(machines) == 0 {
			continue
		}
		free, total, newRoom := s.capacity(req, rooms, slots)
		queue := 0
		for _, l := range leases {
			if l.Platform == name && l.State == protocol.LeaseQueued {
				queue++
			}
		}
		wait := time.Duration(0)
		if free == 0 || queue > 0 {
			wait = s.estimatedWait(ctx, name, queue+1, total)
		}
		entry.FreeSlots, entry.NewRoom, entry.Queue, entry.EstimatedWaitSeconds = free, newRoom, queue, int(wait.Seconds())
		entry.Machines, entry.TypicalLeaseSeconds = machines, int(s.medianDuration(ctx, name, 0).Seconds())
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, out)
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// leaseWait estimates the wait of a queued lease.
func (s *Server) leaseWait(ctx context.Context, l store.Lease) int {
	rooms, err := s.store.Rooms(ctx)
	if err != nil {
		return 0
	}
	slots, err := s.store.Slots(ctx)
	if err != nil {
		return 0
	}
	req, _ := s.leaseRequest(l)
	_, total, _ := s.capacity(req, rooms, slots)
	return int(s.estimatedWait(ctx, l.Platform, l.Position, total).Seconds())
}
