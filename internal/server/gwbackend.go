package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/csansoon/hostel/internal/gateway"
	"github.com/csansoon/hostel/internal/protocol"
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
	return gateway.Target{HelperPath: r.Home + "/hostel/bin/hostel", Slot: l.SlotName}, nil
}

func (b gwBackend) Dial(ctx context.Context, leaseID string) (*ssh.Client, error) {
	l, err := b.s.store.Lease(ctx, leaseID)
	if err != nil {
		return nil, err
	}
	room, _, err := b.s.openRoom(ctx, l.RoomID)
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

// RoomConnected probes the machine's room now if it was unreachable, instead
// of at the next reconcile.
func (b gwBackend) RoomConnected(machine string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rooms, err := b.s.store.Rooms(ctx)
	if err != nil {
		b.s.log.Warn("room connected: list rooms", "machine", machine, "err", err)
		return
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
}
