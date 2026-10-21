package provider

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"golang.org/x/crypto/ssh"

	"github.com/csansoon/hostel/internal/config"
)

// Static gives access to real hosts. Each machine is one room from server start.
type Static struct {
	cfg       *config.Provider
	serverKey ssh.Signer
}

func NewStatic(cfg *config.Provider, serverKey ssh.Signer) *Static {
	return &Static{cfg: cfg, serverKey: serverKey}
}

func (s *Static) Kind() string { return config.ProviderStatic }

func (s *Static) Access(ctx context.Context, room RoomRef) (RoomAccess, error) {
	m := s.cfg.Machines[room.Machine]
	if m == nil {
		return RoomAccess{}, fmt.Errorf("machine %q is not in the configuration", room.Machine)
	}
	user, host, port, err := config.ParseAddress(m.Address)
	if err != nil {
		return RoomAccess{}, err
	}
	return RoomAccess{
		User:    user,
		Dial:    TCPDial(net.JoinHostPort(host, strconv.Itoa(port))),
		Signers: []ssh.Signer{s.serverKey},
	}, nil
}
