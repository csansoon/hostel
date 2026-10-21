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
// ready lease. A tunnel with room_port 0 keeps its pool port, or gets the lowest
// free one. A fixed port that another lease on the room uses is refused (409).
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
	room, err := s.store.Room(ctx, l.RoomID)
	if err != nil {
		s.internalError(w, "read room", err)
		return
	}
	m, err := s.machineOf(room)
	if err != nil {
		s.internalError(w, "read machine", err)
		return
	}
	others, err := s.store.RoomTunnelPorts(ctx, l.RoomID, l.ID)
	if err != nil {
		s.internalError(w, "read tunnels", err)
		return
	}
	current, err := s.store.LeaseTunnels(ctx, l.ID)
	if err != nil {
		s.internalError(w, "read tunnels", err)
		return
	}
	pool := slotConfig(m, l.SlotName).Ports
	ports := map[string]int{}
	used := map[int]bool{}
	for _, t := range req {
		if t.RoomPort == 0 {
			continue
		}
		if others[t.RoomPort] {
			writeError(w, http.StatusConflict, "port_in_use", fmt.Sprintf("room port %d is in use by another lease on the room", t.RoomPort))
			return
		}
		if used[t.RoomPort] {
			writeError(w, http.StatusConflict, "port_in_use", fmt.Sprintf("room port %d is listed twice", t.RoomPort))
			return
		}
		ports[t.Name], used[t.RoomPort] = t.RoomPort, true
	}
	var names []string
	for _, t := range req {
		if t.RoomPort == 0 {
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
			writeError(w, http.StatusConflict, "port_in_use", fmt.Sprintf("tunnel %s: no free pool port on the slot", n))
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
	writeJSON(w, http.StatusOK, map[string]map[string]int{"tunnel_ports": ports})
}
