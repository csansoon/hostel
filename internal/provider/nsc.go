package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Deadlines of the nsc CLI calls. At the plan's concurrency limit, "nsc
// create" does not fail: it blocks without output (measured in M4), so its
// deadline is short and a timeout counts as the limit.
const (
	nscCreateTimeout = 2 * time.Minute
	nscCallTimeout   = time.Minute
	// portForwardTimeout bounds the start of "nsc instance port-forward".
	portForwardTimeout = 30 * time.Second
)

// The room of an instance is a container with OpenSSH on this port: the
// instance's own SSH server (the Namespace agent) refuses remote forwards,
// which tunnels need.
const (
	instanceSSHPort      = 2222
	instanceContainer    = "hostel-room"
	defaultInstanceImage = "ubuntu:24.04"
	instanceUser         = "hostel"
)

// instanceRoomScript runs as root in the room container at each start. It
// installs OpenSSH and sudo when the image does not have them (an image with
// apt-get), creates the room user with the server key, and runs sshd.
const instanceRoomScript = `set -e
if ! [ -x /usr/sbin/sshd ]; then
	export DEBIAN_FRONTEND=noninteractive
	apt-get update -qq
	apt-get install -y -qq --no-install-recommends openssh-server sudo ca-certificates curl git >/dev/null
fi
if ! id hostel >/dev/null 2>&1; then
	useradd --create-home --shell /bin/bash hostel
	usermod --password '*' hostel
fi
if [ -d /etc/sudoers.d ]; then
	echo 'hostel ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/hostel
fi
if ! [ -f /home/hostel/.ssh/authorized_keys ]; then
	install -d -m 700 -o hostel -g hostel /home/hostel/.ssh
	printf '%s\n' "$HOSTEL_AUTHORIZED_KEY" > /home/hostel/.ssh/authorized_keys
	chown hostel:hostel /home/hostel/.ssh/authorized_keys
	chmod 600 /home/hostel/.ssh/authorized_keys
fi
if [ -e /dev/kvm ]; then
	chmod 666 /dev/kvm
fi
ssh-keygen -A
mkdir -p /run/sshd
exec /usr/sbin/sshd -D -e -p "$HOSTEL_SSH_PORT" -o PasswordAuthentication=no -o AllowTcpForwarding=yes`

var listeningRE = regexp.MustCompile(`Listening on (127\.0\.0\.1:\d+)`)

func (n *Namespace) nsc(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "nsc", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if stderr.Len() > 0 {
		n.log.Debug("nsc stderr", "provider", n.cfg.Name, "command", args[0], "stderr", stderr.String())
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("nsc %s: timed out after %s", args[0], timeout)
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("nsc %s: %w", args[0], ctx.Err())
	}
	if err != nil {
		return nil, fmt.Errorf("nsc %s: %v: %s", args[0], err, nscMessage(stderr.String()+"\n"+stdout.String()))
	}
	return stdout.Bytes(), nil
}

// nscMessage returns the error lines of the CLI output, without the update
// notice and the help footer.
func nscMessage(out string) string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "===") || strings.Contains(l, "new version") || strings.Contains(l, "nsc update") ||
			strings.HasPrefix(l, "See https://") || strings.HasPrefix(l, "Agents:") {
			continue
		}
		lines = append(lines, l)
	}
	return strings.Join(lines, "; ")
}

type nscInstance struct {
	ID     string            `json:"cluster_id"`
	Labels map[string]string `json:"labels"`
}

func (n *Namespace) instances(ctx context.Context) ([]nscInstance, error) {
	out, err := n.nsc(ctx, nscCallTimeout, "list", "-o", "json")
	if err != nil {
		return nil, err
	}
	// With no instance, the CLI prints "null".
	var list []nscInstance
	if err := json.Unmarshal(bytes.TrimSpace(out), &list); err != nil {
		return nil, fmt.Errorf("nsc list: %w", err)
	}
	return list, nil
}

func (n *Namespace) findInstance(ctx context.Context, name string) (nscInstance, error) {
	list, err := n.instances(ctx)
	if err != nil {
		return nscInstance{}, err
	}
	for _, i := range list {
		if i.Labels["hostel.name"] == name {
			return i, nil
		}
	}
	return nscInstance{}, fmt.Errorf("instance %s: %w", name, ErrNotFound)
}

// provisionInstance creates a bare instance with the request's deadline as its
// duration, starts the room container, and waits until its SSH server accepts
// the server key. Each step is idempotent by name.
func (n *Namespace) provisionInstance(ctx context.Context, req ProvisionRequest) (RoomRef, error) {
	ref := RoomRef{Name: req.Name, Machine: req.Machine}
	id, err := n.createInstance(ctx, req)
	if err != nil {
		return RoomRef{}, err
	}
	ref.ID = id
	image, _ := req.Spec["image"].(string)
	if image == "" {
		image = defaultInstanceImage
	}
	if _, err := n.nsc(ctx, nscCallTimeout, "ssh", id, "-T", "--", instanceRunCommand(image, req.ServerKey)); err != nil &&
		!strings.Contains(err.Error(), "is already in use") {
		return RoomRef{}, fmt.Errorf("start the room container: %w", err)
	}
	return ref, n.waitReady(ctx, ref)
}

func (n *Namespace) createInstance(ctx context.Context, req ProvisionRequest) (string, error) {
	defer n.lock(req.Name)()
	if i, err := n.findInstance(ctx, req.Name); err == nil {
		return i.ID, nil
	} else if !errors.Is(err, ErrNotFound) {
		return "", err
	}
	// --ssh_key takes a file. The instance's agent accepts the key; the room
	// container gets it in its environment.
	keyFile, err := os.CreateTemp("", "hostel-server-key-*.pub")
	if err != nil {
		return "", err
	}
	defer os.Remove(keyFile.Name())
	if _, err := keyFile.WriteString(req.ServerKey + "\n"); err != nil {
		keyFile.Close()
		return "", err
	}
	if err := keyFile.Close(); err != nil {
		return "", err
	}
	machineType, _ := req.Spec["machine_type"].(string)
	minutes := int(math.Ceil(time.Until(req.Deadline).Minutes()))
	out, cerr := n.nsc(ctx, nscCreateTimeout, "create", "--bare", "--machine_type", machineType,
		"--duration", fmt.Sprintf("%dm", max(minutes, 10)), "--ssh_key", keyFile.Name(),
		"--label", "hostel.name="+req.Name, "--label", "hostel.machine="+req.Machine,
		"--purpose", "hostel room "+req.Name, "-o", "json")
	if cerr == nil {
		var created struct {
			ID string `json:"instance_id"`
		}
		if err := json.Unmarshal(out, &created); err == nil && created.ID != "" {
			return created.ID, nil
		}
	}
	// The call can fail after the instance exists, or a timed-out call can
	// still create it later (reconcile then destroys it).
	if i, err := n.findInstance(ctx, req.Name); err == nil {
		return i.ID, nil
	}
	if cerr != nil && strings.Contains(cerr.Error(), "timed out") {
		return "", fmt.Errorf("%w: nsc create did not return in %s; the plan's concurrency limit is probably reached", ErrLimit, nscCreateTimeout)
	}
	if cerr != nil {
		return "", cerr
	}
	return "", fmt.Errorf("nsc create %s: no instance_id in the output and the instance is not in the list", req.Name)
}

// instanceRunCommand is the shell command on the instance that starts the room
// container. "nsc ssh" runs it with the instance's shell.
func instanceRunCommand(image, serverKey string) string {
	return fmt.Sprintf("docker run -d --name %s --restart unless-stopped --network host $([ -e /dev/kvm ] && echo --device /dev/kvm) "+
		"-e HOSTEL_AUTHORIZED_KEY=%s -e HOSTEL_SSH_PORT=%d --entrypoint /bin/sh %s -c %s",
		instanceContainer, shellQuote(serverKey), instanceSSHPort, shellQuote(image), shellQuote(instanceRoomScript))
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (n *Namespace) destroyInstance(ctx context.Context, room RoomRef) error {
	id := room.ID
	if id == "" {
		i, err := n.findInstance(ctx, room.Name)
		if err != nil {
			return err
		}
		id = i.ID
	}
	_, err := n.nsc(ctx, nscCallTimeout, "destroy", id, "--force")
	return err
}

func (n *Namespace) listInstances(ctx context.Context, prefix string) ([]RoomRef, error) {
	list, err := n.instances(ctx)
	if err != nil {
		return nil, err
	}
	var out []RoomRef
	for _, i := range list {
		if name := i.Labels["hostel.name"]; strings.HasPrefix(name, prefix) {
			out = append(out, RoomRef{Name: name, Machine: i.Labels["hostel.machine"], ID: i.ID})
		}
	}
	return out, nil
}

func (n *Namespace) extendInstance(ctx context.Context, room RoomRef, deadline time.Time) error {
	minutes := int(math.Ceil(time.Until(deadline).Minutes()))
	_, err := n.nsc(ctx, nscCallTimeout, "instance", "extend-duration", room.ID, "--ensure_minimum", fmt.Sprintf("%dm", max(minutes, 1)))
	return err
}

func (n *Namespace) instanceAccess(room RoomRef) RoomAccess {
	return RoomAccess{User: instanceUser, Dial: n.portForwardDial(room.ID), Signers: []ssh.Signer{n.serverKey}}
}

// portForwardDial starts "nsc instance port-forward" to the room's SSH port for
// each connection, connects to the local port that it prints, and stops it
// when the connection closes.
func (n *Namespace) portForwardDial(id string) func(ctx context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		cmd := exec.Command("nsc", "instance", "port-forward", id, "--target_port", fmt.Sprint(instanceSSHPort))
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("nsc instance port-forward: %w", err)
		}
		stop := func() {
			cmd.Process.Kill()
			cmd.Wait()
		}
		addr := make(chan string, 1)
		go func() {
			s := bufio.NewScanner(stdout)
			for s.Scan() {
				if m := listeningRE.FindStringSubmatch(s.Text()); m != nil {
					addr <- m[1]
					break
				}
			}
			close(addr)
			io.Copy(io.Discard, stdout)
		}()
		timer := time.NewTimer(portForwardTimeout)
		defer timer.Stop()
		var a string
		select {
		case a = <-addr:
		case <-ctx.Done():
			stop()
			return nil, ctx.Err()
		case <-timer.C:
			stop()
			return nil, fmt.Errorf("nsc instance port-forward %s: no local port after %s", id, portForwardTimeout)
		}
		if a == "" {
			// stop waits for the process, so stderr is complete.
			stop()
			return nil, fmt.Errorf("nsc instance port-forward %s: %s", id, nscMessage(stderr.String()))
		}
		conn, err := (&net.Dialer{Timeout: DialTimeout}).DialContext(ctx, "tcp", a)
		if err != nil {
			stop()
			return nil, err
		}
		return &forwardConn{Conn: conn, stop: stop}, nil
	}
}

type forwardConn struct {
	net.Conn
	stop  func()
	close sync.Once
}

// Close can come from several goroutines of the SSH client; stop must run once,
// because a second Wait on the same process blocks forever.
func (c *forwardConn) Close() error {
	err := c.Conn.Close()
	c.close.Do(c.stop)
	return err
}
