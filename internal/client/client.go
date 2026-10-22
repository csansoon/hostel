// Package client calls the Hostel server API.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
)

// requestTimeout bounds each request; long polls set their own deadline.
const requestTimeout = 90 * time.Second

type Client struct {
	base  string
	token string
	http  *http.Client
}

// New returns a client for the guest configuration.
func New(g *config.Guest) *Client {
	return &Client{base: strings.TrimSuffix(g.Server, "/"), token: g.Token, http: &http.Client{Timeout: requestTimeout}}
}

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
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the Hostel server at %s: %w", c.base, err)
	}
	defer resp.Body.Close()
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
