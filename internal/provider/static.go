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

// MachineDialer opens a stream to the SSH server of a static machine behind NAT
// through its "hostel helper connect" connection ("<provider>/<machine>").
type MachineDialer func(ctx context.Context, template string) (net.Conn, error)

// Static gives access to real hosts. Each machine is one machine from server start.
type Static struct {
	cfg         *config.Provider
	serverKey   ssh.Signer
	dialMachine MachineDialer
}

func NewStatic(cfg *config.Provider, serverKey ssh.Signer, dialMachine MachineDialer) *Static {
	return &Static{cfg: cfg, serverKey: serverKey, dialMachine: dialMachine}
}

func (s *Static) Kind() string                             { return config.ProviderStatic }
func (s *Static) Capabilities(string) Capabilities         { return Capabilities{} }
func (s *Static) Resume(context.Context, MachineRef) error { return errStatic }
func (s *Static) Stop(context.Context, MachineRef) error   { return errStatic }
func (s *Static) Destroy(context.Context, MachineRef) error {
	return errStatic
}
func (s *Static) Extend(context.Context, MachineRef, time.Time) error { return errStatic }
func (s *Static) List(context.Context, string) ([]MachineRef, error)  { return nil, errStatic }
func (s *Static) Provision(context.Context, ProvisionRequest) (MachineRef, error) {
	return MachineRef{}, errStatic
}

func (s *Static) Access(ctx context.Context, machine MachineRef) (MachineAccess, error) {
	m := s.cfg.Templates[machine.Template]
	if m == nil {
		return MachineAccess{}, fmt.Errorf("machine %q is not in the configuration", machine.Template)
	}
	user, host, port, err := config.ParseAddress(m.Address)
	if err != nil {
		return MachineAccess{}, err
	}
	if m.Connect {
		// The machine connects out; only the user of the address applies.
		key := s.cfg.Name + "/" + machine.Template
		return MachineAccess{
			User: user,
			Dial: func(ctx context.Context) (net.Conn, error) {
				ctx, cancel := context.WithTimeout(ctx, DialTimeout)
				defer cancel()
				return s.dialMachine(ctx, key)
			},
			Signers: []ssh.Signer{s.serverKey},
		}, nil
	}
	return MachineAccess{
		User:    user,
		Dial:    TCPDial(net.JoinHostPort(host, strconv.Itoa(port))),
		Signers: []ssh.Signer{s.serverKey},
	}, nil
}
