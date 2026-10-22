package config

import (
	"bytes"
	"fmt"
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
}

// ReadPaths reads data_dir, platforms_dir, and listen from server.toml without
// the full validation, and applies the defaults.
func ReadPaths(file string) (Paths, error) {
	var raw struct {
		DataDir      string `toml:"data_dir"`
		PlatformsDir string `toml:"platforms_dir"`
		Listen       string `toml:"listen"`
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
	return Paths{
		DataDir:      absPath(raw.DataDir, base, home),
		PlatformsDir: absPath(raw.PlatformsDir, base, home),
		Listen:       raw.Listen,
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
func Template(serverID string) string {
	return `#:schema ./hostel.schema.json
# Hostel server configuration. Check it with "hostel admin config check".
# docs/design.md, section 9, describes all keys.

server_id = "` + serverID + `"   # [a-z0-9]{1,20}; part of the names of created rooms

# listen = "127.0.0.1:7410"          # HTTP API
# gateway_listen = "127.0.0.1:7422"  # SSH gateway
# data_dir = "~/.local/share/hostel"
# platforms_dir = "~/.config/hostel/platforms"

# Add at least one provider. A static machine is a real host. Give its user the
# server's public key (data_dir/server_ed25519.pub) in authorized_keys.
#
# [providers.home]
# kind = "static"
#
# [providers.home.machines.macbook]
# address = "hostel@macbook.local"
# os = "macos"
# platforms = ["ios"]
# labels = ["xcode-26.5"]
`
}
