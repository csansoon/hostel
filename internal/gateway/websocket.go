package gateway

import (
	"context"
	"net/http"

	"github.com/coder/websocket"
)

// ServeHTTP accepts the gateway's SSH inside a WebSocket (binary messages).
// It is for servers that only HTTPS reaches, for example behind an HTTPS
// proxy on a VPS (docs/m5-implementation.md). The SSH authentication inside is the same
// as on the TCP listener.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	// The connection outlives the request; handle closes it.
	g.handle(websocket.NetConn(context.Background(), c, websocket.MessageBinary))
}
