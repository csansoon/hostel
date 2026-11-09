// Package machineops has the server-side operations on machines: SSH connection,
// probe, upload, and helper calls (design, section 8).
package machineops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/csansoon/hostel/internal/provider"
)

var (
	// ErrUnreachable: the dial or the SSH handshake failed. It is not a script
	// failure (design, section 6).
	ErrUnreachable = errors.New("machine unreachable")
	// ErrHostKeyChanged: the machine's host key is not the pinned key.
	ErrHostKeyChanged = errors.New("host key changed")
)

// handshakeTimeout bounds the SSH handshake after the dial.
const handshakeTimeout = 30 * time.Second

// Conn is an SSH connection from the server to a machine.
type Conn struct {
	client *ssh.Client
	// HostKey is the machine's host key in authorized_keys format.
	HostKey string
}

// Connect opens an SSH connection. If pinned is not empty, the machine's host key
// must be that key.
func Connect(ctx context.Context, access provider.MachineAccess, pinned string) (*Conn, error) {
	netConn, err := access.Dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	netConn.SetDeadline(time.Now().Add(handshakeTimeout))
	var seen string
	cfg := &ssh.ClientConfig{
		User: access.User,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(access.Signers...)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			seen = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
			if pinned != "" && seen != pinned {
				return ErrHostKeyChanged
			}
			return nil
		},
		Timeout: handshakeTimeout,
	}
	// A proxy connection has no deadlines, so a timer bounds the handshake.
	limit := handshakeTimeout
	if d, ok := ctx.Deadline(); ok && time.Until(d) < limit {
		limit = time.Until(d)
	}
	timer := time.AfterFunc(limit, func() { netConn.Close() })
	c, chans, reqs, err := ssh.NewClientConn(netConn, "machine", cfg)
	timer.Stop()
	if err != nil {
		netConn.Close()
		if errors.Is(err, ErrHostKeyChanged) {
			return nil, fmt.Errorf("%w: pinned %s, got %s", ErrHostKeyChanged, pinned, seen)
		}
		return nil, fmt.Errorf("%w: ssh handshake: %v", ErrUnreachable, err)
	}
	netConn.SetDeadline(time.Time{})
	return &Conn{client: ssh.NewClient(c, chans, reqs), HostKey: seen}, nil
}

// Close closes the connection.
func (c *Conn) Close() error { return c.client.Close() }

// Client returns the SSH client, for port forwarding.
func (c *Conn) Client() *ssh.Client { return c.client }

// maxStderr limits the stderr that Run keeps.
const maxStderr = 64 << 10

// Run runs a command. It returns the exit code of the command; err is set only
// when the command could not run to its end (connection loss, deadline).
func (c *Conn) Run(ctx context.Context, cmd string, stdin io.Reader, stdout io.Writer) (int, string, error) {
	sess, err := c.client.NewSession()
	if err != nil {
		return 0, "", fmt.Errorf("%w: open session: %v", ErrUnreachable, err)
	}
	defer sess.Close()
	var stderr limitedBuffer
	sess.Stdin, sess.Stdout, sess.Stderr = stdin, stdout, &stderr
	if err := sess.Start(cmd); err != nil {
		return 0, "", fmt.Errorf("start command: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		sess.Signal(ssh.SIGKILL)
		sess.Close()
		return 0, stderr.String(), fmt.Errorf("command deadline: %w", ctx.Err())
	}
	var exitErr *ssh.ExitError
	switch {
	case err == nil:
		return 0, stderr.String(), nil
	case errors.As(err, &exitErr):
		return exitErr.ExitStatus(), stderr.String(), nil
	default:
		return 0, stderr.String(), fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
}

type limitedBuffer struct{ b bytes.Buffer }

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if machine := maxStderr - l.b.Len(); machine > 0 {
		l.b.Write(p[:min(len(p), machine)])
	}
	return len(p), nil
}

func (l *limitedBuffer) String() string { return l.b.String() }

// Quote quotes s for a POSIX shell.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
