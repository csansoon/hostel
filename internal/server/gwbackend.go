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
	"github.com/csansoon/hostel/internal/machineops"
	"github.com/csansoon/hostel/internal/protocol"
)

// gwBackend gives the gateway access to the leases and machines.
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
	r, err := b.s.store.Machine(ctx, l.MachineID)
	if err != nil {
		return gateway.Target{}, errNoAccess
	}
	home, err := b.s.roomHome(ctx, r, l.RoomName)
	if err != nil {
		return gateway.Target{}, errNoAccess
	}
	return gateway.Target{HelperPath: machineops.HelperPath(r.OS, home), Shell: r.Shell, Room: l.RoomName}, nil
}

func (b gwBackend) Dial(ctx context.Context, leaseID string) (*ssh.Client, error) {
	l, err := b.s.store.Lease(ctx, leaseID)
	if err != nil {
		return nil, err
	}
	machine, _, err := b.s.openRoom(ctx, l.MachineID, l.RoomName)
	if err != nil {
		return nil, err
	}
	return machine.Conn.Client(), nil
}

func (b gwBackend) Ready(ctx context.Context, leaseID string) bool {
	l, err := b.s.store.Lease(ctx, leaseID)
	return err == nil && l.State == protocol.LeaseReady
}

// TrafficInterval is 30 s, or a quarter of idle_timeout when that is shorter
// (tests), so that data always renews the lease before it is idle.
func (b gwBackend) TrafficInterval() time.Duration {
	return min(30*time.Second, b.s.config().IdleTimeout.D()/4)
}

func (b gwBackend) Activity(leaseID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := b.s.store.Touch(ctx, leaseID, time.Now()); err != nil {
		b.s.log.Error("record activity", "lease_id", leaseID, "err", err)
	}
}

// AllowDirect allows any machine port on a machine with one room; with several
// rooms, only the ports of the lease's room (design, section 14).
func (b gwBackend) AllowDirect(ctx context.Context, leaseID string, port int) bool {
	l, err := b.s.store.Lease(ctx, leaseID)
	if err != nil {
		return false
	}
	r, err := b.s.store.Machine(ctx, l.MachineID)
	if err != nil {
		return false
	}
	m, err := b.s.templateOf(r)
	if err != nil {
		return false
	}
	if len(m.Rooms) <= 1 {
		return true
	}
	// An exclusive lease holds all rooms of the machine.
	rooms, err := b.s.store.Rooms(ctx)
	if err != nil {
		return false
	}
	held := 0
	for _, sl := range rooms {
		if sl.MachineID == r.ID && sl.LeaseID == l.ID {
			held++
		}
	}
	if held == len(m.Rooms) {
		return true
	}
	for _, sl := range m.Rooms {
		if sl.Name == l.RoomName {
			return slices.Contains(sl.Ports, port)
		}
	}
	return false
}

// AllowForward reports whether the lease registered the machine port as a tunnel.
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

// AuthorizeMachine accepts a machine token whose machine is a static machine with
// connect = true.
func (b gwBackend) AuthorizeMachine(ctx context.Context, token string) (string, int64, error) {
	tok, err := b.s.store.TokenBySecret(ctx, token)
	if err != nil || tok.Role != protocol.RoleMachine {
		return "", 0, errors.New("invalid or revoked machine token")
	}
	m := b.s.config().Template(tok.Template)
	if m == nil || !m.Connect {
		return "", 0, fmt.Errorf("machine %s does not have connect = true", tok.Template)
	}
	return tok.Template, tok.ID, nil
}

// MachineConnected refuses a token that was revoked after the handshake, and
// probes the machine's machine now if it was unreachable, instead of at the next
// reconcile. A revoke writes the store first and then closes the registered
// connections, so either it sees this connection or this check sees the
// revoke.
func (b gwBackend) MachineConnected(template string, token int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	active, err := b.s.store.TokenActive(ctx, token)
	if err != nil {
		return err
	}
	if !active {
		return errors.New("the machine token was revoked")
	}
	machines, err := b.s.store.Machines(ctx)
	if err != nil {
		b.s.log.Warn("machine connected: list machines", "machine", template, "err", err)
		return nil
	}
	for _, r := range machines {
		if r.Provider+"/"+r.Template != template || r.State != protocol.MachineBroken || r.BrokenReason != reasonUnreachable {
			continue
		}
		if _, err := b.s.store.SetMachineState(ctx, r.ID, []string{protocol.MachineBroken}, protocol.MachineProvisioning, "", time.Now()); err != nil {
			b.s.log.Warn("machine connected: probe the machine", "machine", r.Name, "err", err)
		}
	}
	b.s.kickScheduler()
	return nil
}
