package gateway

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// RFC 4254, sections 7.1 and 7.2.
type forwardRequest struct {
	Addr string
	Port uint32
}

type forwardedPayload struct {
	Addr       string
	Port       uint32
	OriginAddr string
	OriginPort uint32
}

type directPayload struct {
	Addr       string
	Port       uint32
	OriginAddr string
	OriginPort uint32
}

// forwardSet has the remote forwards of one ports connection.
type forwardSet struct {
	mu        sync.Mutex
	listeners map[forwardRequest][]net.Listener
}

func (f *forwardSet) set(fr forwardRequest, lns []net.Listener) {
	f.mu.Lock()
	defer f.mu.Unlock()
	closeAll(f.listeners[fr])
	f.listeners[fr] = lns
}

func (f *forwardSet) remove(fr forwardRequest) bool {
	f.mu.Lock()
	lns, ok := f.listeners[fr]
	delete(f.listeners, fr)
	f.mu.Unlock()
	closeAll(lns)
	return ok
}

func closeAll(lns []net.Listener) {
	for _, ln := range lns {
		ln.Close()
	}
}

// RecheckForwards closes the remote forwards of a lease whose machine ports the
// lease no longer registers (after a change of its tunnels). A guest that
// does not cancel a removed tunnel keeps no listener on the machine.
func (g *Gateway) RecheckForwards(ctx context.Context, leaseID string) {
	g.mu.Lock()
	sets := make([]*forwardSet, 0, len(g.forwards[leaseID]))
	for f := range g.forwards[leaseID] {
		sets = append(sets, f)
	}
	g.mu.Unlock()
	for _, f := range sets {
		f.mu.Lock()
		var gone []forwardRequest
		for fr := range f.listeners {
			if !g.be.AllowForward(ctx, leaseID, int(fr.Port)) {
				gone = append(gone, fr)
			}
		}
		f.mu.Unlock()
		for _, fr := range gone {
			g.log.Info("gateway: close a forward that the lease no longer registers", "lease_id", leaseID, "port", fr.Port)
			f.remove(fr)
		}
	}
}

// globalRequests handles the remote forwards of a ports connection: the
// gateway asks the machine's SSH server to listen on 127.0.0.1:<port> and
// [::1]:<port>, only for ports that the lease registered, and relays each
// connection back to the guest.
func (g *Gateway) globalRequests(conn *ssh.ServerConn, reqs <-chan *ssh.Request, upstream *ssh.Client, leaseID, purpose string) {
	fs := &forwardSet{listeners: map[forwardRequest][]net.Listener{}}
	g.mu.Lock()
	if g.forwards[leaseID] == nil {
		g.forwards[leaseID] = map[*forwardSet]bool{}
	}
	g.forwards[leaseID][fs] = true
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.forwards[leaseID], fs)
		if len(g.forwards[leaseID]) == 0 {
			delete(g.forwards, leaseID)
		}
		g.mu.Unlock()
		fs.mu.Lock()
		defer fs.mu.Unlock()
		for _, lns := range fs.listeners {
			closeAll(lns)
		}
	}()
	for req := range reqs {
		switch req.Type {
		case "tcpip-forward":
			var fr forwardRequest
			if purpose != PurposePorts || ssh.Unmarshal(req.Payload, &fr) != nil || fr.Port == 0 {
				req.Reply(false, nil)
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
			allowed := g.be.AllowForward(ctx, leaseID, int(fr.Port))
			cancel()
			if !allowed {
				g.log.Warn("gateway: remote forward of an unregistered port refused", "lease_id", leaseID, "port", fr.Port)
				req.Reply(false, nil)
				continue
			}
			// "localhost" would succeed when only one of the two addresses is
			// free, and a machine service on 127.0.0.1 would then get the tunnel's
			// connections. So 127.0.0.1 must succeed; ::1 is best effort (the
			// machine may have no IPv6 loopback).
			port := strconv.Itoa(int(fr.Port))
			ln4, err := upstream.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
			if err != nil {
				req.Reply(false, nil)
				continue
			}
			lns := []net.Listener{ln4}
			if ln6, err := upstream.Listen("tcp", net.JoinHostPort("::1", port)); err == nil {
				lns = append(lns, ln6)
			}
			fs.set(fr, lns)
			req.Reply(true, nil)
			for _, ln := range lns {
				go g.acceptForwarded(conn, ln, fr)
			}
		case "cancel-tcpip-forward":
			var fr forwardRequest
			if ssh.Unmarshal(req.Payload, &fr) != nil {
				req.Reply(false, nil)
				continue
			}
			req.Reply(fs.remove(fr), nil)
		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
}

func (g *Gateway) acceptForwarded(conn *ssh.ServerConn, ln net.Listener, fr forwardRequest) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			// The originator must be a valid address and port (RFC 4254, 7.2);
			// clients reject port 0.
			origin := forwardedPayload{Addr: fr.Addr, Port: fr.Port, OriginAddr: "127.0.0.1", OriginPort: 1}
			if ra, ok := c.RemoteAddr().(*net.TCPAddr); ok && ra.Port > 0 && ra.IP != nil {
				origin.OriginAddr, origin.OriginPort = ra.IP.String(), uint32(ra.Port)
			}
			ch, reqs, err := conn.OpenChannel("forwarded-tcpip", ssh.Marshal(origin))
			if err != nil {
				g.log.Warn("gateway: forward a tunnel connection to the guest", "port", fr.Port, "err", err)
				return
			}
			go ssh.DiscardRequests(reqs)
			relay(ch, c)
		}()
	}
}

// directTCPIP relays a guest → machine forward. Only the machine's 127.0.0.1 is
// allowed, and on a machine with several rooms only the ports of the lease's room.
func (g *Gateway) directTCPIP(nch ssh.NewChannel, upstream *ssh.Client, leaseID string) {
	var dp directPayload
	if ssh.Unmarshal(nch.ExtraData(), &dp) != nil {
		nch.Reject(ssh.Prohibited, "invalid direct-tcpip request")
		return
	}
	if dp.Addr != "127.0.0.1" && dp.Addr != "localhost" {
		nch.Reject(ssh.Prohibited, "forwards may only go to 127.0.0.1 on the machine")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	allowed := g.be.AllowDirect(ctx, leaseID, int(dp.Port))
	cancel()
	if !allowed {
		nch.Reject(ssh.Prohibited, fmt.Sprintf("port %d is not a port of the lease's room; this machine has several rooms, and a forward may only go to a port of its own room", dp.Port))
		return
	}
	up, err := upstream.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(dp.Port))))
	if err != nil {
		nch.Reject(ssh.ConnectionFailed, "connect to the machine port: "+err.Error())
		return
	}
	defer up.Close()
	ch, reqs, err := nch.Accept()
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	relay(ch, up)
}

type closeWriter interface{ CloseWrite() error }

// relay copies both ways. When one side ends, the other side gets EOF; the
// relay ends when both sides ended or after a short delay.
func relay(a io.ReadWriteCloser, b io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	cp := func(dst io.ReadWriteCloser, src io.Reader) {
		io.Copy(dst, src)
		if cw, ok := dst.(closeWriter); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	select {
	case <-done:
	case <-time.After(30 * time.Second):
	}
	a.Close()
	b.Close()
}
