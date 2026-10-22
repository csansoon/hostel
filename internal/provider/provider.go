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
	// ErrLimit means the provider refused a new room because a limit is reached.
	ErrLimit = errors.New("limit reached")
	// ErrNotFound means the room does not exist at the provider.
	ErrNotFound = errors.New("not found")
)

// Capabilities of a machine.
type Capabilities struct {
	OnDemand  bool
	CanStop   bool
	CanExtend bool
}

// Provider creates, stops, and gives access to rooms.
type Provider interface {
	Kind() string
	Capabilities(machine string) Capabilities
	// Provision is idempotent by name, also under concurrent calls.
	Provision(ctx context.Context, req ProvisionRequest) (RoomRef, error)
	Resume(ctx context.Context, room RoomRef) error
	Stop(ctx context.Context, room RoomRef) error
	Extend(ctx context.Context, room RoomRef, deadline time.Time) error
	// Destroy with an empty ID finds the room by name.
	Destroy(ctx context.Context, room RoomRef) error
	// List returns only rooms whose names start with prefix.
	List(ctx context.Context, prefix string) ([]RoomRef, error)
	Access(ctx context.Context, room RoomRef) (RoomAccess, error)
}

// RoomRef identifies a room at its provider.
type RoomRef struct {
	Name    string // hostel-<server_id>-<room id>, or the machine key for static rooms
	Machine string // machine name in the provider configuration
	ID      string // provider_ref
	State   string // from List: for example running or stopped
}

type ProvisionRequest struct {
	Name      string
	Machine   string
	Spec      map[string]any
	Deadline  time.Time
	ServerKey string // authorized_keys format
}

// RoomAccess is how the server reaches the room's SSH server.
type RoomAccess struct {
	User    string
	Dial    func(ctx context.Context) (net.Conn, error)
	Signers []ssh.Signer
}

// DialTimeout bounds each TCP dial to a room.
const DialTimeout = 15 * time.Second

// TCPDial returns a Dial function for host:port.
func TCPDial(addr string) func(ctx context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		d := net.Dialer{Timeout: DialTimeout}
		return d.DialContext(ctx, "tcp", addr)
	}
}

var errStatic = errors.New("static machines are not created, stopped, or destroyed")
