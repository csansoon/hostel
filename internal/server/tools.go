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

// capacity counts what the candidate machines can take: free and all rooms,
// or, for a whole-machine request, machines with all rooms free and all machines. It
// also says whether one of the machines can create a new machine now.
func (s *Server) capacity(cands []string, machines []store.Machine, rooms []store.Room, whole bool) (free, total int, newMachine bool) {
	perTemplate := map[string]int{}
	for _, r := range machines {
		key := r.Provider + "/" + r.Template
		if !slices.Contains(cands, key) {
			continue
		}
		perTemplate[key]++
		usable := r.State == protocol.MachineRunning && !r.Drained
		machineRooms, machineFree := 0, 0
		for _, sl := range rooms {
			if sl.MachineID != r.ID {
				continue
			}
			machineRooms++
			if sl.State == protocol.RoomFree && usable {
				machineFree++
			}
		}
		switch {
		case !whole:
			total, free = total+machineRooms, free+machineFree
		case machineRooms > 0:
			total++
			if machineFree == machineRooms {
				free++
			}
		}
	}
	for _, key := range cands {
		m := s.config().Template(key)
		if m != nil && m.OnDemand() && perTemplate[key] < *m.MaxMachines {
			newMachine = true
			if whole {
				total++
			} else {
				total += len(m.Rooms)
			}
		}
	}
	return free, total, newMachine
}

// online reports whether a machine can take a lease: an on-demand machine
// always, a static machine when its machine is not offline or retired.
func online(m *config.Template, machines []store.Machine) bool {
	if m.OnDemand() {
		return true
	}
	for _, r := range machines {
		if r.Provider+"/"+r.Template == m.Key() {
			return r.State != protocol.MachineRetired && !(r.State == protocol.MachineBroken && r.BrokenReason == reasonUnreachable)
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
		s.internalError(w, "read machines", err)
		return
	}
	rooms, err := s.store.Rooms(ctx)
	if err != nil {
		s.internalError(w, "read rooms", err)
		return
	}
	leases, err := s.store.ActiveLeases(ctx)
	if err != nil {
		s.internalError(w, "read leases", err)
		return
	}
	measured, err := s.store.LastHardware(ctx)
	if err != nil {
		s.internalError(w, "read machine hardware", err)
		return
	}
	cfg := view.lc.cfg
	templates := view.schedTemplates()
	var names []string
	for _, m := range cfg.Templates() {
		for _, mt := range m.ToolList {
			if !slices.Contains(names, mt.Name) {
				names = append(names, mt.Name)
			}
		}
	}
	out := protocol.Tools{Tools: []protocol.Tool{}, Note: repoToolsNote}
	for _, name := range names {
		entry := protocol.Tool{Name: name, Mise: true, Needs: []string{}, Options: []string{}, Env: []string{}, Templates: []protocol.ToolTemplate{}}
		if t := cfg.Tools[name]; t != nil {
			entry.Mise, entry.Description, entry.Screen, entry.Needs = false, t.Description, t.Screen, nonNil(t.Needs)
			entry.MaxOptions, entry.WholeMachine, entry.Env, entry.Guide = t.MaxOptions, t.WholeMachine, envNames(t), t.Guide
		}
		var cands []string
		for i, m := range cfg.Templates() {
			h, ok := templates[i].Tools[name]
			if !ok {
				continue
			}
			cands = append(cands, m.Key())
			entry.Templates = append(entry.Templates, protocol.ToolTemplate{Key: m.Key(), OS: m.OS, Options: nonNil(h.Options),
				OnDemand: m.OnDemand(), Online: online(m, view.machines), Hardware: templateHardware(m, measured)})
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
		free, total, newMachine := s.capacity(cands, view.machines, rooms, leaseRequest(view.lc.cfg, store.Lease{Tools: []string{name}}).Exclusive)
		for _, l := range leases {
			if l.State == protocol.LeaseQueued && slices.Contains(toolNamesOf(l.Tools), name) {
				entry.Waiting++
			}
		}
		wait := time.Duration(0)
		if free == 0 || entry.Waiting > 0 {
			wait = s.estimatedWait(ctx, cands, entry.Waiting, total, newMachine && free == 0)
		}
		entry.FreeRooms, entry.NewMachine, entry.EstimatedWaitSeconds = free, newMachine, int(wait.Seconds())
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

// leaseWait estimates the wait of a queued lease from the leases before it
// that share one of its candidate machines (from the last scheduler pass),
// and returns their number.
func (s *Server) leaseWait(ctx context.Context, l store.Lease) (int, int) {
	view, err := s.loadToolView(ctx)
	if err != nil {
		return 0, 0
	}
	rooms, err := s.store.Rooms(ctx)
	if err != nil {
		return 0, 0
	}
	req := leaseRequest(view.lc.cfg, l)
	cands := scheduler.Candidates(view.schedTemplates(), req)
	free, total, newMachine := s.capacity(cands, view.machines, rooms, req.Exclusive)
	behind := s.waitingOf(l.ID).Behind
	return int(s.estimatedWait(ctx, cands, behind, total, newMachine && free == 0).Seconds()), behind
}
