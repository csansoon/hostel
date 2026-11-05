package server

import (
	"context"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/scheduler"
	"github.com/csansoon/hostel/internal/store"
)

// repoToolsNote is the note of GET /v1/tools about the repository's tools.
const repoToolsNote = "The tools of the repository's mise files (mise.toml, .tool-versions) are set up for each lease on every machine; do not ask for them."

// capacity counts the free slots and all slots of the candidate machines, and
// whether one of them can create a new room now.
func (s *Server) capacity(cands []string, rooms []store.Room, slots []store.Slot) (free, total int, newRoom bool) {
	perMachine := map[string]int{}
	for _, r := range rooms {
		key := r.Provider + "/" + r.Machine
		if !slices.Contains(cands, key) {
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
	for _, key := range cands {
		m := s.config().Machine(key)
		if m != nil && m.OnDemand() && perMachine[key] < *m.MaxRooms {
			newRoom = true
			total += len(m.Slots)
		}
	}
	return free, total, newRoom
}

// online reports whether a machine can take a lease: an on-demand machine
// always, a static machine when its room is not offline or retired.
func online(m *config.Machine, rooms []store.Room) bool {
	if m.OnDemand() {
		return true
	}
	for _, r := range rooms {
		if r.Provider+"/"+r.Machine == m.Key() {
			return r.State != protocol.RoomRetired && !(r.State == protocol.RoomBroken && r.BrokenReason == reasonUnreachable)
		}
	}
	return false
}

// envNames are the names of a tool's environment, with PATH when the tool
// adds to it.
func envNames(t *config.Tool) []string {
	out := sortedKeys(t.Env)
	if len(t.Path) > 0 {
		out = append(out, "PATH")
	}
	return out
}

func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	view, err := s.loadToolView(ctx)
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
	cfg := view.lc.cfg
	machines := view.schedMachines()
	var names []string
	for _, m := range cfg.Machines() {
		for _, mt := range m.ToolList {
			if !slices.Contains(names, mt.Name) {
				names = append(names, mt.Name)
			}
		}
	}
	out := protocol.Tools{Tools: []protocol.Tool{}, Note: repoToolsNote}
	for _, name := range names {
		entry := protocol.Tool{Name: name, Mise: true, Needs: []string{}, Options: []string{}, Env: []string{}, Machines: []protocol.ToolMachine{}}
		if t := cfg.Tools[name]; t != nil {
			entry.Mise, entry.Description, entry.Screen, entry.Needs = false, t.Description, t.Screen, nonNil(t.Needs)
			entry.MaxOptions, entry.WholeMachine, entry.Env, entry.Guide = t.MaxOptions, t.WholeMachine, envNames(t), t.Guide
		}
		var cands []string
		for i, m := range cfg.Machines() {
			h, ok := machines[i].Tools[name]
			if !ok {
				continue
			}
			cands = append(cands, m.Key())
			entry.Machines = append(entry.Machines, protocol.ToolMachine{Key: m.Key(), OS: m.OS, Options: nonNil(h.Options),
				OnDemand: m.OnDemand(), Online: online(m, view.rooms)})
			for _, o := range h.Options {
				if !slices.Contains(entry.Options, o) {
					entry.Options = append(entry.Options, o)
				}
			}
		}
		if t := cfg.Tools[name]; t != nil && t.Screen {
			// The options in the tool's order.
			var ordered []string
			for _, o := range t.Options {
				if slices.Contains(entry.Options, o) {
					ordered = append(ordered, o)
				}
			}
			entry.Options = nonNil(ordered)
		}
		free, total, newRoom := s.capacity(cands, view.rooms, slots)
		for _, l := range leases {
			if l.State == protocol.LeaseQueued && slices.Contains(toolNamesOf(l.Tools), name) {
				entry.Waiting++
			}
		}
		wait := time.Duration(0)
		if free == 0 || entry.Waiting > 0 {
			wait = s.estimatedWait(ctx, cands, entry.Waiting+1, total)
		}
		entry.FreeSlots, entry.NewRoom, entry.EstimatedWaitSeconds = free, newRoom, int(wait.Seconds())
		entry.TypicalLeaseSeconds = int(s.medianDuration(ctx, nil, name, 0).Seconds())
		out.Tools = append(out.Tools, entry)
	}
	sort.SliceStable(out.Tools, func(i, j int) bool {
		a, b := out.Tools[i], out.Tools[j]
		if a.Screen != b.Screen {
			return a.Screen
		}
		return a.Name < b.Name
	})
	writeJSON(w, http.StatusOK, out)
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// leaseWait estimates the wait of a queued lease: the leases before it that
// share one of its candidate machines (from the last scheduler pass), and
// the lease itself.
func (s *Server) leaseWait(ctx context.Context, l store.Lease) (int, int) {
	view, err := s.loadToolView(ctx)
	if err != nil {
		return 0, 0
	}
	slots, err := s.store.Slots(ctx)
	if err != nil {
		return 0, 0
	}
	cands := scheduler.Candidates(view.schedMachines(), leaseRequest(view.lc.cfg, l))
	_, total, _ := s.capacity(cands, view.rooms, slots)
	behind := s.waitingOf(l.ID).Behind
	return int(s.estimatedWait(ctx, cands, behind+1, total).Seconds()), behind
}
