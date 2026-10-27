package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/csansoon/hostel/internal/config"
)

// Deadlines of the devbox CLI calls.
const (
	devboxCreateTimeout = 10 * time.Minute
	devboxCallTimeout   = time.Minute
)

// devboxIdleStop is the idle stop of each devbox. It deletes an ephemeral
// devbox, so it is the provider-side bound of a leaked room (design, section 7,
// deadlines). Namespace stops a devbox about 15 minutes after the last activity
// also with a shorter value (measured in M3).
const devboxIdleStop = "15m"

// errDevboxStop: devboxes are ephemeral, because a stopped macOS devbox keeps
// only its workspace volume (design, section 7).
var errDevboxStop = errors.New("namespace devboxes are ephemeral; they cannot stop, resume, or extend")

var errInstanceStop = errors.New("namespace instances cannot stop or resume")

// Namespace creates Namespace devboxes with the devbox CLI and instances with
// the nsc CLI on the server's PATH (spec.product of each machine). The CLIs
// must be logged in.
type Namespace struct {
	cfg       *config.Provider
	log       *slog.Logger
	serverKey ssh.Signer

	mu    sync.Mutex
	names map[string]*sync.Mutex
	// users caches main_user by devbox name: the gateway connects for each
	// guest command, and "devbox list" takes about 1 s.
	users map[string]string
}

func NewNamespace(cfg *config.Provider, serverKey ssh.Signer, log *slog.Logger) *Namespace {
	return &Namespace{cfg: cfg, log: log, serverKey: serverKey, names: map[string]*sync.Mutex{}, users: map[string]string{}}
}

func (n *Namespace) product(machine string) string {
	m := n.cfg.Machines[machine]
	if m == nil {
		return ""
	}
	p, _ := m.Spec["product"].(string)
	return p
}

// uses reports whether a machine of the provider is of the product, so the
// server needs only the CLIs that its machines use.
func (n *Namespace) uses(product string) bool {
	for name := range n.cfg.Machines {
		if n.product(name) == product {
			return true
		}
	}
	return false
}

// CheckLogin fails when a CLI that the machines need is missing or not logged
// in. The server calls it at start.
func (n *Namespace) CheckLogin(ctx context.Context) error {
	if n.uses(productDevbox) {
		if _, err := n.devbox(ctx, devboxCallTimeout, "auth", "check-login"); err != nil {
			return fmt.Errorf("the devbox CLI is not ready (install it and run \"devbox login\" as the server user): %w", err)
		}
	}
	if n.uses(productInstance) {
		if _, err := n.nsc(ctx, nscCallTimeout, "auth", "check-login"); err != nil {
			return fmt.Errorf("the nsc CLI is not ready (install it and run \"nsc login\" as the server user): %w", err)
		}
	}
	return nil
}

func (n *Namespace) Kind() string { return config.ProviderNamespace }

const (
	productDevbox   = "devbox"
	productInstance = "instance"
)

func (n *Namespace) Capabilities(machine string) Capabilities {
	return Capabilities{OnDemand: true, CanExtend: n.product(machine) == productInstance}
}

func (n *Namespace) Resume(_ context.Context, room RoomRef) error { return n.stopError(room) }
func (n *Namespace) Stop(_ context.Context, room RoomRef) error   { return n.stopError(room) }

func (n *Namespace) stopError(room RoomRef) error {
	if n.product(room.Machine) == productInstance {
		return errInstanceStop
	}
	return errDevboxStop
}

func (n *Namespace) Extend(ctx context.Context, room RoomRef, deadline time.Time) error {
	if n.product(room.Machine) == productInstance {
		return n.extendInstance(ctx, room, deadline)
	}
	return errDevboxStop
}

// devbox runs the CLI with stdin from /dev/null, so it never waits for an
// answer. Its output is not secret; stderr goes to the debug log.
func (n *Namespace) devbox(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "devbox", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if stderr.Len() > 0 {
		n.log.Debug("devbox stderr", "provider", n.cfg.Name, "args", strings.Join(args, " "), "stderr", stderr.String())
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("devbox %s: timed out after %s", args[0], timeout)
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("devbox %s: %w", args[0], ctx.Err())
	}
	if err != nil {
		msg := cliMessage(stderr.String() + "\n" + stdout.String())
		switch {
		// ResourceExhausted is also a rate limit; only the limit text means
		// that the account has no space for another devbox.
		case args[0] == "create" && strings.Contains(msg, "code = ResourceExhausted") && strings.Contains(msg, "devbox limit reached"):
			return nil, fmt.Errorf("devbox %s: %w: %s", args[0], ErrLimit, msg)
		case strings.Contains(msg, "code = NotFound"):
			return nil, fmt.Errorf("devbox %s: %w: %s", args[0], ErrNotFound, msg)
		}
		return nil, fmt.Errorf("devbox %s: %v: %s", args[0], err, msg)
	}
	return stdout.Bytes(), nil
}

// cliMessage returns the error lines of the CLI output, without the update
// notice.
func cliMessage(out string) string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.Contains(l, "new version of devbox") || strings.Contains(l, "devbox update") {
			continue
		}
		lines = append(lines, l)
	}
	return strings.Join(lines, "; ")
}

type devboxInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	MainUser string `json:"main_user"`
}

func (n *Namespace) list(ctx context.Context) ([]devboxInfo, error) {
	out, err := n.devbox(ctx, devboxCallTimeout, "list", "-o", "json")
	if err != nil {
		return nil, err
	}
	// With no devbox, the CLI prints a text line before the JSON.
	start := bytes.Index(out, []byte("\n["))
	if bytes.HasPrefix(out, []byte("[")) {
		start = 0
	} else if start < 0 {
		return nil, fmt.Errorf("devbox list: no JSON in the output: %s", cliMessage(string(out)))
	}
	var list []devboxInfo
	// A decoder ignores text after the JSON.
	if err := json.NewDecoder(bytes.NewReader(out[start:])).Decode(&list); err != nil {
		return nil, fmt.Errorf("devbox list: %w", err)
	}
	return list, nil
}

func (n *Namespace) find(ctx context.Context, name string) (devboxInfo, error) {
	list, err := n.list(ctx)
	if err != nil {
		return devboxInfo{}, err
	}
	for _, d := range list {
		if d.Name == name {
			return d, nil
		}
	}
	return devboxInfo{}, fmt.Errorf("devbox %s: %w", name, ErrNotFound)
}

func (n *Namespace) lock(name string) func() {
	n.mu.Lock()
	m := n.names[name]
	if m == nil {
		m = &sync.Mutex{}
		n.names[name] = m
	}
	n.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// Provision creates an ephemeral devbox. It is idempotent by name: a devbox
// with the name counts as created, also when the create call failed (for
// example because a killed server's call made it). Such a devbox can still
// boot, so Provision then waits until it accepts SSH.
func (n *Namespace) Provision(ctx context.Context, req ProvisionRequest) (RoomRef, error) {
	if n.product(req.Machine) == productInstance {
		return n.provisionInstance(ctx, req)
	}
	ref, created, err := n.create(ctx, req)
	if err != nil || created {
		return ref, err
	}
	return ref, n.waitReady(ctx, ref)
}

// create reports created = true when its own create call made the devbox; the
// CLI returns only when the devbox is ready.
func (n *Namespace) create(ctx context.Context, req ProvisionRequest) (ref RoomRef, created bool, err error) {
	defer n.lock(req.Name)()
	ref = RoomRef{Name: req.Name, Machine: req.Machine}
	if d, err := n.find(ctx, req.Name); err == nil {
		ref.ID = d.ID
		return ref, false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return RoomRef{}, false, err
	}
	str := func(k string) string { s, _ := req.Spec[k].(string); return s }
	_, cerr := n.devbox(ctx, devboxCreateTimeout, "create", "--name", req.Name,
		"--platform", str("platform"), "--size", str("size"), "--image", str("image"),
		"--ephemeral", "--auto_stop_idle_timeout", devboxIdleStop, "--no_checkout",
		"--purpose", "hostel room "+req.Name)
	d, err := n.find(ctx, req.Name)
	switch {
	case err == nil:
		ref.ID = d.ID
		return ref, cerr == nil, nil
	case cerr != nil:
		return RoomRef{}, false, cerr
	default:
		return RoomRef{}, false, fmt.Errorf("devbox create %s: the devbox is not in the list: %w", req.Name, err)
	}
}

// waitReady waits until an SSH handshake with the devbox works, for at most
// the create timeout.
func (n *Namespace) waitReady(ctx context.Context, ref RoomRef) error {
	ctx, cancel := context.WithTimeout(ctx, devboxCreateTimeout)
	defer cancel()
	for {
		err := n.handshake(ctx, ref)
		if err == nil || errors.Is(err, ErrNotFound) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("devbox %s is not ready after %s: %w", ref.Name, devboxCreateTimeout, err)
		case <-time.After(2 * time.Second):
		}
	}
}

func (n *Namespace) handshake(ctx context.Context, ref RoomRef) error {
	a, err := n.Access(ctx, ref)
	if err != nil {
		return err
	}
	conn, err := a.Dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	timer := time.AfterFunc(30*time.Second, func() { conn.Close() })
	defer timer.Stop()
	// Only a check that the devbox accepts the key; the server pins the host
	// key at its first real connection.
	c, chans, reqs, err := ssh.NewClientConn(conn, "devbox", &ssh.ClientConfig{User: a.User,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(a.Signers...)}, HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	if err != nil {
		return err
	}
	go ssh.DiscardRequests(reqs)
	go func() {
		for ch := range chans {
			ch.Reject(ssh.Prohibited, "")
		}
	}()
	return c.Close()
}

// Destroy deletes an instance, or a devbox by name and the SSH files that the
// CLI wrote for it.
func (n *Namespace) Destroy(ctx context.Context, room RoomRef) error {
	if n.product(room.Machine) == productInstance {
		return n.destroyInstance(ctx, room)
	}
	n.mu.Lock()
	delete(n.users, room.Name)
	n.mu.Unlock()
	_, err := n.devbox(ctx, devboxCallTimeout, "delete", room.Name, "--force")
	for _, ext := range []string{".key", ".ssh"} {
		if rerr := os.Remove(n.sshFile(room.Name) + ext); rerr != nil && !errors.Is(rerr, os.ErrNotExist) && err == nil {
			err = rerr
		}
	}
	return err
}

// List lists devboxes and instances. A listed instance has its machine (from
// its label), so Destroy knows the product.
func (n *Namespace) List(ctx context.Context, prefix string) ([]RoomRef, error) {
	var out []RoomRef
	if n.uses(productDevbox) {
		list, err := n.list(ctx)
		if err != nil {
			return nil, err
		}
		for _, d := range list {
			if strings.HasPrefix(d.Name, prefix) {
				out = append(out, RoomRef{Name: d.Name, ID: d.ID})
			}
		}
	}
	if n.uses(productInstance) {
		refs, err := n.listInstances(ctx, prefix)
		if err != nil {
			return nil, err
		}
		out = append(out, refs...)
	}
	return out, nil
}

// key reads the devbox key, and writes the key file again when it is missing:
// Namespace stores the key. The lock keeps a connection from reading a
// half-written file.
func (n *Namespace) key(ctx context.Context, name string) ([]byte, error) {
	defer n.lock(name)()
	if key, err := os.ReadFile(n.sshFile(name) + ".key"); err == nil {
		return key, nil
	}
	if _, err := n.devbox(ctx, devboxCallTimeout, "configure-ssh", name); err != nil {
		return nil, err
	}
	return os.ReadFile(n.sshFile(name) + ".key")
}

// sshFile is the path of the CLI's SSH files of a devbox, without the
// extension.
func (n *Namespace) sshFile(name string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".namespace", "ssh", name+".devbox.namespace")
}

// Access goes through the CLI's SSH proxy with the devbox key: the devbox SSH
// server accepts only that key, not the server key.
func (n *Namespace) Access(ctx context.Context, room RoomRef) (RoomAccess, error) {
	if n.product(room.Machine) == productInstance {
		return n.instanceAccess(room), nil
	}
	n.mu.Lock()
	user := n.users[room.Name]
	n.mu.Unlock()
	if user == "" {
		d, err := n.find(ctx, room.Name)
		if err != nil {
			return RoomAccess{}, err
		}
		user = d.MainUser
		n.mu.Lock()
		n.users[room.Name] = user
		n.mu.Unlock()
	}
	key, err := n.key(ctx, room.Name)
	if err != nil {
		return RoomAccess{}, fmt.Errorf("read the devbox key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return RoomAccess{}, fmt.Errorf("parse the devbox key of %s: %w", room.Name, err)
	}
	return RoomAccess{User: user, Dial: proxyDial([]string{"devbox", "ssh-proxy", room.Name}), Signers: []ssh.Signer{signer}}, nil
}
