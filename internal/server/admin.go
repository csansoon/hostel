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

func (s *Server) roomFor(w http.ResponseWriter, r *http.Request) (store.Room, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "", "the room ID must be a number; \"hostel status <room>\" shows it")
		return store.Room{}, false
	}
	room, err := s.store.Room(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "", fmt.Sprintf("no room %d", id))
		return room, false
	}
	if err != nil {
		s.internalError(w, "read room", err)
		return room, false
	}
	return room, true
}

func (s *Server) handleRoomDrain(drained bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		room, ok := s.roomFor(w, r)
		if !ok {
			return
		}
		if err := s.store.SetDrained(r.Context(), room.ID, drained); err != nil {
			s.internalError(w, "drain room", err)
			return
		}
		s.store.AddEvent(r.Context(), time.Now(), "", room.ID, "admin", fmt.Sprintf("room %s drained=%v", room.Name, drained))
		s.kickScheduler()
		writeJSON(w, http.StatusOK, struct{}{})
	}
}

// handleRoomRepair runs the cleanup of the broken and dirty slots of a broken
// static room again, then its health scripts; the room is running if all pass
// (design, section 6).
func (s *Server) handleRoomRepair(w http.ResponseWriter, r *http.Request) {
	room, ok := s.roomFor(w, r)
	if !ok {
		return
	}
	var req struct {
		AcceptHostKey bool `json:"accept_host_key"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	m, err := s.machineOf(room)
	if err != nil {
		writeError(w, http.StatusConflict, "", err.Error())
		return
	}
	if m.OnDemand() {
		writeError(w, http.StatusConflict, "", "on-demand rooms are not repaired; a broken one is destroyed")
		return
	}
	if room.State != protocol.RoomBroken {
		writeError(w, http.StatusConflict, "", fmt.Sprintf("room %d is %s, not broken", room.ID, room.State))
		return
	}
	ctx := r.Context()
	if req.AcceptHostKey {
		if err := s.store.ClearHostKey(ctx, room.ID); err != nil {
			s.internalError(w, "clear the host key", err)
			return
		}
	}
	if err := s.store.BreakSlots(ctx, room.ID); err != nil {
		s.internalError(w, "mark slots for cleanup", err)
		return
	}
	if _, err := s.store.SetRoomState(ctx, room.ID, []string{protocol.RoomBroken}, protocol.RoomProvisioning, "", time.Now()); err != nil {
		s.internalError(w, "repair room", err)
		return
	}
	s.kickScheduler()
	writeJSON(w, http.StatusOK, struct{}{})
}

// handleRoomDestroy destroys an on-demand room that has no lease.
func (s *Server) handleRoomDestroy(w http.ResponseWriter, r *http.Request) {
	room, ok := s.roomFor(w, r)
	if !ok {
		return
	}
	m, err := s.machineOf(room)
	if err == nil && !m.OnDemand() {
		writeError(w, http.StatusConflict, "", "static rooms are not destroyed; remove the machine from the configuration")
		return
	}
	if leases := s.roomLeases(r.Context(), room.ID); len(leases) > 0 {
		writeError(w, http.StatusConflict, "", fmt.Sprintf("room %d has lease %s; release it first", room.ID, leases[0].ID))
		return
	}
	ok, err = s.store.SetRoomState(r.Context(), room.ID, []string{protocol.RoomRunning, protocol.RoomStopped, protocol.RoomBroken},
		protocol.RoomDestroying, "destroyed by an admin", time.Now())
	if err != nil {
		s.internalError(w, "destroy room", err)
		return
	}
	if !ok {
		writeError(w, http.StatusConflict, "", fmt.Sprintf("room %d is %s; try again later", room.ID, room.State))
		return
	}
	s.kickScheduler()
	writeJSON(w, http.StatusOK, struct{}{})
}
