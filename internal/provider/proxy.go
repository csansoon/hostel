package provider

import (
	"context"
	"io"
	"net"
	"os/exec"
	"sync"
	"time"
)

// proxyDial runs a proxy command and uses its stdin and stdout as the
// connection to the room's SSH server.
func proxyDial(argv []string) func(ctx context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		cmd := exec.Command(argv[0], argv[1:]...)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return &cmdConn{cmd: cmd, r: stdout, w: stdin}, nil
	}
}

type cmdConn struct {
	cmd   *exec.Cmd
	r     io.ReadCloser
	w     io.WriteCloser
	close sync.Once
}

func (c *cmdConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *cmdConn) Write(p []byte) (int, error) { return c.w.Write(p) }

// Close can come from several goroutines of the SSH client; a second Wait on
// the same process blocks forever.
func (c *cmdConn) Close() error {
	c.close.Do(func() {
		c.w.Close()
		c.r.Close()
		c.cmd.Process.Kill()
		c.cmd.Wait()
	})
	return nil
}
func (c *cmdConn) LocalAddr() net.Addr              { return proxyAddr{} }
func (c *cmdConn) RemoteAddr() net.Addr             { return proxyAddr{} }
func (c *cmdConn) SetDeadline(time.Time) error      { return nil }
func (c *cmdConn) SetReadDeadline(time.Time) error  { return nil }
func (c *cmdConn) SetWriteDeadline(time.Time) error { return nil }

type proxyAddr struct{}

func (proxyAddr) Network() string { return "proxy" }
func (proxyAddr) String() string  { return "proxy" }
