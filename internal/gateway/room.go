package gateway

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// RoomSSHChannel is the channel type that the gateway opens on a room's
// connection ("hostel room connect"): the room connects it to its own SSH
// server.
const RoomSSHChannel = "hostel-room-ssh"

// RoomConns are the connections of static rooms behind NAT (design, section
// 13), one for each machine ("<provider>/<machine>"). A new connection of a
// machine replaces the old one.
type RoomConns struct {
	mu    sync.Mutex
	conns map[string]*ssh.ServerConn
}

func NewRoomConns() *RoomConns { return &RoomConns{conns: map[string]*ssh.ServerConn{}} }

func (r *RoomConns) set(machine string, c *ssh.ServerConn) *ssh.ServerConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.conns[machine]
	r.conns[machine] = c
	return old
}

func (r *RoomConns) remove(machine string, c *ssh.ServerConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns[machine] == c {
		delete(r.conns, machine)
	}
}

// Dial opens a stream to the room's SSH server through its connection.
func (r *RoomConns) Dial(ctx context.Context, machine string) (net.Conn, error) {
	r.mu.Lock()
	conn := r.conns[machine]
	r.mu.Unlock()
	if conn == nil {
		return nil, fmt.Errorf("machine %s is not connected: \"hostel room connect\" does not run on it", machine)
	}
	type result struct {
		ch  ssh.Channel
		err error
	}
	res := make(chan result, 1)
	go func() {
		ch, reqs, err := conn.OpenChannel(RoomSSHChannel, nil)
		if err == nil {
			go ssh.DiscardRequests(reqs)
		}
		res <- result{ch, err}
	}()
	select {
	case got := <-res:
		if got.err != nil {
			return nil, fmt.Errorf("open a stream to machine %s: %w", machine, got.err)
		}
		return channelConn{got.ch}, nil
	case <-ctx.Done():
		// A room that does not answer has a dead connection; its connect
		// process connects again.
		conn.Close()
		go func() {
			if got := <-res; got.err == nil {
				got.ch.Close()
			}
		}()
		return nil, fmt.Errorf("open a stream to machine %s: %w", machine, ctx.Err())
	}
}

// channelConn is an SSH channel as a net.Conn. It has no deadlines; the
// server's SSH client bounds its handshake with a timer.
type channelConn struct{ ssh.Channel }

func (channelConn) LocalAddr() net.Addr              { return roomAddr{} }
func (channelConn) RemoteAddr() net.Addr             { return roomAddr{} }
func (channelConn) SetDeadline(time.Time) error      { return nil }
func (channelConn) SetReadDeadline(time.Time) error  { return nil }
func (channelConn) SetWriteDeadline(time.Time) error { return nil }

type roomAddr struct{}

func (roomAddr) Network() string { return "hostel-room" }
func (roomAddr) String() string  { return "hostel-room" }

// serveRoom keeps a room's connection until it ends. The room only answers
// the channels that the gateway opens; it cannot open channels itself.
func (g *Gateway) serveRoom(conn *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request, machine string) {
	if old := g.rooms.set(machine, conn); old != nil {
		old.Close()
	}
	defer g.rooms.remove(machine, conn)
	g.log.Info("gateway: room connected", "machine", machine)
	defer g.log.Info("gateway: room disconnected", "machine", machine)
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		nch.Reject(ssh.Prohibited, "a room connection cannot open channels")
	}
}
