package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/roomops"
	"github.com/csansoon/hostel/internal/scheduler"
	"github.com/csansoon/hostel/internal/store"
)

// maxWait bounds a long poll.
const maxWait = 60 * time.Second

// notifier wakes long polls when a lease changes.
type notifier struct {
	mu sync.Mutex
	ch map[string]chan struct{}
}

func (n *notifier) wait(id string) <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ch == nil {
		n.ch = map[string]chan struct{}{}
	}
	c, ok := n.ch[id]
	if !ok {
		c = make(chan struct{})
		n.ch[id] = c
	}
	return c
}

func (n *notifier) notify(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if c, ok := n.ch[id]; ok {
		close(c)
		delete(n.ch, id)
	}
}

func newLeaseID() string {
	b := make([]byte, 13)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func poolTunnels(tunnels []protocol.TunnelRequest) int {
	n := 0
	for _, t := range tunnels {
		if t.RoomPort == 0 {
			n++
		}
	}
	return n
}

func (s *Server) handleCreateLease(w http.ResponseWriter, r *http.Request) {
	var req protocol.CreateLeaseRequest
	if !decodeBody(w, r, &req) {
		return
	}
	// A configuration or tool change takes schedMu for its checks and its
	// swap: the checks of the lease and its insert see one configuration.
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	cfg := s.config()
	tools, code, msg := checkLeaseTools(cfg, req.Tools)
	if code != "" {
		writeError(w, http.StatusUnprocessableEntity, code, msg)
		return
	}
	priority := protocol.PriorityNormal
	if req.Priority != "" {
		v, ok := protocol.PriorityNames[req.Priority]
		if !ok {
			writeError(w, http.StatusBadRequest, "", fmt.Sprintf("unknown priority %q; use low, normal, or high", req.Priority))
			return
		}
		priority = v
	}
	if strings.TrimSpace(req.Task) == "" {
		writeError(w, http.StatusBadRequest, "", "task is required: describe what the lease is for")
		return
	}
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(req.PublicKey)); err != nil {
		writeError(w, http.StatusBadRequest, "", "public_key is not an SSH public key: "+err.Error())
		return
	}
	if msg := checkTunnels(req.Tunnels); msg != "" {
		writeError(w, http.StatusBadRequest, "", msg)
		return
	}
	l := store.Lease{ID: newLeaseID(), TokenID: requestToken(r).ID, Tools: tools, Task: req.Task,
		Priority: priority, Tunnels: req.Tunnels, Fresh: req.Fresh, PublicKey: strings.TrimSpace(req.PublicKey),
		GuestHost: req.GuestHost, Repo: req.Repo, Worktree: req.Worktree, Branch: req.Branch}
	if l.Tunnels == nil {
		l.Tunnels = []protocol.TunnelRequest{}
	}
	view, err := s.loadToolView(r.Context())
	if err != nil {
		s.internalError(w, "read rooms", err)
		return
	}
	machines := view.schedMachines()
	if sreq := leaseRequest(cfg, l); len(scheduler.Candidates(machines, sreq)) == 0 {
		writeError(w, http.StatusUnprocessableEntity, protocol.CodeNoMachine, noMachineMessage(machines, sreq))
		return
	}
	now := time.Now()
	if err := s.store.InsertLease(r.Context(), l, now); err != nil {
		s.internalError(w, "create lease", err)
		return
	}
	s.kickScheduler()
	stored, err := s.store.Lease(r.Context(), l.ID)
	if err != nil {
		s.internalError(w, "read lease", err)
		return
	}
	writeJSON(w, http.StatusCreated, s.apiLease(r, stored))
}

func checkTunnels(tunnels []protocol.TunnelRequest) string {
	seen := map[string]bool{}
	fixed := map[int]string{}
	for _, t := range tunnels {
		if other, ok := fixed[t.RoomPort]; ok && t.RoomPort != 0 {
			return fmt.Sprintf("tunnels %s and %s use the same room port %d", other, t.Name, t.RoomPort)
		}
		if t.RoomPort != 0 {
			fixed[t.RoomPort] = t.Name
		}
		if !config.ValidName(t.Name) {
			return fmt.Sprintf("tunnel name %q must match ^[a-z][a-z0-9-]*$", t.Name)
		}
		if seen[t.Name] {
			return fmt.Sprintf("tunnel %q is listed twice", t.Name)
		}
		seen[t.Name] = true
		if t.RoomPort != 0 && (t.RoomPort < 1024 || t.RoomPort > 65535) {
			return fmt.Sprintf("tunnel %s: room port %d is not between 1024 and 65535", t.Name, t.RoomPort)
		}
	}
	return ""
}

// leaseFor reads a lease and checks that the token may use it.
func (s *Server) leaseFor(w http.ResponseWriter, r *http.Request) (store.Lease, bool) {
	l, err := s.store.Lease(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "", fmt.Sprintf("no lease %q", r.PathValue("id")))
		return l, false
	}
	if err != nil {
		s.internalError(w, "read lease", err)
		return l, false
	}
	tok := requestToken(r)
	if tok.Role != protocol.RoleAdmin && !s.ownLease(r, tok, l) {
		writeError(w, http.StatusForbidden, "", "this lease belongs to another user; only an admin can change it")
		return l, false
	}
	return l, true
}

// ownLease reports whether the lease belongs to the token's user (from any
// of the user's computers).
func (s *Server) ownLease(r *http.Request, tok store.Token, l store.Lease) bool {
	if tok.ID == l.TokenID {
		return true
	}
	owner, err := s.store.TokenUserID(r.Context(), l.TokenID)
	return err == nil && tok.UserID != 0 && owner == tok.UserID
}

func (s *Server) handleGetLease(w http.ResponseWriter, r *http.Request) {
	l, ok := s.leaseFor(w, r)
	if !ok {
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	wait, _ := time.ParseDuration(r.URL.Query().Get("wait"))
	// last_poll_at is set at the start of the poll, so a poll must end before
	// poll_timeout; the guest then polls again.
	wait = min(wait, maxWait, s.config().PollTimeout.D()/2)
	// Only the owner's polls keep a queued lease alive; an admin can look
	// without that effect.
	if requestToken(r).ID == l.TokenID {
		if err := s.store.Poll(r.Context(), l.ID, time.Now()); err != nil {
			s.internalError(w, "record poll", err)
			return
		}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
poll:
	for l.Revision <= since && wait > 0 {
		// Take the channel before the read, so no change is missed.
		changed := s.notify.wait(l.ID)
		var err error
		if l, err = s.store.Lease(r.Context(), l.ID); err != nil {
			s.internalError(w, "read lease", err)
			return
		}
		if l.Revision > since {
			break
		}
		select {
		case <-changed:
		case <-timer.C:
			break poll
		case <-r.Context().Done():
			return
		}
	}
	writeJSON(w, http.StatusOK, s.apiLease(r, l))
}

func (s *Server) handleTouch(w http.ResponseWriter, r *http.Request) {
	l, ok := s.leaseFor(w, r)
	if !ok {
		return
	}
	if store.Final(l.State) || l.State == protocol.LeaseCleaning {
		writeError(w, http.StatusConflict, protocol.CodeLeaseEnded, fmt.Sprintf("lease %s ended (%s: %s)", l.ID, endState(l), l.EndReason))
		return
	}
	if err := s.store.Touch(r.Context(), l.ID, time.Now()); err != nil {
		s.internalError(w, "record activity", err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	l, ok := s.leaseFor(w, r)
	if !ok {
		return
	}
	reason := "released by the guest"
	if tok := requestToken(r); !s.ownLease(r, tok, l) {
		reason = "released by " + tok.Name + " (admin)"
		s.store.AddEventBy(r.Context(), time.Now(), l.ID, 0, "lease", reason, tok.UserID)
	}
	if err := s.endLease(r.Context(), l.ID, reason); err != nil {
		s.internalError(w, "release lease", err)
		return
	}
	l, err := s.store.Lease(r.Context(), l.ID)
	if err != nil {
		s.internalError(w, "read lease", err)
		return
	}
	writeJSON(w, http.StatusOK, s.apiLease(r, l))
}

// endState returns the final state of an ended lease (also during cleaning).
func endState(l store.Lease) string {
	if l.State == protocol.LeaseCleaning {
		return l.EndState
	}
	return l.State
}

// apiLease converts a stored lease to the API form, with the access data when
// the lease is ready.
func (s *Server) apiLease(r *http.Request, l store.Lease) protocol.Lease {
	out := protocol.Lease{ID: l.ID, State: l.State, Revision: l.Revision, Position: l.Position, Reason: l.Reason,
		Tools: nonNil(l.Tools), Started: nonNilStarted(l.Started), Task: l.Task, Priority: protocol.PriorityName(l.Priority),
		Provider: l.Provider, Machine: l.Machine, CreatedAt: l.CreatedAt, EndReason: l.EndReason,
		PollTimeoutMS: s.config().PollTimeout.D().Milliseconds(), IdleTimeoutMS: s.config().IdleTimeout.D().Milliseconds()}
	if l.State == protocol.LeaseQueued {
		out.EstimatedWaitSeconds, out.WaitingBehind = s.leaseWait(r.Context(), l)
	}
	if l.State != protocol.LeaseReady {
		return out
	}
	room, err := s.store.Room(r.Context(), l.RoomID)
	if err != nil {
		return out
	}
	ports, err := s.store.LeaseTunnels(r.Context(), l.ID)
	if err != nil {
		return out
	}
	home, err := s.slotHome(r.Context(), room, l.SlotName)
	if err != nil {
		return out
	}
	out.Access = &protocol.Access{
		GatewayAddress:     s.gatewayAddr,
		GatewayHostKey:     strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.gatewayKey.PublicKey()))),
		HelperPath:         roomops.HelperPath(room.OS, home),
		Slot:               l.SlotName,
		WorkDir:            roomops.WorkDir(room.OS, home, l.SlotName),
		TunnelPorts:        ports,
		RoomOS:             room.OS,
		RoomArch:           room.Arch,
		TunnelCheck:        s.config().Intervals.TunnelCheck.D().String(),
		PortProcessOffline: s.config().PortProcessOffline.D().String(),
		ReadyAt:            l.ReadyAt,
		ExpiresAt:          l.ExpiresAt,
		IdleExpiresAt:      l.LastActivityAt.Add(s.config().IdleTimeout.D()),
	}
	out.Access.Hardware, out.Access.Tools = s.roomFacts(r.Context(), room)
	return out
}

func nonNilStarted(v []protocol.StartedTool) []protocol.StartedTool {
	if v == nil {
		return []protocol.StartedTool{}
	}
	return v
}

// roomFacts returns the hardware and the inventory of a room.
func (s *Server) roomFacts(ctx context.Context, room store.Room) (*protocol.Hardware, []protocol.InventoryTool) {
	rts, err := s.store.RoomTools(ctx)
	if err != nil {
		return nil, []protocol.InventoryTool{}
	}
	rs := roomStatus(s.config(), room, nil, rts)
	return rs.Hardware, rs.Tools
}
