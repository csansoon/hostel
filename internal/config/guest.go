package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Guest is the client's connection to one server,
// ~/.config/hostel/config.toml (written by "hostel connect"). HOSTEL_SERVER,
// HOSTEL_TOKEN, and HOSTEL_SERVER_KEY override the file.
type Guest struct {
	ServerID  string   `toml:"server_id,omitempty"`
	Addresses []string `toml:"addresses,omitempty"`
	// Key is the pinned TLS key of a server with its own certificate
	// ("sha256:..."); empty for a server behind a proxy with TLS.
	Key   string `toml:"key,omitempty"`
	Token string `toml:"token"`
	// Server is the address in the form before M8 (one address, no key).
	Server string `toml:"server,omitempty"`
}

// URL is the server address that the client uses: the first address.
func (g *Guest) URL() string {
	if len(g.Addresses) > 0 {
		return strings.TrimSuffix(g.Addresses[0], "/")
	}
	return strings.TrimSuffix(g.Server, "/")
}

// ErrNotConnected: the client has no connection to a server.
var ErrNotConnected = errors.New(`this computer is not connected to a Hostel server.
Ask an admin of the server for an invite code, then run:
  hostel connect <invite code>`)

// GuestPath returns ~/.config/hostel/config.toml.
func GuestPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".config", "hostel", "config.toml"), nil
}

// ReadGuest reads the configuration file only (no environment); a missing file
// gives an empty Guest.
func ReadGuest() (*Guest, error) {
	g := &Guest{}
	file, err := GuestPath()
	if err != nil {
		return nil, err
	}
	if _, err := decodeFile(file, g); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return g, nil
}

// LoadGuest reads the connection and applies the environment overrides.
func LoadGuest() (*Guest, error) {
	g, err := ReadGuest()
	if err != nil {
		return nil, err
	}
	if v := os.Getenv("HOSTEL_SERVER"); v != "" {
		g.Addresses, g.Server = []string{v}, ""
	}
	if v := os.Getenv("HOSTEL_TOKEN"); v != "" {
		g.Token = v
	}
	if v, ok := os.LookupEnv("HOSTEL_SERVER_KEY"); ok {
		g.Key = v
	}
	if g.URL() == "" || g.Token == "" {
		return nil, ErrNotConnected
	}
	return g, nil
}

// SaveGuest writes the connection file (mode 0600: it has the token).
func SaveGuest(g *Guest) error {
	file, err := GuestPath()
	if err != nil {
		return err
	}
	var b bytes.Buffer
	b.WriteString("# The connection to a Hostel server, written by \"hostel connect\".\n")
	if err := toml.NewEncoder(&b).Encode(g); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, b.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// RemoveGuest deletes the connection file.
func RemoveGuest() error {
	file, err := GuestPath()
	if err != nil {
		return err
	}
	if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
