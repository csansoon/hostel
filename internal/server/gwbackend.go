package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/csansoon/hostel/internal/gateway"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/roomops"
)

// gwBackend gives the gateway access to the leases and rooms.
type gwBackend struct{ s *Server }

var errNoAccess = errors.New("no access: the lease does not exist, is not ready, or has another key")

func (b gwBackend) Authorize(ctx context.Context, leaseID string, key ssh.PublicKey) (gateway.Target, error) {
	l, err := b.s.store.Lease(ctx, leaseID)
	if err != nil || l.State != protocol.LeaseReady {
		return gateway.Target{}, errNoAccess
	}
	want, _, _, _, err := ssh.ParseAuthorizedKey([]byte(l.PublicKey))
	if err != nil || !bytes.Equal(want.Marshal(), key.Marshal()) {
		return gateway.Target{}, errNoAccess
	}
	r, err := b.s.store.Room(ctx, l.RoomID)
	if err != nil {
		return gateway.Target{}, errNoAccess
	}
	home, err := b.s.slotHome(ctx, r, l.SlotName)
	if err != nil {
		return gateway.Target{}, errNoAccess
	}
	return gateway.Target{HelperPath: roomops.HelperPath(r.OS, home), Shell: r.Shell, Slot: l.SlotName}, nil
}

func (b gwBackend) Dial(ctx context.Context, leaseID string) (*ssh.Client, error) {
	l, err := b.s.store.Lease(ctx, leaseID)
	if err != nil {
		return nil, err
	}
	room, _, err := b.s.openSlot(ctx, l.RoomID, l.SlotName)
	if err != nil {
		return nil, err
	}
	return room.Conn.Client(), nil
}

func (b gwBackend) Ready(ctx context.Context, leaseID string) bool {
	l, err := b.s.store.Lease(ctx, leaseID)
	return err == nil && l.State == protocol.LeaseReady
}

func (b gwBackend) Activity(leaseID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := b.s.store.Touch(ctx, leaseID, time.Now()); err != nil {
		b.s.log.Error("record activity", "lease_id", leaseID, "err", err)
	}
}

// AllowDirect allows any room port on a room with one slot; with several
// slots, only the ports of the lease's slot (design, section 14).
func (b gwBackend) AllowDirect(ctx context.Context, leaseID string, port int) bool {
	l, err := b.s.store.Lease(ctx, leaseID)
	if err != nil {
		return false
	}
	r, err := b.s.store.Room(ctx, l.RoomID)
	if err != nil {
		return false
	}
	m, err := b.s.machineOf(r)
	if err != nil {
		return false
	}
	if len(m.Slots) <= 1 {
		return true
	}
	// An exclusive lease holds all slots of the room.
	slots, err := b.s.store.Slots(ctx)
	if err != nil {
		return false
	}
	held := 0
	for _, sl := range slots {
		if sl.RoomID == r.ID && sl.LeaseID == l.ID {
			held++
		}
	}
	if held == len(m.Slots) {
		return true
	}
	for _, sl := range m.Slots {
		if sl.Name == l.SlotName {
			return slices.Contains(sl.Ports, port)
		}
	}
	return false
}

// AllowForward reports whether the lease registered the room port as a tunnel.
func (b gwBackend) AllowForward(ctx context.Context, leaseID string, port int) bool {
	ports, err := b.s.store.LeaseTunnels(ctx, leaseID)
	if err != nil {
		return false
	}
	for _, p := range ports {
		if p == port {
			return true
		}
	}
	return false
}

// AuthorizeRoom accepts a room token whose machine is a static machine with
// connect = true.
func (b gwBackend) AuthorizeRoom(ctx context.Context, token string) (string, int64, error) {
	tok, err := b.s.store.TokenBySecret(ctx, token)
	if err != nil || tok.Role != protocol.RoleRoom {
		return "", 0, errors.New("invalid or revoked room token")
	}
	m := b.s.cfg.Machine(tok.Machine)
	if m == nil || !m.Connect {
		return "", 0, fmt.Errorf("machine %s does not have connect = true", tok.Machine)
	}
	return tok.Machine, tok.ID, nil
}

// RoomConnected refuses a token that was revoked after the handshake, and
// probes the machine's room now if it was unreachable, instead of at the next
// reconcile. A revoke writes the store first and then closes the registered
// connections, so either it sees this connection or this check sees the
// revoke.
func (b gwBackend) RoomConnected(machine string, token int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	active, err := b.s.store.TokenActive(ctx, token)
	if err != nil {
		return err
	}
	if !active {
		return errors.New("the room token was revoked")
	}
	rooms, err := b.s.store.Rooms(ctx)
	if err != nil {
		b.s.log.Warn("room connected: list rooms", "machine", machine, "err", err)
		return nil
	}
	for _, r := range rooms {
		if r.Provider+"/"+r.Machine != machine || r.State != protocol.RoomBroken || r.BrokenReason != reasonUnreachable {
			continue
		}
		if _, err := b.s.store.SetRoomState(ctx, r.ID, []string{protocol.RoomBroken}, protocol.RoomProvisioning, "", time.Now()); err != nil {
			b.s.log.Warn("room connected: probe the room", "room", r.Name, "err", err)
		}
	}
	b.s.kickScheduler()
	return nil
}
