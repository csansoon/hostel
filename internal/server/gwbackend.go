package server

import (
	"bytes"
	"context"
	"errors"
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
