package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/store"
)

func requestToken(r *http.Request) store.Token {
	return r.Context().Value(tokenKey{}).(store.Token)
}

// admin allows the request only for admin tokens.
func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if requestToken(r).Role != protocol.RoleAdmin {
			writeError(w, http.StatusForbidden, "", "this request needs an admin token")
			return
		}
		next(w, r)
	}
}

func (s *Server) internalError(w http.ResponseWriter, what string, err error) {
	s.log.Error(what, "err", err)
	writeError(w, http.StatusInternalServerError, "", what+" failed; see the server log")
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "", "invalid request body: "+err.Error())
		return false
	}
	return true
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
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
	rps, err := s.store.RoomPlatforms(ctx)
	if err != nil {
		s.internalError(w, "read room platforms", err)
		return
	}
	out := protocol.Status{Rooms: []protocol.RoomStatus{}}
	for _, room := range rooms {
		rs := protocol.RoomStatus{ID: room.ID, Name: room.Name, Provider: room.Provider, Machine: room.Machine,
			OS: room.OS, Arch: room.Arch, State: room.State, Reason: room.BrokenReason, Drained: room.Drained,
			Slots: []protocol.SlotStatus{}, Platforms: []string{}, Deadline: room.ProviderDeadline}
		for _, sl := range slots {
			if sl.RoomID == room.ID {
				rs.Slots = append(rs.Slots, protocol.SlotStatus{Name: sl.Name, State: sl.State, LeaseID: sl.LeaseID})
			}
		}
		for _, rp := range rps {
			if rp.RoomID == room.ID {
				rs.Platforms = append(rs.Platforms, rp.Platform)
			}
		}
		out.Rooms = append(out.Rooms, rs)
	}
	leases, err := s.store.ActiveLeases(ctx)
	if err != nil {
		s.internalError(w, "read leases", err)
		return
	}
	out.Leases = []protocol.LeaseStatus{}
	for _, l := range leases {
		out.Leases = append(out.Leases, protocol.LeaseStatus{ID: l.ID, State: l.State, Position: l.Position, Reason: l.Reason,
			Platform: l.Platform, Devices: l.Devices, Task: l.Task, Priority: protocol.PriorityName(l.Priority), Guest: l.GuestHost,
			Repo: l.Repo, Provider: l.Provider, Machine: l.Machine, RoomID: l.RoomID, Slot: l.SlotName, CreatedAt: l.CreatedAt,
			ExpiresAt: l.ExpiresAt})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var req protocol.CreateTokenRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "", "name is required")
		return
	}
	switch req.Role {
	case protocol.RoleAdmin, protocol.RoleGuest:
		if req.Machine != "" {
			writeError(w, http.StatusBadRequest, "", "machine is only for room tokens")
			return
		}
	case protocol.RoleRoom:
		m := s.cfg.Machine(req.Machine)
		if m == nil || m.Kind != config.ProviderStatic {
			writeError(w, http.StatusBadRequest, "", fmt.Sprintf("a room token needs machine = <provider>/<machine> of a static machine; %q is not one", req.Machine))
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "", fmt.Sprintf("unknown role %q; use admin, guest, or room", req.Role))
		return
	}
	tok, secret, err := s.store.CreateToken(r.Context(), req.Name, req.Role, req.Machine, time.Now())
	if err != nil {
		s.internalError(w, "create token", err)
		return
	}
	writeJSON(w, http.StatusCreated, protocol.TokenResponse{ID: tok.ID, Name: tok.Name, Role: tok.Role, Machine: tok.Machine, Token: secret})
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "", "the token ID must be a number")
		return
	}
	err = s.store.RevokeToken(r.Context(), id, time.Now())
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "", fmt.Sprintf("no active token with ID %d", id))
		return
	}
	if err != nil {
		s.internalError(w, "revoke token", err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}
