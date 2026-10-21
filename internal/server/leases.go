package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
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

// exclusive reports whether a lease takes all slots of a room (design, section 5).
func exclusive(p *config.Platform, labels []string) bool {
	return p.Exclusive || slices.Contains(labels, "desktop")
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
	p := s.cfg.Platforms[req.Platform]
	if p == nil {
		writeError(w, http.StatusUnprocessableEntity, protocol.CodeUnknownPlatform,
			fmt.Sprintf("unknown platform %q; run \"hostel platforms\" for the list", req.Platform))
		return
	}
	devices, code, msg := checkDevices(p, req.Devices)
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
	labels := mergeLabels(p.Requires, req.RequiredLabels)
	sreq := scheduler.Request{Platform: p.Name, PlatformOS: p.Kind == config.KindOS, Labels: labels,
		PoolTunnels: poolTunnels(req.Tunnels), Fresh: req.Fresh, Exclusive: exclusive(p, labels)}
	if len(scheduler.Candidates(s.schedMachines(), sreq)) == 0 {
		writeError(w, http.StatusUnprocessableEntity, protocol.CodeNoMachine, noMachineMessage(sreq))
		return
	}
	now := time.Now()
	l := store.Lease{ID: newLeaseID(), TokenID: requestToken(r).ID, Platform: p.Name, Devices: devices, Task: req.Task,
		Priority: priority, RequiredLabels: labels, Tunnels: req.Tunnels, Fresh: req.Fresh, PublicKey: strings.TrimSpace(req.PublicKey),
		GuestHost: req.GuestHost, Repo: req.Repo, Worktree: req.Worktree}
	if l.Tunnels == nil {
		l.Tunnels = []protocol.TunnelRequest{}
	}
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

func checkDevices(p *config.Platform, devices []string) ([]string, string, string) {
	if p.Kind == config.KindOS {
		if len(devices) > 0 {
			return nil, protocol.CodeUnknownDevice, fmt.Sprintf("platform %s has no devices", p.Name)
		}
		return []string{}, "", ""
	}
	if len(devices) == 0 {
		return []string{p.Devices[0]}, "", ""
	}
	var out []string
	for _, d := range devices {
		if !slices.Contains(p.Devices, d) {
			return nil, protocol.CodeUnknownDevice, fmt.Sprintf("platform %s has no device %q; devices: %s", p.Name, d, strings.Join(p.Devices, ", "))
		}
		if !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	if len(out) > p.MaxDevices {
		return nil, protocol.CodeTooManyDevices, fmt.Sprintf("platform %s allows at most %d devices in one lease", p.Name, p.MaxDevices)
	}
	return out, "", ""
}

func checkTunnels(tunnels []protocol.TunnelRequest) string {
	seen := map[string]bool{}
	for _, t := range tunnels {
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

func mergeLabels(lists ...[]string) []string {
	out := []string{}
	for _, l := range lists {
		for _, v := range l {
			if !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
	}
	return out
}

func noMachineMessage(r scheduler.Request) string {
	msg := "no machine can serve platform " + r.Platform
	if len(r.Labels) > 0 {
		msg += " with the labels " + strings.Join(r.Labels, ", ")
	}
	if r.PoolTunnels > 0 {
		msg += fmt.Sprintf(" and %d tunnel ports", r.PoolTunnels)
	}
	if r.Fresh {
		msg += " on an on-demand machine (--fresh)"
	}
	return msg + "; run \"hostel platforms\" to see what exists"
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
	if tok.Role != protocol.RoleAdmin && tok.ID != l.TokenID {
		writeError(w, http.StatusForbidden, "", "this lease belongs to another token")
		return l, false
	}
	return l, true
}

func (s *Server) handleGetLease(w http.ResponseWriter, r *http.Request) {
	l, ok := s.leaseFor(w, r)
	if !ok {
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	wait, _ := time.ParseDuration(r.URL.Query().Get("wait"))
	wait = min(wait, maxWait)
	if err := s.store.Poll(r.Context(), l.ID, time.Now()); err != nil {
		s.internalError(w, "record poll", err)
		return
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
	if tok := requestToken(r); tok.ID != l.TokenID {
		reason = "released by an admin (" + tok.Name + ")"
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
		Platform: l.Platform, Devices: l.Devices, Task: l.Task, Priority: protocol.PriorityName(l.Priority),
		Provider: l.Provider, Machine: l.Machine, CreatedAt: l.CreatedAt, EndReason: l.EndReason}
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
	slotDir := room.Home + "/hostel/slots/" + l.SlotName
	out.Access = &protocol.Access{
		GatewayAddress:     s.gatewayAddr,
		GatewayHostKey:     strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.gatewayKey.PublicKey()))),
		HelperPath:         room.Home + "/hostel/bin/hostel",
		Slot:               l.SlotName,
		WorkDir:            slotDir + "/work",
		TunnelPorts:        ports,
		RoomOS:             room.OS,
		RoomArch:           room.Arch,
		TunnelCheck:        s.cfg.Intervals.TunnelCheck.D().String(),
		PortProcessOffline: s.cfg.PortProcessOffline.D().String(),
		ReadyAt:            l.ReadyAt,
		ExpiresAt:          l.ExpiresAt,
		IdleExpiresAt:      l.LastActivityAt.Add(s.cfg.IdleTimeout.D()),
	}
	return out
}
