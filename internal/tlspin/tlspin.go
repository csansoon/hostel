// Package tlspin makes the server's own TLS certificate and checks it on the
// client by the SHA-256 of its public key (pinning, like an SSH host key), so
// a server without a domain has HTTPS (design: multiplayer.md, section 3).
package tlspin

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Files in data_dir.
const (
	CertFile = "tls_cert.pem"
	KeyFile  = "tls_key.pem"
)

// EnsureCert creates a self-signed certificate in dir if it does not exist.
// Clients pin its key, not its name, so it is valid for 20 years.
func EnsureCert(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, CertFile)); err == nil {
		return nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Hostel server"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(20, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, KeyFile), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return fmt.Errorf("write TLS key: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, CertFile), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return fmt.Errorf("write TLS certificate: %w", err)
	}
	return nil
}

// Load reads the certificate of EnsureCert and returns it with its key hash.
func Load(dir string) (tls.Certificate, []byte, error) {
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, CertFile), filepath.Join(dir, KeyFile))
	if err != nil {
		return cert, nil, fmt.Errorf("read the TLS certificate in %s: %w (run \"hostel server init\")", dir, err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return cert, nil, err
	}
	return cert, KeyHash(leaf), nil
}

// KeyHash is the SHA-256 of the certificate's SubjectPublicKeyInfo.
func KeyHash(c *x509.Certificate) []byte {
	sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return sum[:]
}

// Fingerprint is the form that people compare and the client stores.
func Fingerprint(hash []byte) string {
	return "sha256:" + base64.RawStdEncoding.EncodeToString(hash)
}

// ParseFingerprint reverses Fingerprint.
func ParseFingerprint(s string) ([]byte, error) {
	b, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(s, "sha256:"))
	if err != nil || len(b) != sha256.Size || !strings.HasPrefix(s, "sha256:") {
		return nil, fmt.Errorf("%q is not a server key fingerprint (sha256:...)", s)
	}
	return b, nil
}

// ErrMismatch: the server's key is not the pinned key.
var ErrMismatch = errors.New("the server's TLS key is not the pinned key")

// ClientConfig returns a TLS configuration that accepts only a server with
// the pinned key hash (the name and the chain do not matter). A nil hash
// gives the normal checks with the system roots.
func ClientConfig(hash []byte) *tls.Config {
	if hash == nil {
		return &tls.Config{}
	}
	return &tls.Config{
		// The pin replaces the name and chain checks.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || !bytes.Equal(KeyHash(cs.PeerCertificates[0]), hash) {
				return ErrMismatch
			}
			return nil
		},
	}
}
