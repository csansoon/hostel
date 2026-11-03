package config

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Paths has the values that "hostel server init" needs before the configuration
// is complete (it has no providers yet).
type Paths struct {
	DataDir      string
	PlatformsDir string
	Listen       string
	TLS          string
	PublicURLs   []string
}

// ReadPaths reads data_dir, platforms_dir, and listen from server.toml without
// the full validation, and applies the defaults.
func ReadPaths(file string) (Paths, error) {
	var raw struct {
		DataDir      string   `toml:"data_dir"`
		PlatformsDir string   `toml:"platforms_dir"`
		Listen       string   `toml:"listen"`
		TLS          string   `toml:"tls"`
		PublicURLs   []string `toml:"public_urls"`
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return Paths{}, err
	}
	if err := toml.NewDecoder(bytes.NewReader(data)).Decode(&raw); err != nil {
		return Paths{}, positioned(file, err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("find home directory: %w", err)
	}
	base := filepath.Dir(file)
	setStr(&raw.DataDir, filepath.Join(home, ".local", "share", "hostel"))
	setStr(&raw.PlatformsDir, filepath.Join(home, ".config", "hostel", "platforms"))
	setStr(&raw.Listen, "127.0.0.1:7410")
	setStr(&raw.TLS, TLSOff)
	return Paths{
		DataDir:      absPath(raw.DataDir, base, home),
		PlatformsDir: absPath(raw.PlatformsDir, base, home),
		Listen:       raw.Listen,
		TLS:          raw.TLS,
		PublicURLs:   raw.PublicURLs,
	}, nil
}

// DefaultServerID derives a server_id from the host name.
func DefaultServerID() string {
	host, _ := os.Hostname()
	host, _, _ = strings.Cut(strings.ToLower(host), ".")
	id := regexp.MustCompile(`[^a-z0-9]`).ReplaceAllString(host, "")
	if len(id) > 20 {
		id = id[:20]
	}
	if id == "" {
		id = "hostel"
	}
	return id
}

// Template returns the commented server.toml that "hostel server init" writes.
func Template(serverID, listen, tlsMode string, publicURLs []string, helperDir string) string {
	urls := make([]string, len(publicURLs))
	for i, u := range publicURLs {
		urls[i] = fmt.Sprintf("%q", u)
	}
	extra := ""
	// Behind a proxy, guests reach the gateway over WebSocket on the public
	// address (the proxy forwards /v1/gateway to the API).
	if tlsMode == TLSOff && len(publicURLs) > 0 {
		if u, err := url.Parse(publicURLs[0]); err == nil && u.Scheme == "https" {
			extra += fmt.Sprintf("gateway_public = %q\n", "wss://"+u.Host+GatewayPath)
		}
	}
	if helperDir != "" {
		extra += fmt.Sprintf("helper_dir = %q   # the binaries for the rooms (install.sh --server)\n", helperDir)
	}
	return `#:schema ./hostel.schema.json
# Hostel server: the keys that the server needs on this machine. Check the
# file with "hostel admin config check"; docs/design.md, section 9, describes
# all keys.

server_id = "` + serverID + `"   # [a-z0-9]{1,20}; part of the names of created rooms
listen = "` + listen + `"
# self-signed: HTTPS with the server's own certificate (clients pin its key);
# off: plain HTTP behind a proxy that has TLS.
tls = "` + tlsMode + `"
# The addresses that clients use; invite codes contain them.
public_urls = [` + strings.Join(urls, ", ") + `]
` + extra + `
# gateway_listen = "127.0.0.1:7422"  # SSH gateway
# data_dir = "~/.local/share/hostel"

# The providers, machines, settings, and platforms are in the server's
# database: an admin changes them from a client with "hostel config edit" and
# "hostel config platforms" (docs/design.md, sections 8 and 9).
`
}
