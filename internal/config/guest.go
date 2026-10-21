package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Guest is the guest configuration ~/.config/hostel/config.toml.
// HOSTEL_SERVER and HOSTEL_TOKEN override the file.
type Guest struct {
	Server string `toml:"server"`
	Token  string `toml:"token"`
}

// GuestPath returns ~/.config/hostel/config.toml.
func GuestPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".config", "hostel", "config.toml"), nil
}

// LoadGuest reads the guest configuration and applies the environment overrides.
func LoadGuest() (*Guest, error) {
	g := &Guest{}
	file, err := GuestPath()
	if err != nil {
		return nil, err
	}
	if _, err := decodeFile(file, g); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if v := os.Getenv("HOSTEL_SERVER"); v != "" {
		g.Server = v
	}
	if v := os.Getenv("HOSTEL_TOKEN"); v != "" {
		g.Token = v
	}
	if g.Server == "" || g.Token == "" {
		return nil, fmt.Errorf("no Hostel server or token: set server and token in %s, or set HOSTEL_SERVER and HOSTEL_TOKEN", file)
	}
	return g, nil
}
