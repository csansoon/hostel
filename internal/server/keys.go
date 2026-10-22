package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Key files in data_dir.
const (
	GatewayHostKeyFile = "gateway_host_ed25519"
	ServerKeyFile      = "server_ed25519"
)

// EnsureKey creates an ed25519 key pair at path (and path.pub) if it does not
// exist. It reports whether it created the key.
func EnsureKey(path, comment string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return false, err
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return false, fmt.Errorf("write key: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return false, err
	}
	line := strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(sshPub)), "\n") + " " + comment + "\n"
	if err := os.WriteFile(path+".pub", []byte(line), 0o644); err != nil {
		return false, fmt.Errorf("write public key: %w", err)
	}
	return true, nil
}

// LoadKey reads a private key that EnsureKey wrote.
func LoadKey(path string) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read key %s: %w (run \"hostel server init\")", path, err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("parse key %s: %w", path, err)
	}
	return signer, nil
}
