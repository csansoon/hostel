// Package gateway is the SSH server inside the Hostel server (design, section
// 13). It terminates each guest SSH connection and opens its own connection to
// the lease's machine, so a guest never holds a credential that a machine accepts.
package gateway

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/csansoon/hostel/internal/shell"
)

// Purposes of a guest connection (the part after "." in the user name).
const (
	PurposeCmd   = "cmd"
	PurposePorts = "ports"
)

// Target is where a lease's sessions go.
type Target struct {
	HelperPath string
	Shell      string // the machine's SSH shell (package shell)
	Room       string
}

// Backend connects the gateway to the server state.
type Backend interface {
	// Authorize accepts the key only if it is the key of a ready lease.
	Authorize(ctx context.Context, leaseID string, key ssh.PublicKey) (Target, error)
	// Dial opens an upstream SSH connection to the lease's machine.
	Dial(ctx context.Context, leaseID string) (*ssh.Client, error)
	// Ready reports whether the lease is still ready.
	Ready(ctx context.Context, leaseID string) bool
	// Activity records guest activity: a cmd connection opened or closed, or
	// data went through a tunnel or a forward.
	Activity(leaseID string)
	// TrafficInterval is the shortest time between two activity records for the
	// data of one lease's tunnels and forwards.
	TrafficInterval() time.Duration
	// AllowForward reports whether the lease registered a machine port as a tunnel.
	AllowForward(ctx context.Context, leaseID string, port int) bool
	// AllowDirect reports whether a guest may forward to a machine port: on a machine
	// with several rooms, only to a port of the lease's room.
	AllowDirect(ctx context.Context, leaseID string, port int) bool
	// AuthorizeMachine accepts a machine token of a machine with connect = true and
	// returns the machine ("<provider>/<machine>") and the token ID.
	AuthorizeMachine(ctx context.Context, token string) (string, int64, error)
	// MachineConnected checks again that the token is not revoked (a revoke
	// during the handshake did not see the connection), then asks for a
	// probe of the machine's machine, which was unreachable until now.
	MachineConnected(template string, token int64) error
}

type Gateway struct {
	be       Backend
	log      *slog.Logger
	config   *ssh.ServerConfig
	machines *MachineConns

	mu       sync.Mutex
	conns    map[string]map[*ssh.ServerConn]string // lease → connection → purpose
	forwards map[string]map[*forwardSet]bool       // lease → remote forwards of its ports connections
	traffic  map[string]time.Time                  // lease → last activity record for tunnel or forward data
}

// handshakeTimeout bounds the guest handshake and the upstream dial.
const handshakeTimeout = 30 * time.Second

func New(hostKey ssh.Signer, be Backend, machines *MachineConns, log *slog.Logger) *Gateway {
	g := &Gateway{be: be, log: log, machines: machines, conns: map[string]map[*ssh.ServerConn]string{}, forwards: map[string]map[*forwardSet]bool{}, traffic: map[string]time.Time{}}
	g.config = &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			leaseID, purpose, ok := strings.Cut(meta.User(), ".")
			if !ok || (purpose != PurposeCmd && purpose != PurposePorts) {
				return nil, fmt.Errorf("user must be <lease-id>.cmd or <lease-id>.ports")
			}
			ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
			defer cancel()
			t, err := be.Authorize(ctx, leaseID, key)
			if err != nil {
				return nil, err
			}
			return &ssh.Permissions{Extensions: map[string]string{
				"lease": leaseID, "purpose": purpose, "helper": t.HelperPath, "shell": t.Shell, "room": t.Room}}, nil
		},
		// Machines behind NAT ("hostel helper connect"): user "machine", a machine token
		// as the password.
		PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if meta.User() != "machine" {
				return nil, fmt.Errorf("password authentication is only for the user machine")
			}
			ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
			defer cancel()
			template, id, err := be.AuthorizeMachine(ctx, string(password))
			if err != nil {
				return nil, err
			}
			return &ssh.Permissions{Extensions: map[string]string{"machine": template, "machine_token": strconv.FormatInt(id, 10)}}, nil
		},
		ServerVersion: "SSH-2.0-hostel-gateway",
	}
	g.config.AddHostKey(hostKey)
	return g
}

// Serve accepts guest connections until the listener closes.
func (g *Gateway) Serve(ln net.Listener) error {
	for {
		nc, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go g.handle(nc)
	}
}

// CloseLease closes all guest connections of a lease at once.
func (g *Gateway) CloseLease(leaseID string) {
	g.mu.Lock()
	conns := g.conns[leaseID]
	delete(g.conns, leaseID)
	delete(g.traffic, leaseID)
	g.mu.Unlock()
	for c := range conns {
		c.Close()
	}
}

// CmdConns returns the number of open cmd connections of a lease.
func (g *Gateway) CmdConns(leaseID string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, purpose := range g.conns[leaseID] {
		if purpose == PurposeCmd {
			n++
		}
	}
	return n
}

func (g *Gateway) register(leaseID, purpose string, c *ssh.ServerConn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.conns[leaseID] == nil {
		g.conns[leaseID] = map[*ssh.ServerConn]string{}
	}
	g.conns[leaseID][c] = purpose
}

func (g *Gateway) unregister(leaseID string, c *ssh.ServerConn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.conns[leaseID], c)
	if len(g.conns[leaseID]) == 0 {
		delete(g.conns, leaseID)
		delete(g.traffic, leaseID)
	}
}

func (g *Gateway) handle(nc net.Conn) {
	nc.SetDeadline(time.Now().Add(handshakeTimeout))
	conn, chans, reqs, err := ssh.NewServerConn(nc, g.config)
	if err != nil {
		nc.Close()
		return
	}
	nc.SetDeadline(time.Time{})
	defer conn.Close()
	if template := conn.Permissions.Extensions["machine"]; template != "" {
		token, _ := strconv.ParseInt(conn.Permissions.Extensions["machine_token"], 10, 64)
		g.serveMachine(conn, chans, reqs, template, token)
		return
	}
	leaseID := conn.Permissions.Extensions["lease"]
	purpose := conn.Permissions.Extensions["purpose"]
	target := Target{HelperPath: conn.Permissions.Extensions["helper"], Shell: conn.Permissions.Extensions["shell"], Room: conn.Permissions.Extensions["room"]}

	g.register(leaseID, purpose, conn)
	defer g.unregister(leaseID, conn)
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	ready := g.be.Ready(ctx, leaseID)
	cancel()
	// A release between the authentication and the registration must still
	// close this connection.
	if !ready {
		return
	}
	if purpose == PurposeCmd {
		g.be.Activity(leaseID)
		defer g.be.Activity(leaseID)
	}

	ctx, cancel = context.WithTimeout(context.Background(), handshakeTimeout)
	upstream, err := g.be.Dial(ctx, leaseID)
	cancel()
	if err != nil {
		g.log.Warn("gateway: upstream connection failed", "lease_id", leaseID, "err", err)
		return
	}
	defer upstream.Close()
	go func() {
		// The guest connection ends when the machine connection ends.
		upstream.Wait()
		conn.Close()
	}()

	go g.globalRequests(conn, reqs, upstream, leaseID, purpose)
	for nch := range chans {
		switch nch.ChannelType() {
		case "session":
			go g.session(nch, upstream, target)
		case "direct-tcpip":
			go g.directTCPIP(nch, upstream, leaseID)
		default:
			nch.Reject(ssh.UnknownChannelType, "channel type "+nch.ChannelType()+" is not allowed")
		}
	}
}

// helperCommand wraps a guest command in the helper, so that each guest process
// has the lease environment and HOSTEL_LEASE (cleanup finds it by that variable).
func helperCommand(t Target, args ...string) (string, error) {
	return shell.Command(t.Shell, t.HelperPath, append([]string{"helper"}, args...)...)
}

// stringPayload encodes an SSH string (uint32 length + bytes).
func stringPayload(s string) []byte {
	b := make([]byte, 4+len(s))
	n := len(s)
	b[0], b[1], b[2], b[3] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	copy(b[4:], s)
	return b
}

func parseString(b []byte) (string, bool) {
	if len(b) < 4 {
		return "", false
	}
	n := int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3])
	if len(b) != 4+n {
		return "", false
	}
	return string(b[4:]), true
}

// session relays one guest session to an upstream session. exec and shell
// requests are rewritten into helper commands; other requests pass as they are.
func (g *Gateway) session(nch ssh.NewChannel, upstream *ssh.Client, t Target) {
	up, upReqs, err := upstream.OpenChannel("session", nil)
	if err != nil {
		nch.Reject(ssh.ConnectionFailed, "open a session on the machine: "+err.Error())
		return
	}
	ch, reqs, err := nch.Accept()
	if err != nil {
		up.Close()
		return
	}
	defer ch.Close()
	defer up.Close()

	upDone := make(chan struct{})
	go func() {
		defer close(upDone)
		for r := range upReqs {
			// exit-status, exit-signal, and similar requests go to the guest.
			ch.SendRequest(r.Type, false, r.Payload)
			if r.WantReply {
				r.Reply(false, nil)
			}
		}
	}()
	go func() {
		for r := range reqs {
			payload := r.Payload
			typ := r.Type
			var wrapped string
			var err error
			switch r.Type {
			case "exec":
				cmd, ok := parseString(r.Payload)
				if !ok {
					r.Reply(false, nil)
					continue
				}
				wrapped, err = helperCommand(t, "exec", "--room", t.Room, "--command-b64", base64.StdEncoding.EncodeToString([]byte(cmd)))
			case "shell":
				typ = "exec"
				wrapped, err = helperCommand(t, "shell", "--room", t.Room)
			case "subsystem":
				r.Reply(false, nil)
				continue
			}
			if err != nil {
				r.Reply(false, nil)
				continue
			}
			if wrapped != "" {
				payload = stringPayload(wrapped)
			}
			ok, err := up.SendRequest(typ, r.WantReply, payload)
			if r.WantReply {
				r.Reply(ok && err == nil, nil)
			}
		}
		up.CloseWrite()
	}()

	// The guest can keep stdin open after the command ends, so only the output
	// copies are waited for. Then the exit status arrives before the close.
	go func() { io.Copy(up, ch); up.CloseWrite() }()
	var out sync.WaitGroup
	out.Add(2)
	go func() { defer out.Done(); io.Copy(ch, up) }()
	go func() { defer out.Done(); io.Copy(ch.Stderr(), up.Stderr()) }()
	out.Wait()
	<-upDone
}
