package guest

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/csansoon/hostel/internal/client"
	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/gateway"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/shell"
)

// dialTimeout bounds the connection to the gateway.
const dialTimeout = 30 * time.Second

// ErrConnLost: the SSH connection to the gateway failed or dropped. The remote
// command can still run (jobs continue).
var ErrConnLost = errors.New("connection to the machine lost")

// Conn is an SSH connection to the gateway for one lease.
type Conn struct {
	client *ssh.Client
	access *protocol.Access
}

// Dial connects to the gateway as <lease-id>.<purpose> with the lease key. The
// guest checks only the pinned gateway host key.
func Dial(ctx context.Context, st *State, purpose string) (*Conn, error) {
	if st.Access == nil {
		return nil, errors.New("the lease is not ready")
	}
	signer, err := st.signer()
	if err != nil {
		return nil, err
	}
	hostKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(st.Access.GatewayHostKey))
	if err != nil {
		return nil, fmt.Errorf("parse the gateway host key: %w", err)
	}
	addr, tlsConfig, err := gatewayTarget(st.Access.GatewayAddress)
	if err != nil {
		return nil, err
	}
	nc, err := gateway.Dial(ctx, addr, tlsConfig, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("%w: connect to the Hostel gateway at %s: %v", ErrConnLost, addr, err)
	}
	nc.SetDeadline(time.Now().Add(dialTimeout))
	c, chans, reqs, err := ssh.NewClientConn(nc, st.Access.GatewayAddress, &ssh.ClientConfig{
		User:            st.LeaseID + "." + purpose,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.FixedHostKey(hostKey),
		Timeout:         dialTimeout,
	})
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("%w: the gateway refused the connection (the lease may have ended): %v", ErrConnLost, err)
	}
	nc.SetDeadline(time.Time{})
	return &Conn{client: ssh.NewClient(c, chans, reqs), access: st.Access}, nil
}

func (c *Conn) Close() error { return c.client.Close() }

// gatewayTarget resolves the gateway address against the connection's server
// and returns the TLS configuration: the pinned key when the gateway is on
// the server, else the system roots.
func gatewayTarget(addr string) (string, *tls.Config, error) {
	g, err := config.LoadGuest()
	if err != nil {
		return "", nil, err
	}
	addr, same := gateway.Resolve(addr, g.URL())
	if !same {
		return addr, nil, nil
	}
	tlsConfig, err := client.TLSConfig(g)
	return addr, tlsConfig, err
}

// helperCmd returns the command line of a helper command for the lease room.
// The gateway wraps it in the helper, which runs it with sh, or with
// PowerShell on Windows.
func (c *Conn) helperCmd(args ...string) string {
	kind := shell.SH
	if c.access.MachineOS == config.OSWindows {
		kind = shell.PowerShell
	}
	cmd, _ := shell.Command(kind, c.access.HelperPath, append(append([]string{"helper"}, args...), "--room", c.access.Room)...)
	return cmd
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Run runs a command on the machine (the gateway wraps it in the lease
// environment). It returns the exit code; err is set if the connection failed.
func (c *Conn) Run(ctx context.Context, cmd string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	sess, err := c.client.NewSession()
	if err != nil {
		return 0, fmt.Errorf("%w: open a session: %v", ErrConnLost, err)
	}
	defer sess.Close()
	sess.Stdin, sess.Stdout, sess.Stderr = stdin, stdout, stderr
	if err := sess.Start(cmd); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrConnLost, err)
	}
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		sess.Close()
		return 0, ctx.Err()
	}
	var exitErr *ssh.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exitErr):
		return exitErr.ExitStatus(), nil
	}
	return 0, fmt.Errorf("%w: %v", ErrConnLost, err)
}

// Helper runs a helper control command: in is JSON on stdin (or an io.Reader),
// out receives the JSON output.
func (c *Conn) Helper(ctx context.Context, args []string, in, out any) error {
	var stdin io.Reader
	switch v := in.(type) {
	case nil:
	case io.Reader:
		stdin = v
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		stdin = bytes.NewReader(data)
	}
	var stdout, stderr bytes.Buffer
	code, err := c.Run(ctx, c.helperCmd(args...), stdin, &stdout, &stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		var he protocol.HelperError
		if json.Unmarshal(stdout.Bytes(), &he) == nil && he.Error != "" {
			return errors.New(he.Error)
		}
		return fmt.Errorf("machine command %s failed with code %d: %s", args[0], code, strings.TrimSpace(stderr.String()))
	}
	if out != nil {
		if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
			return fmt.Errorf("machine command %s: decode output: %w", args[0], err)
		}
	}
	return nil
}

// Stream runs a streaming helper command (diff, tar-out, job attach).
func (c *Conn) Stream(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	var stderr bytes.Buffer
	code, err := c.Run(ctx, c.helperCmd(args...), stdin, stdout, &stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		return errors.New(strings.TrimPrefix(strings.TrimSpace(stderr.String()), "hostel: "))
	}
	return nil
}
