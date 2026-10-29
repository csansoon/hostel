package provider

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/csansoon/hostel/internal/config"
)

// RoomDialer opens a stream to the SSH server of a static room behind NAT
// through its "hostel room connect" connection ("<provider>/<machine>").
type RoomDialer func(ctx context.Context, machine string) (net.Conn, error)

// Static gives access to real hosts. Each machine is one room from server start.
type Static struct {
	cfg       *config.Provider
	serverKey ssh.Signer
	dialRoom  RoomDialer
}

func NewStatic(cfg *config.Provider, serverKey ssh.Signer, dialRoom RoomDialer) *Static {
	return &Static{cfg: cfg, serverKey: serverKey, dialRoom: dialRoom}
}

func (s *Static) Kind() string                          { return config.ProviderStatic }
func (s *Static) Capabilities(string) Capabilities      { return Capabilities{} }
func (s *Static) Resume(context.Context, RoomRef) error { return errStatic }
func (s *Static) Stop(context.Context, RoomRef) error   { return errStatic }
func (s *Static) Destroy(context.Context, RoomRef) error {
	return errStatic
}
func (s *Static) Extend(context.Context, RoomRef, time.Time) error { return errStatic }
func (s *Static) List(context.Context, string) ([]RoomRef, error)  { return nil, errStatic }
func (s *Static) Provision(context.Context, ProvisionRequest) (RoomRef, error) {
	return RoomRef{}, errStatic
}

func (s *Static) Access(ctx context.Context, room RoomRef) (RoomAccess, error) {
	m := s.cfg.Machines[room.Machine]
	if m == nil {
		return RoomAccess{}, fmt.Errorf("machine %q is not in the configuration", room.Machine)
	}
	user, host, port, err := config.ParseAddress(m.Address)
	if err != nil {
		return RoomAccess{}, err
	}
	if m.Connect {
		// The room connects out; only the user of the address applies.
		key := s.cfg.Name + "/" + room.Machine
		return RoomAccess{
			User: user,
			Dial: func(ctx context.Context) (net.Conn, error) {
				ctx, cancel := context.WithTimeout(ctx, DialTimeout)
				defer cancel()
				return s.dialRoom(ctx, key)
			},
			Signers: []ssh.Signer{s.serverKey},
		}, nil
	}
	return RoomAccess{
		User:    user,
		Dial:    TCPDial(net.JoinHostPort(host, strconv.Itoa(port))),
		Signers: []ssh.Signer{s.serverKey},
	}, nil
}
