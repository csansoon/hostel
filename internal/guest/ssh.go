package guest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/csansoon/hostel/internal/protocol"
)

// dialTimeout bounds the connection to the gateway.
const dialTimeout = 30 * time.Second

// ErrConnLost: the SSH connection to the gateway failed or dropped. The remote
// command can still run (jobs continue).
var ErrConnLost = errors.New("connection to the room lost")

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
	d := net.Dialer{Timeout: dialTimeout}
	nc, err := d.DialContext(ctx, "tcp", st.Access.GatewayAddress)
	if err != nil {
		return nil, fmt.Errorf("%w: connect to the Hostel gateway at %s: %v", ErrConnLost, st.Access.GatewayAddress, err)
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

// helperCmd returns the command line of a helper command for the lease slot.
func (c *Conn) helperCmd(args ...string) string {
	cmd := quote(c.access.HelperPath) + " room"
	for _, a := range args {
		cmd += " " + quote(a)
	}
	return cmd + " --slot " + quote(c.access.Slot)
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Run runs a command on the room (the gateway wraps it in the lease
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
		return fmt.Errorf("room command %s failed with code %d: %s", args[0], code, strings.TrimSpace(stderr.String()))
	}
	if out != nil {
		if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
			return fmt.Errorf("room command %s: decode output: %w", args[0], err)
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
