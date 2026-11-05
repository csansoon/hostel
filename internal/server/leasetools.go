package server

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/scheduler"
	"github.com/csansoon/hostel/internal/store"
)

// toolView is one configuration with the inventories of the rooms: what each
// machine and each room gives (design, section 5).
type toolView struct {
	lc    *liveConfig
	rooms []store.Room
	rts   []store.RoomTool
}

func (s *Server) loadToolView(ctx context.Context) (*toolView, error) {
	rooms, err := s.store.Rooms(ctx)
	if err != nil {
		return nil, err
	}
	rts, err := s.store.RoomTools(ctx)
	if err != nil {
		return nil, err
	}
	return &toolView{lc: s.live.Load(), rooms: rooms, rts: rts}, nil
}

// configuredHaves is what the configuration says a machine gives: the
// options of its entries, or all options of a screen tool.
func configuredHaves(cfg *config.Server, m *config.Machine) map[string]scheduler.Have {
	out := map[string]scheduler.Have{}
	for _, mt := range m.ToolList {
		t := cfg.Tools[mt.Name]
		h := out[mt.Name]
		h.Screen = t != nil && t.Screen
		if mt.Option != "" {
			h.Options = append(h.Options, mt.Option)
		}
		out[mt.Name] = h
	}
	for name, h := range out {
		if t := cfg.Tools[name]; t != nil && t.Screen && len(h.Options) == 0 {
			h.Options = slices.Clone(t.Options)
			out[name] = h
		}
	}
	return out
}

// splitOptions splits the option column of the inventory.
func splitOptions(s string) []string {
	var out []string
	for _, o := range strings.Split(s, ",") {
		if o = strings.TrimSpace(o); o != "" && !slices.Contains(out, o) {
			out = append(out, o)
		}
	}
	return out
}

// withInventory applies the options that check scripts reported: the versions
// of a software tool replace the configured ones; a screen tool keeps the
// configured options that the room has. ok is false for a screen tool that
// has none of them: it gives nothing (empty options would match any).
func withInventory(h scheduler.Have, reported []string) (scheduler.Have, bool) {
	if len(reported) == 0 {
		return h, true
	}
	if h.Screen {
		if len(h.Options) == 0 {
			return h, true
		}
		var keep []string
		for _, o := range h.Options {
			if slices.Contains(reported, o) {
				keep = append(keep, o)
			}
		}
		h.Options = keep
		return h, len(keep) > 0
	}
	h.Options, h.Checked = reported, true
	return h, true
}

func (v *toolView) inventory(roomID int64, tool string) []string {
	for _, rt := range v.rts {
		if rt.RoomID == roomID && rt.Tool == tool && rt.State == store.ToolReady {
			return splitOptions(rt.Option)
		}
	}
	return nil
}

// current reports a room that the machine's current settings describe: its
// inventory says what a new lease can get. A stale room installs again before
// its next lease.
func (v *toolView) current(r store.Room, m *config.Machine) bool {
	if r.State == protocol.RoomDestroyed || r.State == protocol.RoomRetired || r.State == protocol.RoomDestroying {
		return false
	}
	return r.ConfigSHA256 == "" || r.ConfigSHA256 == machineHash(v.lc, m)
}

// roomHaves is what a room gives: its inventory, else its machine's
// configuration.
func (v *toolView) roomHaves(m *config.Machine, roomID int64) map[string]scheduler.Have {
	out := configuredHaves(v.lc.cfg, m)
	for name, h := range out {
		if h, ok := withInventory(h, v.inventory(roomID, name)); ok {
			out[name] = h
		} else {
			delete(out, name)
		}
	}
	return out
}

// machineHaves is what a machine gives: the inventories of its rooms where
// they report options, else its configuration.
func (v *toolView) machineHaves(m *config.Machine) map[string]scheduler.Have {
	out := configuredHaves(v.lc.cfg, m)
	for name, h := range out {
		var reported []string
		for _, r := range v.rooms {
			if r.Provider+"/"+r.Machine != m.Key() || !v.current(r, m) {
				continue
			}
			for _, o := range v.inventory(r.ID, name) {
				if !slices.Contains(reported, o) {
					reported = append(reported, o)
				}
			}
		}
		if h, ok := withInventory(h, reported); ok {
			out[name] = h
		} else {
			delete(out, name)
		}
	}
	return out
}

func (v *toolView) schedMachines() []scheduler.Machine {
	var out []scheduler.Machine
	for _, m := range v.lc.cfg.Machines() {
		pool := -1
		for _, sl := range m.Slots {
			if pool < 0 || len(sl.Ports) < pool {
				pool = len(sl.Ports)
			}
		}
		out = append(out, scheduler.Machine{
			Key: m.Key(), Provider: m.Provider, OnDemand: m.OnDemand(), Tools: v.machineHaves(m),
			CanResume: v.lc.providers[m.Provider].Capabilities(m.Name).CanStop,
			PoolPorts: pool, Slots: slotNames(m), MaxRooms: *m.MaxRooms,
			MaxLease: m.MaxLease.D(), MaxLifetime: m.MaxLifetime.D(),
		})
	}
	return out
}

// toolWants parses the tool entries of a lease (checked at its creation).
func toolWants(tools []string) []scheduler.Want {
	var out []scheduler.Want
	for _, raw := range tools {
		if e, err := config.ParseToolEntry(raw); err == nil {
			out = append(out, scheduler.Want{Name: e.Name, Option: e.Option})
		}
	}
	return out
}

// toolNamesOf returns the tool names of entries, each once, in order.
func toolNamesOf(tools []string) []string {
	var out []string
	for _, w := range toolWants(tools) {
		if !slices.Contains(out, w.Name) {
			out = append(out, w.Name)
		}
	}
	return out
}

// leaseClosure returns the defined tools that a lease needs: its tools and
// their needs, each after the tools that it needs (the order of start).
func leaseClosure(cfg *config.Server, tools []string) []*config.Tool {
	var out []*config.Tool
	var visit func(name string)
	visit = func(name string) {
		t := cfg.Tools[name]
		if t == nil || slices.Contains(out, t) {
			return
		}
		for _, n := range t.Needs {
			visit(n)
		}
		out = append(out, t)
	}
	for _, name := range toolNamesOf(tools) {
		visit(name)
	}
	return out
}

// leaseToolNames returns the names of a lease's tools and their needs, also
// the needs that are mise tools, in the order of needs.
func leaseToolNames(cfg *config.Server, tools []string) []string {
	out := []string{}
	var visit func(name string)
	visit = func(name string) {
		if slices.Contains(out, name) {
			return
		}
		if t := cfg.Tools[name]; t != nil {
			for _, n := range toolNamesOf(t.Needs) {
				visit(n)
			}
		}
		out = append(out, name)
	}
	for _, name := range toolNamesOf(tools) {
		visit(name)
	}
	return out
}

// leaseRequest is what a lease asks the scheduler for.
func leaseRequest(cfg *config.Server, l store.Lease) scheduler.Request {
	req := scheduler.Request{Tools: toolWants(l.Tools), PoolTunnels: poolTunnels(l.Tunnels), Fresh: l.Fresh}
	for _, t := range leaseClosure(cfg, l.Tools) {
		req.Exclusive = req.Exclusive || t.WholeMachine
	}
	return req
}

// listedBy reports whether a machine of the configuration lists the tool.
func listedBy(cfg *config.Server, name string) bool {
	return slices.ContainsFunc(cfg.Machines(), func(m *config.Machine) bool { return m.Tool(name) != nil })
}

// checkLeaseTools checks the tools of a lease request (R4): each tool
// exists, each option of a screen tool is one of its options, and a screen
// tool has at most max_options options. It returns the entries without
// duplicates, or an error code and message.
func checkLeaseTools(cfg *config.Server, tools []string) ([]string, string, string) {
	out := []string{}
	options := map[string]int{}
	for _, raw := range tools {
		e, err := config.ParseToolEntry(raw)
		if err != nil {
			return nil, protocol.CodeUnknownTool, err.Error()
		}
		if slices.Contains(out, e.String()) {
			continue
		}
		t := cfg.Tools[e.Name]
		switch {
		case t == nil && config.IsOSName(e.Name):
			return nil, protocol.CodeUnknownTool, fmt.Sprintf("%s is an OS, not a tool: ask for the tools that you need (an OS has no tools of its own), or for none to get any machine; run \"hostel tools\"", e.Name)
		case t == nil && !listedBy(cfg, e.Name):
			return nil, protocol.CodeUnknownTool, fmt.Sprintf("unknown tool %q: no machine of this server has it; run \"hostel tools\" for the list", e.Name)
		case t != nil && t.Screen && e.Option != "" && !slices.Contains(t.Options, e.Option):
			return nil, protocol.CodeUnknownOption, fmt.Sprintf("the tool %s has no option %q; options: %s", e.Name, e.Option, strings.Join(t.Options, ", "))
		case t != nil && t.Screen && len(t.Options) == 0 && e.Option != "":
			return nil, protocol.CodeUnknownOption, fmt.Sprintf("the tool %s has no options; ask for %s", e.Name, e.Name)
		}
		if t != nil && t.Screen && e.Option != "" {
			options[e.Name]++
			if options[e.Name] > t.MaxOptions {
				return nil, protocol.CodeTooManyOptions, fmt.Sprintf("the tool %s allows at most %d options in one lease", e.Name, t.MaxOptions)
			}
		}
		out = append(out, e.String())
	}
	return out, "", ""
}

// noMachineMessage explains why no configured machine can serve a request
// (design, section 5).
func noMachineMessage(machines []scheduler.Machine, r scheduler.Request) string {
	toolsOnly := r
	toolsOnly.PoolTunnels, toolsOnly.Fresh = 0, false
	if len(scheduler.Candidates(machines, toolsOnly)) > 0 {
		var why []string
		if r.PoolTunnels > 0 {
			why = append(why, fmt.Sprintf("%d tunnel ports from the slot pool", r.PoolTunnels))
		}
		if r.Fresh {
			why = append(why, "a new room (--fresh needs an on-demand machine)")
		}
		return fmt.Sprintf("no machine with these tools has %s; run \"hostel tools\"", strings.Join(why, " and "))
	}
	if len(r.Tools) == 0 {
		return "the server has no machines; ask an admin to add one"
	}
	var entries, missing, with []string
	seen := map[string]bool{}
	for _, w := range r.Tools {
		entry := w.Name
		if w.Option != "" {
			entry += ":" + w.Option
		}
		entries = append(entries, entry)
		if len(scheduler.Candidates(machines, scheduler.Request{Tools: []scheduler.Want{w}})) == 0 {
			missing = append(missing, entry)
		}
		if seen[w.Name] {
			continue
		}
		seen[w.Name] = true
		var ms []string
		for _, m := range machines {
			if h, ok := m.Tools[w.Name]; ok {
				desc := m.Key
				if len(h.Options) > 0 {
					desc += " (" + strings.Join(h.Options, ", ") + ")"
				}
				ms = append(ms, desc)
			}
		}
		if len(ms) > 0 {
			with = append(with, fmt.Sprintf("machines with %s: %s", w.Name, strings.Join(ms, ", ")))
		}
	}
	msg := "no machine has " + strings.Join(entries, " and ") + " together"
	if len(missing) > 0 {
		msg = "no machine has " + strings.Join(missing, " and ")
	}
	if len(with) > 0 {
		msg += "; " + strings.Join(with, "; ")
	}
	return msg + "; run \"hostel tools\""
}

// leaseUses reports whether a lease asked for a tool, needs it, or started it.
func leaseUses(cfg *config.Server, l store.Lease, name string) bool {
	return slices.Contains(leaseToolNames(cfg, l.Tools), name) ||
		slices.ContainsFunc(l.Started, func(st protocol.StartedTool) bool { return st.Name == name })
}

// planStarts returns the screen tools that start for a lease on a room, with
// their options, in the order of needs: the requested options, else the first
// option that the room has.
func planStarts(cfg *config.Server, l store.Lease, room map[string]scheduler.Have) []protocol.StartedTool {
	var out []protocol.StartedTool
	for _, t := range leaseClosure(cfg, l.Tools) {
		if !t.Screen {
			continue
		}
		st := protocol.StartedTool{Name: t.Name, Options: []string{}}
		for _, w := range toolWants(l.Tools) {
			if w.Name == t.Name && w.Option != "" && !slices.Contains(st.Options, w.Option) {
				st.Options = append(st.Options, w.Option)
			}
		}
		if len(st.Options) == 0 {
			if have := room[t.Name].Options; len(have) > 0 {
				st.Options = []string{have[0]}
			} else if len(t.Options) > 0 {
				st.Options = []string{t.Options[0]}
			}
		}
		out = append(out, st)
	}
	return out
}

// leaseToolEntries are the lease's tools with the options of its screen tools
// (HOSTEL_TOOLS): the requested entries, with the options that each started
// screen tool got.
func leaseToolEntries(tools []string, started []protocol.StartedTool) []string {
	out := []string{}
	for _, raw := range tools {
		e, _ := config.ParseToolEntry(raw)
		if !slices.ContainsFunc(started, func(st protocol.StartedTool) bool { return st.Name == e.Name }) {
			out = append(out, raw)
		}
	}
	for _, st := range started {
		if len(st.Options) == 0 {
			out = append(out, st.Name)
		}
		for _, o := range st.Options {
			out = append(out, st.Name+":"+o)
		}
	}
	return out
}
