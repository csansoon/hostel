package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
			writeError(w, http.StatusForbidden, "", "only an admin can do this; ask an admin of the server")
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

// statusKey is the notifier key of any lease change (the status long poll).
const statusKey = "status"

// maxStatusWait bounds the long poll of GET /v1/status (below common proxy
// timeouts).
const maxStatusWait = 50 * time.Second

// handleStatus answers the status. With since=<revision> and wait=<duration>
// it waits until the revision differs from since (M10).
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	since := r.URL.Query().Get("since")
	wait, _ := time.ParseDuration(r.URL.Query().Get("wait"))
	wait = min(max(wait, 0), maxStatusWait)
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	// Rooms and the configuration change without the hook: look again every
	// 2 s.
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		// Wait for a lease change before reading, so no change between the
		// read and the wait is lost.
		changed := s.notify.wait(statusKey)
		st, leases, err := s.buildStatus(ctx, requestToken(r))
		if err != nil {
			s.internalError(w, "read status", err)
			return
		}
		send := since == "" || st.Revision != since || wait == 0
		if !send {
			select {
			case <-changed:
				continue
			case <-tick.C:
				continue
			case <-deadline.C:
			case <-ctx.Done():
				return
			}
		}
		// The wait estimates are not in the revision (they change with time
		// alone), so they are calculated only for the answer.
		for i, l := range leases {
			if l.State == protocol.LeaseQueued {
				st.Leases[i].EstimatedWaitSeconds, _ = s.leaseWait(ctx, l)
			}
		}
		writeJSON(w, http.StatusOK, st)
		return
	}
}

// buildStatus reads the status without the wait estimates; leases are in the
// order of the status leases.
func (s *Server) buildStatus(ctx context.Context, tok store.Token) (protocol.Status, []store.Lease, error) {
	lc := s.live.Load()
	out := protocol.Status{ServerID: lc.cfg.ServerID, Machines: []protocol.MachineStatus{}, Rooms: []protocol.RoomStatus{}, Leases: []protocol.LeaseStatus{},
		Warning: lc.warning}
	if tok.UserID != 0 {
		out.User, out.Role = tok.Name, tok.Role
	}
	for _, m := range lc.cfg.Machines() {
		ms := machineStatus(m)
		if m.Connect {
			ms.ConnectVersion = s.rooms.Version(m.Key())
		}
		out.Machines = append(out.Machines, ms)
	}
	rooms, err := s.store.Rooms(ctx)
	if err != nil {
		return out, nil, err
	}
	measured, err := s.store.LastHardware(ctx)
	if err != nil {
		return out, nil, err
	}
	for i, m := range lc.cfg.Machines() {
		out.Machines[i].Hardware = machineHardware(m, measured)
	}
	slots, err := s.store.Slots(ctx)
	if err != nil {
		return out, nil, err
	}
	rts, err := s.store.RoomTools(ctx)
	if err != nil {
		return out, nil, err
	}
	for _, room := range rooms {
		out.Rooms = append(out.Rooms, roomStatus(lc.cfg, room, slots, rts))
	}
	leases, err := s.store.ActiveLeases(ctx)
	if err != nil {
		return out, nil, err
	}
	users, err := s.store.TokenUserNames(ctx)
	if err != nil {
		return out, nil, err
	}
	for _, l := range leases {
		out.Leases = append(out.Leases, leaseStatus(l, users))
	}
	data, _ := json.Marshal(out)
	sum := sha256.Sum256(data)
	out.Revision = hex.EncodeToString(sum[:8])
	return out, leases, nil
}

func machineStatus(m *config.Machine) protocol.MachineStatus {
	return protocol.MachineStatus{Key: m.Key(), Kind: m.Kind, OS: m.OS, Tools: append([]string{}, m.Tools...),
		MaxRooms: *m.MaxRooms, Connect: m.Connect}
}

// machineHardware is what a new room of the machine will have: what its
// configuration states, else what its newest room measured (only what does not
// change). measured comes from Store.LastHardware.
func machineHardware(m *config.Machine, measured map[string]string) *protocol.Hardware {
	if cores, gb := m.DeclaredSize(); cores > 0 || gb > 0 {
		return &protocol.Hardware{Cores: cores, MemoryBytes: int64(gb) << 30}
	}
	var hw protocol.Hardware
	if json.Unmarshal([]byte(measured[m.Key()]), &hw) != nil {
		return nil
	}
	hw.DiskFreeBytes = 0
	return &hw
}

func roomStatus(cfg *config.Server, room store.Room, slots []store.Slot, rts []store.RoomTool) protocol.RoomStatus {
	rs := protocol.RoomStatus{ID: room.ID, Name: room.Name, Provider: room.Provider, Machine: room.Machine,
		OS: room.OS, Arch: room.Arch, State: room.State, Reason: room.BrokenReason, Drained: room.Drained,
		Slots: []protocol.SlotStatus{}, Tools: []protocol.InventoryTool{}, Deadline: room.ProviderDeadline}
	for _, sl := range slots {
		if sl.RoomID == room.ID {
			rs.Slots = append(rs.Slots, protocol.SlotStatus{Name: sl.Name, State: sl.State, LeaseID: sl.LeaseID})
		}
	}
	for _, rt := range rts {
		if rt.RoomID == room.ID {
			t := cfg.Tools[rt.Tool]
			rs.Tools = append(rs.Tools, protocol.InventoryTool{Name: rt.Tool, Options: nonNil(splitOptions(rt.Option)), State: rt.State,
				Env: nonNil(rt.Env), Screen: t != nil && t.Screen})
		}
	}
	if room.State == protocol.RoomBroken && room.BrokenReason == reasonUnreachable {
		rs.LastSeen = room.LastSeenAt
	}
	if room.Hardware != "" {
		var hw protocol.Hardware
		if json.Unmarshal([]byte(room.Hardware), &hw) == nil {
			rs.Hardware = &hw
		}
	}
	return rs
}

func leaseStatus(l store.Lease, users map[int64]string) protocol.LeaseStatus {
	return protocol.LeaseStatus{ID: l.ID, State: l.State, Position: l.Position, Reason: l.Reason,
		Tools: nonNil(l.Tools), Started: nonNilStarted(l.Started), Task: l.Task, Priority: protocol.PriorityName(l.Priority), User: users[l.TokenID], Guest: l.GuestHost,
		Repo: l.Repo, Branch: l.Branch, Provider: l.Provider, Machine: l.Machine, RoomID: l.RoomID, Slot: l.SlotName, CreatedAt: l.CreatedAt,
		ReadyAt: l.ReadyAt, ExpiresAt: l.ExpiresAt}
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
	// Users get their tokens with invite codes (hostel connect).
	if req.Role != protocol.RoleRoom {
		writeError(w, http.StatusBadRequest, "", "only room tokens are created here; invite users with \"hostel config users invite\"")
		return
	}
	m := s.config().Machine(req.Machine)
	if m == nil || m.Kind != config.ProviderStatic {
		writeError(w, http.StatusBadRequest, "", fmt.Sprintf("a room token needs machine = <provider>/<machine> of a static machine; %q is not one", req.Machine))
		return
	}
	tok, secret, err := s.store.CreateRoomToken(r.Context(), req.Name, req.Machine, time.Now())
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
	s.rooms.CloseToken(id)
	writeJSON(w, http.StatusOK, struct{}{})
}
