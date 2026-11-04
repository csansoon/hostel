// Package client calls the Hostel server API.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/tlspin"
	"github.com/csansoon/hostel/internal/version"
)

// requestTimeout bounds each request; long polls set their own deadline.
const requestTimeout = 90 * time.Second

// ServerVersion, when set, gets the server version of each answer
// (X-Hostel-Version), for the update notice.
var ServerVersion func(v string)

type Client struct {
	base  string
	token string
	http  *http.Client
}

// New returns a client for the connection. With a pinned key, it accepts
// only a server with that TLS key.
func New(g *config.Guest) (*Client, error) {
	tlsConfig, err := TLSConfig(g)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	return &Client{base: g.URL(), token: g.Token, http: &http.Client{Timeout: requestTimeout, Transport: transport}}, nil
}

// TLSConfig returns the TLS configuration of the connection (pinned key or
// the system roots).
func TLSConfig(g *config.Guest) (*tls.Config, error) {
	if g.Key == "" {
		return tlspin.ClientConfig(nil), nil
	}
	hash, err := tlspin.ParseFingerprint(g.Key)
	if err != nil {
		return nil, fmt.Errorf("the connection's key: %w; run \"hostel connect\" again", err)
	}
	return tlspin.ClientConfig(hash), nil
}

// Base is the server address of the client.
func (c *Client) Base() string { return c.base }

// APIError is an error response of the server.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string { return e.Message }

// Do sends a request with a JSON body (if in is not nil) and decodes the JSON
// response into out (if not nil).
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set(protocol.VersionHeader, version.Version)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if errors.Is(err, tlspin.ErrMismatch) {
		return fmt.Errorf("the server at %s has another TLS key than the one that this computer pinned: it is not your Hostel server, or the server was set up again. Ask an admin; to trust the new server, run \"hostel connect\" with a new invite code", c.base)
	}
	if err != nil {
		return fmt.Errorf("cannot reach the Hostel server at %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	if v := resp.Header.Get(protocol.VersionHeader); v != "" && ServerVersion != nil {
		ServerVersion(v)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("read the server response: %w", err)
	}
	if resp.StatusCode >= 300 {
		var e protocol.Error
		if json.Unmarshal(data, &e) != nil || e.Error == "" {
			e.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
		}
		return &APIError{Status: resp.StatusCode, Code: e.Code, Message: e.Error}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode the server response: %w", err)
		}
	}
	return nil
}
