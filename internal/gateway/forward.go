package gateway

import (
	"context"
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

// globalRequests handles the remote forwards of a ports connection: the
// gateway asks the room's SSH server to listen on 127.0.0.1:<port> and
// [::1]:<port>, only for ports that the lease registered, and relays each
// connection back to the guest.
func (g *Gateway) globalRequests(conn *ssh.ServerConn, reqs <-chan *ssh.Request, upstream *ssh.Client, leaseID, purpose string) {
	var mu sync.Mutex
	listeners := map[forwardRequest][]net.Listener{}
	closeAll := func(lns []net.Listener) {
		for _, ln := range lns {
			ln.Close()
		}
	}
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for _, lns := range listeners {
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
			// free, and a room service on 127.0.0.1 would then get the tunnel's
			// connections. So 127.0.0.1 must succeed; ::1 is best effort (the
			// room may have no IPv6 loopback).
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
			mu.Lock()
			closeAll(listeners[fr])
			listeners[fr] = lns
			mu.Unlock()
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
			mu.Lock()
			lns := listeners[fr]
			delete(listeners, fr)
			mu.Unlock()
			closeAll(lns)
			req.Reply(lns != nil, nil)
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

// directTCPIP relays a guest → room forward. Only the room's 127.0.0.1 is
// allowed.
func (g *Gateway) directTCPIP(nch ssh.NewChannel, upstream *ssh.Client) {
	var dp directPayload
	if ssh.Unmarshal(nch.ExtraData(), &dp) != nil {
		nch.Reject(ssh.Prohibited, "invalid direct-tcpip request")
		return
	}
	if dp.Addr != "127.0.0.1" && dp.Addr != "localhost" {
		nch.Reject(ssh.Prohibited, "forwards may only go to 127.0.0.1 on the room")
		return
	}
	up, err := upstream.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(dp.Port))))
	if err != nil {
		nch.Reject(ssh.ConnectionFailed, "connect to the room port: "+err.Error())
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
