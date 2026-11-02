package gateway

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/csansoon/hostel/internal/config"
)

// Dial connects to the gateway: TCP for host:port, or a WebSocket for a ws://
// or wss:// URL (a server that only HTTPS reaches), with tlsConfig for wss.
// timeout bounds the dial.
func Dial(ctx context.Context, addr string, tlsConfig *tls.Config, timeout time.Duration) (net.Conn, error) {
	if !config.IsWebSocketURL(addr) {
		d := net.Dialer{Timeout: timeout}
		return d.DialContext(ctx, "tcp", addr)
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	c, _, err := websocket.Dial(dctx, addr, &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		return nil, err
	}
	// The connection outlives the dial; closing the net.Conn closes it.
	return websocket.NetConn(context.Background(), c, websocket.MessageBinary), nil
}

// Resolve returns the gateway address for a client of server (an http or
// https URL): a server with its own TLS gives only the path (config.GatewayPath),
// which is on the server address that the client uses. It also reports
// whether the gateway is on that server, so the client checks it with the same
// pinned key.
func Resolve(addr, server string) (string, bool) {
	su, err := url.Parse(server)
	if err != nil {
		return addr, false
	}
	if strings.HasPrefix(addr, "/") {
		scheme := "wss"
		if su.Scheme == "http" {
			scheme = "ws"
		}
		return scheme + "://" + su.Host + addr, true
	}
	gu, err := url.Parse(addr)
	return addr, err == nil && config.IsWebSocketURL(addr) && gu.Host == su.Host
}
