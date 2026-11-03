// Package connect connects this computer to a Hostel server with an invite
// code, and disconnects it. "hostel connect", "hostel disconnect", and the
// Hostel app use it.
package connect

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/csansoon/hostel/internal/client"
	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/invite"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/tlspin"
)

// requestTimeout bounds each request to the server.
const requestTimeout = 30 * time.Second

// ComputerName is this computer's name for the server: the host name in the
// form of a Hostel name.
func ComputerName() string {
	host, _ := os.Hostname()
	host, _, _ = strings.Cut(strings.ToLower(host), ".")
	name := strings.Trim(regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(host, "-"), "-")
	if len(name) > 40 {
		name = name[:40]
	}
	if name == "" || !config.ValidName(name) {
		return "computer"
	}
	return name
}

// GitUserName is "git config user.name", or empty.
func GitUserName() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "config", "user.name").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Result is a new connection.
type Result struct {
	Guest    *config.Guest
	Response protocol.ConnectResponse
	Computer string
}

// Connect uses an invite code: it asks each address of the code until one
// answers, and saves the connection. It does not touch an older connection;
// the caller revokes it (Revoke) after the new one is saved.
func Connect(ctx context.Context, code invite.Code, name string) (Result, error) {
	g := &config.Guest{}
	if code.KeyHash != nil {
		g.Key = tlspin.Fingerprint(code.KeyHash)
	}
	req := protocol.ConnectRequest{Secret: code.Secret, Name: name, Computer: ComputerName()}
	var resp protocol.ConnectResponse
	var errs []string
	for i, addr := range code.Addresses {
		g.Addresses = append([]string{addr}, append(append([]string{}, code.Addresses[:i]...), code.Addresses[i+1:]...)...)
		c, err := client.New(g)
		if err != nil {
			return Result{}, err
		}
		rctx, cancel := context.WithTimeout(ctx, requestTimeout)
		err = c.Do(rctx, "POST", "/v1/connect", req, &resp)
		cancel()
		// The server's answer about the code ends the attempts; a server-side
		// failure (a proxy or an outage) lets the next address try.
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status < 500 {
			return Result{}, err
		}
		if err == nil {
			break
		}
		errs = append(errs, err.Error())
	}
	if resp.Token == "" {
		return Result{}, fmt.Errorf("no address of the server answered:\n  %s", strings.Join(errs, "\n  "))
	}
	g.ServerID, g.Token = resp.ServerID, resp.Token
	if err := config.SaveGuest(g); err != nil {
		return Result{}, fmt.Errorf("save the connection: %w", err)
	}
	return Result{Guest: g, Response: resp, Computer: req.Computer}, nil
}

// Revoke deletes a connection's token on its server, so that it does not stay
// valid.
func Revoke(ctx context.Context, g *config.Guest) error {
	c, err := client.New(g)
	if err != nil {
		return err
	}
	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if err := c.Do(rctx, "DELETE", "/v1/me", nil, nil); err != nil {
		return fmt.Errorf("%s did not delete this computer's token (%v); an admin of that server can run: hostel config users remove \"<your name>\" --computer %s", g.URL(), err, ComputerName())
	}
	return nil
}

// Disconnect revokes this computer's token (a failure is returned as warning,
// the connection is removed anyway) and removes the connection file.
func Disconnect(ctx context.Context, g *config.Guest) (warning, err error) {
	warning = Revoke(ctx, g)
	return warning, config.RemoveGuest()
}
