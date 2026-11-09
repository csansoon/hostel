package server

import (
	"fmt"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
)

// handleSetTunnels is PUT /v1/leases/{id}/tunnels: it replaces the tunnels of a
// ready lease. A tunnel with machine_port 0 keeps its pool port, or gets the lowest
// free one. A fixed port that another lease on the machine uses is refused (409).
func (s *Server) handleSetTunnels(w http.ResponseWriter, r *http.Request) {
	l, ok := s.leaseFor(w, r)
	if !ok {
		return
	}
	var req []protocol.TunnelRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if msg := checkTunnels(req); msg != "" {
		writeError(w, http.StatusBadRequest, "", msg)
		return
	}
	if l.State != protocol.LeaseReady {
		writeError(w, http.StatusConflict, protocol.CodeLeaseEnded, fmt.Sprintf("lease %s is %s, not ready", l.ID, l.State))
		return
	}
	ctx := r.Context()
	machine, err := s.store.Machine(ctx, l.MachineID)
	if err != nil {
		s.internalError(w, "read machine", err)
		return
	}
	m, err := s.templateOf(machine)
	if err != nil {
		s.internalError(w, "read machine", err)
		return
	}
	others, err := s.store.MachineTunnelPorts(ctx, l.MachineID, l.ID)
	if err != nil {
		s.internalError(w, "read tunnels", err)
		return
	}
	current, err := s.store.LeaseTunnels(ctx, l.ID)
	if err != nil {
		s.internalError(w, "read tunnels", err)
		return
	}
	pool := roomConfig(m, l.RoomName).Ports
	ports := map[string]int{}
	used := map[int]bool{}
	for _, t := range req {
		if t.MachinePort == 0 {
			continue
		}
		if others[t.MachinePort] {
			writeError(w, http.StatusConflict, "port_in_use", fmt.Sprintf("machine port %d is in use by another lease on the machine", t.MachinePort))
			return
		}
		if used[t.MachinePort] {
			writeError(w, http.StatusConflict, "port_in_use", fmt.Sprintf("machine port %d is listed twice", t.MachinePort))
			return
		}
		// A port of another room belongs to that room's lease.
		for _, sl := range m.Rooms {
			if sl.Name != l.RoomName && slices.Contains(sl.Ports, t.MachinePort) {
				writeError(w, http.StatusConflict, "port_in_use", fmt.Sprintf("machine port %d belongs to room %s of the machine; choose another port", t.MachinePort, sl.Name))
				return
			}
		}
		ports[t.Name], used[t.MachinePort] = t.MachinePort, true
	}
	var names []string
	for _, t := range req {
		if t.MachinePort == 0 {
			names = append(names, t.Name)
		}
	}
	sort.Strings(names)
	// First, pool tunnels keep their port; then the others get the lowest free one.
	for _, n := range names {
		if p, ok := current[n]; ok && slices.Contains(pool, p) && !used[p] && !others[p] {
			ports[n], used[p] = p, true
		}
	}
	free := slices.Clone(pool)
	sort.Ints(free)
	for _, n := range names {
		if _, ok := ports[n]; ok {
			continue
		}
		for len(free) > 0 && (used[free[0]] || others[free[0]]) {
			free = free[1:]
		}
		if len(free) == 0 {
			writeError(w, http.StatusConflict, "port_in_use", fmt.Sprintf("tunnel %s: no free pool port on the room", n))
			return
		}
		ports[n], used[free[0]] = free[0], true
		free = free[1:]
	}
	ok, err = s.store.SetTunnels(ctx, l.ID, req, ports, time.Now())
	if err != nil {
		s.internalError(w, "set tunnels", err)
		return
	}
	if !ok {
		writeError(w, http.StatusConflict, protocol.CodeLeaseEnded, fmt.Sprintf("lease %s is no longer ready", l.ID))
		return
	}
	s.gateway.RecheckForwards(ctx, l.ID)
	writeJSON(w, http.StatusOK, map[string]map[string]int{"tunnel_ports": ports})
}
