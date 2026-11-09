// Package provider is the boundary to the sources of machines (design, section 7).
package provider

import (
	"context"
	"errors"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

// Errors that change what the scheduler does.
var (
	// ErrLimit means the provider refused a new machine because a limit is reached.
	ErrLimit = errors.New("limit reached")
	// ErrNotFound means the machine does not exist at the provider.
	ErrNotFound = errors.New("not found")
)

// Capabilities of a machine.
type Capabilities struct {
	OnDemand  bool
	CanStop   bool
	CanExtend bool
}

// Provider creates, stops, and gives access to machines.
type Provider interface {
	Kind() string
	Capabilities(template string) Capabilities
	// Provision is idempotent by name, also under concurrent calls.
	Provision(ctx context.Context, req ProvisionRequest) (MachineRef, error)
	Resume(ctx context.Context, machine MachineRef) error
	Stop(ctx context.Context, machine MachineRef) error
	Extend(ctx context.Context, machine MachineRef, deadline time.Time) error
	// Destroy with an empty ID finds the machine by name.
	Destroy(ctx context.Context, machine MachineRef) error
	// List returns only machines whose names start with prefix.
	List(ctx context.Context, prefix string) ([]MachineRef, error)
	Access(ctx context.Context, machine MachineRef) (MachineAccess, error)
}

// MachineRef identifies a machine at its provider.
type MachineRef struct {
	Name     string // hostel-<server_id>-<machine id>, or the machine key for static machines
	Template string // machine name in the provider configuration
	ID       string // provider_ref
	State    string // from List: for example running or stopped
}

type ProvisionRequest struct {
	Name      string
	Template  string
	Spec      map[string]any
	Deadline  time.Time
	ServerKey string // authorized_keys format
}

// MachineAccess is how the server reaches the machine's SSH server.
type MachineAccess struct {
	User    string
	Dial    func(ctx context.Context) (net.Conn, error)
	Signers []ssh.Signer
}

// DialTimeout bounds each TCP dial to a machine.
const DialTimeout = 15 * time.Second

// TCPDial returns a Dial function for host:port.
func TCPDial(addr string) func(ctx context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		d := net.Dialer{Timeout: DialTimeout}
		return d.DialContext(ctx, "tcp", addr)
	}
}

var errStatic = errors.New("static machines are not created, stopped, or destroyed")
