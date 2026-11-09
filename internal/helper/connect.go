package helper

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/gateway"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/tlspin"
	"github.com/csansoon/hostel/internal/version"
)

// Deadlines of "hostel helper connect".
const (
	connectTimeout    = 30 * time.Second
	keepaliveInterval = 30 * time.Second
	maxBackoff        = 30 * time.Second
	localDialTimeout  = 10 * time.Second
)

// Connect is "hostel helper connect" (design, section 13): a static machine behind
// NAT keeps an outbound SSH connection to the gateway, and the gateway opens
// streams to the machine's own SSH server through it. It runs until it is stopped
// and connects again after each loss. The token comes from a file or
// HOSTEL_MACHINE_TOKEN, never from the arguments (other users can read them).
func Connect(args []string, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel helper connect", stderr)
	server := fs.String("server", "", "the Hostel server URL, for example https://hostel.example.com")
	tokenFile := fs.String("token-file", "", "a file with the machine token (default: $HOSTEL_MACHINE_TOKEN)")
	sshAddr := fs.String("ssh", "127.0.0.1:22", "the address of the machine's SSH server")
	serverKey := fs.String("server-key", os.Getenv("HOSTEL_SERVER_KEY"), "the pinned TLS key of a server with its own certificate (sha256:...)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	token := os.Getenv("HOSTEL_MACHINE_TOKEN")
	if *tokenFile != "" {
		data, err := os.ReadFile(*tokenFile)
		if err != nil {
			fmt.Fprintf(stderr, "hostel helper connect: read the token file: %v\n", err)
			return 1
		}
		token = strings.TrimSpace(string(data))
	}
	if *server == "" || token == "" {
		fmt.Fprintln(stderr, "hostel helper connect: --server and a machine token (--token-file or HOSTEL_MACHINE_TOKEN) are required; create the token with: hostel admin token create --role machine --machine <provider>/<machine>")
		return 2
	}
	tlsConfig := tlspin.ClientConfig(nil)
	if *serverKey != "" {
		hash, err := tlspin.ParseFingerprint(*serverKey)
		if err != nil {
			fmt.Fprintln(stderr, "hostel helper connect:", err)
			return 2
		}
		tlsConfig = tlspin.ClientConfig(hash)
	}
	logger := log.New(stderr, "hostel helper connect: ", log.LstdFlags)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	backoff := time.Second
	for {
		start := time.Now()
		err := connectOnce(ctx, strings.TrimSuffix(*server, "/"), token, *sshAddr, tlsConfig, logger)
		if ctx.Err() != nil {
			return 0
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		logger.Printf("%v; connect again in %s", err, backoff)
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, maxBackoff)
	}
}

// connectOnce reads the gateway address from the server, connects, and serves
// the gateway's streams until the connection ends.
func connectOnce(ctx context.Context, server, token, sshAddr string, tlsConfig *tls.Config, logger *log.Logger) error {
	info, err := gatewayInfo(ctx, server, token, tlsConfig)
	if err != nil {
		return err
	}
	hostKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(info.GatewayHostKey))
	if err != nil {
		return fmt.Errorf("parse the gateway host key: %w", err)
	}
	addr, same := gateway.Resolve(info.GatewayAddress, server)
	gwTLS := tlspin.ClientConfig(nil)
	if same {
		gwTLS = tlsConfig
	}
	nc, err := gateway.Dial(ctx, addr, gwTLS, connectTimeout)
	if err != nil {
		return fmt.Errorf("connect to the gateway at %s: %w", addr, err)
	}
	nc.SetDeadline(time.Now().Add(connectTimeout))
	conn, chans, reqs, err := ssh.NewClientConn(nc, "hostel-gateway", &ssh.ClientConfig{
		User:            "machine",
		ClientVersion:   gateway.ConnectVersionPrefix + version.Version,
		Auth:            []ssh.AuthMethod{ssh.Password(token)},
		HostKeyCallback: ssh.FixedHostKey(hostKey),
		Timeout:         connectTimeout,
	})
	if err != nil {
		nc.Close()
		return fmt.Errorf("the gateway refused the connection (it needs a machine token of a machine with connect = true): %w", err)
	}
	nc.SetDeadline(time.Time{})
	defer conn.Close()
	logger.Printf("connected to the gateway at %s", addr)
	go ssh.DiscardRequests(reqs)
	go keepalive(ctx, conn)
	for nch := range chans {
		if nch.ChannelType() != gateway.MachineSSHChannel {
			nch.Reject(ssh.UnknownChannelType, "only "+gateway.MachineSSHChannel)
			continue
		}
		go relay(nch, sshAddr, logger)
	}
	return errors.New("the gateway connection ended")
}

func gatewayInfo(ctx context.Context, server, token string, tlsConfig *tls.Config) (protocol.Info, error) {
	var info protocol.Info
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server+"/v1/info", nil)
	if err != nil {
		return info, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return info, fmt.Errorf("read the gateway address from %s: %w", server, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return info, errors.New("the server refused the machine token (revoked, or not a machine token)")
	}
	if resp.StatusCode != http.StatusOK {
		return info, fmt.Errorf("read the gateway address from %s: HTTP %d", server, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return info, fmt.Errorf("read the gateway address: %w", err)
	}
	return info, nil
}

// keepalive closes the connection when the gateway stops answering, so a dead
// connection (for example after a network change) is replaced quickly; the
// requests also keep proxies from closing an idle connection.
func keepalive(ctx context.Context, conn ssh.Conn) {
	t := time.NewTicker(keepaliveInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			conn.Close()
			return
		case <-t.C:
		}
		done := make(chan error, 1)
		go func() {
			_, _, err := conn.SendRequest("keepalive@openssh.com", true, nil)
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				return
			}
		case <-time.After(keepaliveInterval):
			conn.Close()
			return
		}
	}
}

// relay connects one gateway stream to the machine's SSH server.
func relay(nch ssh.NewChannel, sshAddr string, logger *log.Logger) {
	local, err := net.DialTimeout("tcp", sshAddr, localDialTimeout)
	if err != nil {
		nch.Reject(ssh.ConnectionFailed, fmt.Sprintf("the machine's SSH server at %s: %v", sshAddr, err))
		logger.Printf("the machine's SSH server at %s: %v", sshAddr, err)
		return
	}
	ch, reqs, err := nch.Accept()
	if err != nil {
		local.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	done := make(chan struct{}, 2)
	go func() { io.Copy(local, ch); local.(*net.TCPConn).CloseWrite(); done <- struct{}{} }()
	go func() { io.Copy(ch, local); ch.CloseWrite(); done <- struct{}{} }()
	// The SSH connection inside is over when one side ends.
	<-done
	ch.Close()
	local.Close()
}
