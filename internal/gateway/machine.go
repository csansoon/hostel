package gateway

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// MachineSSHChannel is the channel type that the gateway opens on a machine's
// connection ("hostel helper connect"): the machine connects it to its own SSH
// server.
const MachineSSHChannel = "hostel-machine-ssh"

// MachineConns are the connections of static machines behind NAT (design, section
// 13), one for each machine ("<provider>/<machine>"). A new connection of a
// machine replaces the old one.
type MachineConns struct {
	mu    sync.Mutex
	conns map[string]machineConn
}

// machineConn is a machine's connection and the ID of the token it authenticated
// with.
type machineConn struct {
	conn  *ssh.ServerConn
	token int64
}

func NewMachineConns() *MachineConns { return &MachineConns{conns: map[string]machineConn{}} }

func (r *MachineConns) set(template string, c machineConn) *ssh.ServerConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.conns[template]
	r.conns[template] = c
	return old.conn
}

func (r *MachineConns) remove(template string, c *ssh.ServerConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns[template].conn == c {
		delete(r.conns, template)
	}
}

// ConnectVersionPrefix starts the SSH client version of "hostel machine
// connect" (and "hostel helper connect"): SSH-2.0-hostel-<version> (M10d).
const ConnectVersionPrefix = "SSH-2.0-hostel-"

// beforeM10d is the version of a connection without the prefix: the only
// release before M10d.
const beforeM10d = "v0.1.0"

// Version is the hostel version of a machine's connection ("" when it is not
// connected; v0.1.0 for a service older than M10d, which sends no version).
func (r *MachineConns) Version(template string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.conns[template]
	if !ok {
		return ""
	}
	v, found := strings.CutPrefix(string(c.conn.ClientVersion()), ConnectVersionPrefix)
	if !found {
		return beforeM10d
	}
	return v
}

// CloseToken closes the connections that authenticated with the token (after
// it was revoked).
func (r *MachineConns) CloseToken(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.conns {
		if c.token == id {
			c.conn.Close()
		}
	}
}

// Dial opens a stream to the machine's SSH server through its connection.
func (r *MachineConns) Dial(ctx context.Context, template string) (net.Conn, error) {
	r.mu.Lock()
	conn := r.conns[template].conn
	r.mu.Unlock()
	if conn == nil {
		return nil, fmt.Errorf("machine %s is not connected: \"hostel helper connect\" does not run on it", template)
	}
	type result struct {
		ch  ssh.Channel
		err error
	}
	res := make(chan result, 1)
	go func() {
		ch, reqs, err := conn.OpenChannel(MachineSSHChannel, nil)
		if err == nil {
			go ssh.DiscardRequests(reqs)
		}
		res <- result{ch, err}
	}()
	select {
	case got := <-res:
		if got.err != nil {
			return nil, fmt.Errorf("open a stream to machine %s: %w", template, got.err)
		}
		return channelConn{got.ch}, nil
	case <-ctx.Done():
		// A machine that does not answer has a dead connection; its connect
		// process connects again.
		conn.Close()
		go func() {
			if got := <-res; got.err == nil {
				got.ch.Close()
			}
		}()
		return nil, fmt.Errorf("open a stream to machine %s: %w", template, ctx.Err())
	}
}

// channelConn is an SSH channel as a net.Conn. It has no deadlines; the
// server's SSH client bounds its handshake with a timer.
type channelConn struct{ ssh.Channel }

func (channelConn) LocalAddr() net.Addr              { return machineAddr{} }
func (channelConn) RemoteAddr() net.Addr             { return machineAddr{} }
func (channelConn) SetDeadline(time.Time) error      { return nil }
func (channelConn) SetReadDeadline(time.Time) error  { return nil }
func (channelConn) SetWriteDeadline(time.Time) error { return nil }

type machineAddr struct{}

func (machineAddr) Network() string { return "hostel-machine" }
func (machineAddr) String() string  { return "hostel-machine" }

// serveMachine keeps a machine's connection until it ends. The machine only answers
// the channels that the gateway opens; it cannot open channels itself.
func (g *Gateway) serveMachine(conn *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request, template string, token int64) {
	if old := g.machines.set(template, machineConn{conn, token}); old != nil {
		old.Close()
	}
	defer g.machines.remove(template, conn)
	if err := g.be.MachineConnected(template, token); err != nil {
		g.log.Warn("gateway: machine connection refused", "machine", template, "err", err)
		return
	}
	g.log.Info("gateway: machine connected", "machine", template)
	defer g.log.Info("gateway: machine disconnected", "machine", template)
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		nch.Reject(ssh.Prohibited, "a machine connection cannot open channels")
	}
}
