package gateway

import (
	"context"
	"net"
	"time"

	"github.com/coder/websocket"

	"github.com/csansoon/hostel/internal/config"
)

// Dial connects to the gateway: TCP for host:port, or a WebSocket for a ws://
// or wss:// URL (a server that only HTTPS reaches). timeout bounds the dial.
func Dial(ctx context.Context, addr string, timeout time.Duration) (net.Conn, error) {
	if !config.IsWebSocketURL(addr) {
		d := net.Dialer{Timeout: timeout}
		return d.DialContext(ctx, "tcp", addr)
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c, _, err := websocket.Dial(dctx, addr, nil)
	if err != nil {
		return nil, err
	}
	// The connection outlives the dial; closing the net.Conn closes it.
	return websocket.NetConn(context.Background(), c, websocket.MessageBinary), nil
}
