package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/store"
)

func (s *Server) machineFor(w http.ResponseWriter, r *http.Request) (store.Machine, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "", "the machine ID must be a number; \"hostel status <machine>\" shows it")
		return store.Machine{}, false
	}
	machine, err := s.store.Machine(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "", fmt.Sprintf("no machine %d", id))
		return machine, false
	}
	if err != nil {
		s.internalError(w, "read machine", err)
		return machine, false
	}
	return machine, true
}

func (s *Server) handleMachineDrain(drained bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine, ok := s.machineFor(w, r)
		if !ok {
			return
		}
		if err := s.store.SetDrained(r.Context(), machine.ID, drained); err != nil {
			s.internalError(w, "drain machine", err)
			return
		}
		s.store.AddEvent(r.Context(), time.Now(), "", machine.ID, "admin", fmt.Sprintf("machine %s drained=%v", machine.Name, drained))
		s.kickScheduler()
		writeJSON(w, http.StatusOK, struct{}{})
	}
}

// handleMachineRepair runs the cleanup of the broken and dirty rooms of a broken
// static machine again, then its check scripts; the machine is running if all pass
// (design, section 6).
func (s *Server) handleMachineRepair(w http.ResponseWriter, r *http.Request) {
	machine, ok := s.machineFor(w, r)
	if !ok {
		return
	}
	var req struct {
		AcceptHostKey bool `json:"accept_host_key"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	m, err := s.templateOf(machine)
	if err != nil {
		writeError(w, http.StatusConflict, "", err.Error())
		return
	}
	if m.OnDemand() {
		writeError(w, http.StatusConflict, "", "on-demand machines are not repaired; a broken one is destroyed")
		return
	}
	if machine.State != protocol.MachineBroken {
		writeError(w, http.StatusConflict, "", fmt.Sprintf("machine %d is %s, not broken", machine.ID, machine.State))
		return
	}
	ctx := r.Context()
	if req.AcceptHostKey {
		if err := s.store.ClearHostKey(ctx, machine.ID); err != nil {
			s.internalError(w, "clear the host key", err)
			return
		}
	}
	if err := s.store.BreakRooms(ctx, machine.ID); err != nil {
		s.internalError(w, "mark rooms for cleanup", err)
		return
	}
	if _, err := s.store.SetMachineState(ctx, machine.ID, []string{protocol.MachineBroken}, protocol.MachineProvisioning, "", time.Now()); err != nil {
		s.internalError(w, "repair machine", err)
		return
	}
	s.kickScheduler()
	writeJSON(w, http.StatusOK, struct{}{})
}

// handleMachineDestroy destroys an on-demand machine that has no lease.
func (s *Server) handleMachineDestroy(w http.ResponseWriter, r *http.Request) {
	machine, ok := s.machineFor(w, r)
	if !ok {
		return
	}
	m, err := s.templateOf(machine)
	if err == nil && !m.OnDemand() {
		writeError(w, http.StatusConflict, "", "static machines are not destroyed; remove the machine from the configuration")
		return
	}
	if leases := s.machineLeases(r.Context(), machine.ID); len(leases) > 0 {
		writeError(w, http.StatusConflict, "", fmt.Sprintf("machine %d has lease %s; release it first", machine.ID, leases[0].ID))
		return
	}
	ok, err = s.store.SetMachineState(r.Context(), machine.ID, []string{protocol.MachineRunning, protocol.MachineStopped, protocol.MachineBroken},
		protocol.MachineDestroying, "destroyed by an admin", time.Now())
	if err != nil {
		s.internalError(w, "destroy machine", err)
		return
	}
	if !ok {
		writeError(w, http.StatusConflict, "", fmt.Sprintf("machine %d is %s; try again later", machine.ID, machine.State))
		return
	}
	s.kickScheduler()
	writeJSON(w, http.StatusOK, struct{}{})
}
