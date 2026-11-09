//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestGatewayWebSocket: with a wss:// gateway_public, guests reach the gateway
// over a WebSocket on the API listener (a server that only HTTPS reaches, for
// example on exe.dev): commands, the port process (a tunnel), and the system
// ssh through "hostel gateway-proxy".
func TestGatewayWebSocket(t *testing.T) {
	t.Parallel()
	free := freePort(t)
	f := newFixture(t, &fixtureOpts{
		serverKeys: "gateway_public = \"wss://127.0.0.1:0/v1/gateway\"\n",
		files: map[string]string{
			"hostel.toml": tunnelRepoConfig,
			".gitignore":  ".env\n",
			".env":        fmt.Sprintf("PORT=%d\n", free),
		},
	})
	f.acquire()
	if r := f.e.mustHostel("run", "--", "echo over-websocket"); r.stdout != "over-websocket\n" {
		t.Fatalf("run: %q", r.stdout)
	}
	localHTTP(t, fmt.Sprintf("127.0.0.1:%d", free), "guest")
	if r := f.machineGet("$HOSTEL_TUNNEL_API_PORT", "/ws"); !strings.Contains(r.stdout, "hello from guest /ws") {
		t.Fatalf("tunnel over the WebSocket gateway: %s", r)
	}
	r := f.e.mustHostel("ssh-config")
	cfg, err := os.ReadFile(strings.TrimSpace(r.stdout))
	if err != nil || !strings.Contains(string(cfg), "ProxyCommand ") || !strings.Contains(string(cfg), " gateway-proxy 'wss://127.0.0.1:") {
		t.Fatalf("ssh_config for a WebSocket gateway: %q, %v", cfg, err)
	}
	if r := f.e.mustHostel("ssh", "--", "echo $HOSTEL_ROOM"); strings.TrimSpace(r.stdout) != "a" {
		t.Fatalf("system ssh through gateway-proxy: %s", r)
	}
}
