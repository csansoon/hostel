package guest

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/csansoon/hostel/internal/client"
	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
)

// TunnelConfig is one tunnel of a lease (machine → guest).
type TunnelConfig struct {
	LocalPort    int    `json:"local_port,omitempty"`
	LocalPortEnv string `json:"local_port_env,omitempty"`
	// EnvValue is the value of LocalPortEnv in the environment of acquire, the
	// last source of the local port.
	EnvValue string `json:"env_value,omitempty"`
	// FixedMachinePort is the requested machine port (0: from the room's pool).
	FixedMachinePort int `json:"fixed_machine_port,omitempty"`
	// MachinePort is the machine port that the server gave.
	MachinePort int `json:"machine_port"`
}

// tunnelsFile is <state>/tunnels.json. Only acquire (before the port process
// starts) and the port process write it.
func tunnelsFile(st *State) string { return filepath.Join(st.Dir, "tunnels.json") }

func loadTunnels(st *State) (map[string]*TunnelConfig, error) {
	out := map[string]*TunnelConfig{}
	data, err := os.ReadFile(tunnelsFile(st))
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	return out, json.Unmarshal(data, &out)
}

func saveTunnels(st *State, t map[string]*TunnelConfig) error {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	tmp := tunnelsFile(st) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, tunnelsFile(st))
}

func tunnelPorts(t map[string]*TunnelConfig) map[string]int {
	out := map[string]int{}
	for n, c := range t {
		out[n] = c.MachinePort
	}
	return out
}

// socketPath is the control socket of the port process of a lease. Unix socket
// paths are limited to 104 bytes on macOS, so the path is short and not in
// $TMPDIR. Go supports Unix sockets on Windows 10 and later too.
func socketPath(stateDir string) (string, error) {
	sum := sha256.Sum256([]byte(stateDir))
	name := hex.EncodeToString(sum[:])[:16] + ".sock"
	dir := filepath.Join("/tmp", fmt.Sprintf("hostel-%d", os.Getuid()))
	if base := os.Getenv("LOCALAPPDATA"); base != "" && os.PathSeparator == '\\' {
		dir = filepath.Join(base, "hostel")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// The directory is in the shared /tmp: another user must not own it.
	if err := checkPrivateDir(dir); err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// portRequest is one request on the control socket.
type portRequest struct {
	Op           string `json:"op"`
	Name         string `json:"name,omitempty"`
	LocalPort    *int   `json:"local_port,omitempty"`
	LocalPortEnv string `json:"local_port_env,omitempty"`
	// EnvValue is the value of LocalPortEnv in the command's environment.
	EnvValue    string `json:"env_value,omitempty"`
	MachinePort *int   `json:"machine_port,omitempty"`
}

type tunnelStatus struct {
	Name        string `json:"name"`
	MachinePort int    `json:"machine_port"`
	Local       string `json:"local"`
	Confirmed   bool   `json:"confirmed"`
	Error       string `json:"error,omitempty"`
	// Problem says why the guest side does not work now; only the "check"
	// request looks at that side.
	Problem string `json:"problem,omitempty"`
}

type forwardStatus struct {
	MachinePort int `json:"machine_port"`
	LocalPort   int `json:"local_port"`
}

type portResponse struct {
	Error    string          `json:"error,omitempty"`
	Tunnels  []tunnelStatus  `json:"tunnels,omitempty"`
	Forwards []forwardStatus `json:"forwards,omitempty"`
	// LocalPort is the local port of a new forward.
	LocalPort int `json:"local_port,omitempty"`
}

// callPorts sends one request to the port process.
func callPorts(st *State, req portRequest) (portResponse, error) {
	var resp portResponse
	path, err := socketPath(st.Dir)
	if err != nil {
		return resp, err
	}
	c, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return resp, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(60 * time.Second))
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return resp, err
	}
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		return resp, err
	}
	if resp.Error != "" {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

// ensurePorts starts the port process of the lease if it does not run.
func ensurePorts(st *State) error {
	if _, err := callPorts(st, portRequest{Op: "ping"}); err == nil {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(st.Dir, "ports.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	if err := startDetached(exe, []string{"ports", "--dir", st.Dir}, logFile); err != nil {
		return fmt.Errorf("start the port process: %w", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := callPorts(st, portRequest{Op: "ping"}); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("the port process did not start; see %s", filepath.Join(st.Dir, "ports.log"))
}

// stopPorts stops the port process of the lease, if it runs.
func stopPorts(st *State) {
	callPorts(st, portRequest{Op: "stop"})
}

// portProc is the port process: one gateway connection with purpose "ports"
// for all tunnels and forwards of a lease (design, section 14).
type portProc struct {
	st   *State
	api  *client.Client
	log  *log.Logger
	stop context.CancelFunc

	mu       sync.Mutex
	tunnels  map[string]*TunnelConfig
	state    map[string]*tunnelStatus
	conn     *Conn
	tunLn    map[string]net.Listener // tunnel name → listener on the machine (through the gateway)
	forwards map[int]net.Listener    // machine port → local listener
}

// Ports is the hidden "hostel ports --dir <state dir>" command.
func Ports(args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 || args[0] != "--dir" {
		fmt.Fprintln(stderr, "hostel: usage: hostel ports --dir <state dir>")
		return ExitUsage
	}
	logger := log.New(stderr, "", log.LstdFlags)
	st := &State{Dir: args[1]}
	data, err := os.ReadFile(st.path())
	if err == nil {
		err = json.Unmarshal(data, st)
	}
	if err != nil || st.Access == nil {
		logger.Printf("ports: no ready lease in %s: %v", st.Dir, err)
		return ExitError
	}
	api, err := apiClient()
	if err != nil {
		logger.Printf("ports: %v", err)
		return ExitError
	}
	tunnels, err := loadTunnels(st)
	if err != nil {
		logger.Printf("ports: %v", err)
		return ExitError
	}
	path, err := socketPath(st.Dir)
	if err != nil {
		logger.Printf("ports: %v", err)
		return ExitError
	}
	if _, err := callPorts(st, portRequest{Op: "ping"}); err == nil {
		return 0 // another port process runs for this lease
	}
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		logger.Printf("ports: listen on %s: %v", path, err)
		return ExitError
	}
	defer os.Remove(path)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	p := &portProc{st: st, api: api, log: logger, stop: cancel, tunnels: tunnels, state: map[string]*tunnelStatus{},
		tunLn: map[string]net.Listener{}, forwards: map[int]net.Listener{}}
	go func() { <-ctx.Done(); ln.Close() }()
	go p.watchLease(ctx)
	go p.connectLoop(ctx)
	logger.Printf("ports: lease %s: started", st.LeaseID)
	for {
		c, err := ln.Accept()
		if err != nil {
			break
		}
		go p.control(ctx, c)
	}
	p.closeAll()
	logger.Printf("ports: lease %s: stopped", st.LeaseID)
	return 0
}

func parseDuration(s string, fallback time.Duration) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return fallback
}

// watchLease reads the lease every tunnel_check (without touch, so the port
// process does not keep a lease alive). It stops the process when the lease is
// no longer ready, or when the server is unreachable for port_process_offline.
func (p *portProc) watchLease(ctx context.Context) {
	every := parseDuration(p.st.Access.TunnelCheck, 30*time.Second)
	offline := parseDuration(p.st.Access.PortProcessOffline, 10*time.Minute)
	lastOK := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		var l protocol.Lease
		err := p.api.Do(rctx, "GET", "/v1/leases/"+p.st.LeaseID, nil, &l)
		cancel()
		var apiErr *client.APIError
		switch {
		case err == nil && l.State != protocol.LeaseReady:
			p.log.Printf("ports: lease %s is %s; stop", p.st.LeaseID, l.State)
			p.stop()
			return
		case errors.As(err, &apiErr) && (apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden):
			p.log.Printf("ports: lease %s: %v; stop", p.st.LeaseID, err)
			p.stop()
			return
		case err != nil:
			if time.Since(lastOK) > offline {
				p.log.Printf("ports: server unreachable for %s; stop", offline)
				p.stop()
				return
			}
		default:
			lastOK = time.Now()
		}
	}
}

// connectLoop keeps the gateway connection and requests the tunnels again
// after each reconnect, with backoff.
func (p *portProc) connectLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		dctx, cancel := context.WithTimeout(ctx, dialTimeout)
		conn, err := Dial(dctx, p.st, "ports")
		cancel()
		if err != nil {
			p.log.Printf("ports: connect: %v; retry in %s", err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 10*time.Second)
			continue
		}
		backoff = time.Second
		p.mu.Lock()
		p.conn = conn
		names := make([]string, 0, len(p.tunnels))
		for n := range p.tunnels {
			names = append(names, n)
		}
		p.mu.Unlock()
		sort.Strings(names)
		for _, n := range names {
			p.listenTunnel(n)
		}
		done := make(chan struct{})
		go func() { conn.client.Wait(); close(done) }()
		select {
		case <-ctx.Done():
			conn.Close()
			return
		case <-done:
		}
		p.log.Printf("ports: gateway connection lost; reconnect")
		p.mu.Lock()
		p.conn = nil
		for n, ln := range p.tunLn {
			ln.Close()
			delete(p.tunLn, n)
			if s := p.state[n]; s != nil {
				s.Confirmed = false
			}
		}
		p.mu.Unlock()
	}
}

// listenTunnel asks the gateway for the remote forward of a tunnel. A refusal
// (for example a machine service uses the port) is recorded in the tunnel state.
func (p *portProc) listenTunnel(name string) error {
	p.mu.Lock()
	cfg, conn := p.tunnels[name], p.conn
	if old := p.tunLn[name]; old != nil {
		old.Close()
		delete(p.tunLn, name)
	}
	p.mu.Unlock()
	if cfg == nil || conn == nil {
		return errors.New("no gateway connection; the port process reconnects")
	}
	ln, err := conn.client.Listen("tcp", net.JoinHostPort("localhost", strconv.Itoa(cfg.MachinePort)))
	p.mu.Lock()
	defer p.mu.Unlock()
	st := &tunnelStatus{Name: name, MachinePort: cfg.MachinePort}
	p.state[name] = st
	if err != nil {
		st.Error = fmt.Sprintf("the machine refused to listen on port %d (a machine service may use it): %v", cfg.MachinePort, err)
		return errors.New(st.Error)
	}
	st.Confirmed = true
	p.tunLn[name] = ln
	go p.serveTunnel(name, ln)
	return nil
}

func (p *portProc) serveTunnel(name string, ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			local, err := p.dialLocal(name)
			if err != nil {
				p.log.Printf("ports: tunnel %s: %v", name, err)
				return
			}
			defer local.Close()
			pipe(c, local)
		}()
	}
}

// localPort finds the local port of a tunnel, at each connection: the explicit
// port, else the variable in the repository's .env, else the value of the
// variable when acquire ran.
func (p *portProc) localPort(name string) (int, error) {
	p.mu.Lock()
	t := p.tunnels[name]
	var cfg TunnelConfig
	if t != nil {
		cfg = *t
	}
	p.mu.Unlock()
	// The tunnel can go while a request or a connection uses it.
	if t == nil {
		return 0, fmt.Errorf("no tunnel %s", name)
	}
	if cfg.LocalPort != 0 {
		return cfg.LocalPort, nil
	}
	value := readDotEnv(filepath.Join(p.st.Root, ".env"))[cfg.LocalPortEnv]
	if value == "" {
		value = cfg.EnvValue
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("no local port: %s is not set in .env or in the environment of acquire", cfg.LocalPortEnv)
	}
	return port, nil
}

func (p *portProc) dialLocal(name string) (net.Conn, error) {
	port, err := p.localPort(name)
	if err != nil {
		return nil, err
	}
	return dialPort(port)
}

func dialPort(port int) (net.Conn, error) {
	// A local server can listen on either loopback address.
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 5*time.Second)
	if err == nil {
		return c, nil
	}
	c, err6 := net.DialTimeout("tcp", net.JoinHostPort("::1", strconv.Itoa(port)), 5*time.Second)
	if err6 == nil {
		return c, nil
	}
	return nil, fmt.Errorf("nothing listens on local port %d: %v", port, err)
}

// checkLocal adds the state of the guest side to a status: the machine side alone
// does not make a tunnel work. It is outside status, which acquire polls.
func (p *portProc) checkLocal(resp portResponse) portResponse {
	for i := range resp.Tunnels {
		t := &resp.Tunnels[i]
		port, err := p.localPort(t.Name)
		if err != nil {
			t.Problem = err.Error()
			continue
		}
		if strings.HasPrefix(t.Local, "$") {
			t.Local = fmt.Sprintf("%s (%d)", t.Local, port)
		}
		c, err := dialPort(port)
		if err != nil {
			t.Problem = fmt.Sprintf("nothing listens on local port %d", port)
			continue
		}
		c.Close()
	}
	return resp
}

// readDotEnv reads KEY=VALUE lines (with optional "export " and quotes).
func readDotEnv(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[strings.TrimSpace(k)] = v
	}
	return out
}

func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	select {
	case <-done:
	case <-time.After(30 * time.Second):
	}
}

func (p *portProc) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ln := range p.tunLn {
		ln.Close()
	}
	for _, ln := range p.forwards {
		ln.Close()
	}
	if p.conn != nil {
		p.conn.Close()
	}
}

func (p *portProc) status() portResponse {
	p.mu.Lock()
	defer p.mu.Unlock()
	var resp portResponse
	names := make([]string, 0, len(p.tunnels))
	for n := range p.tunnels {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		cfg := p.tunnels[n]
		ts := tunnelStatus{Name: n, MachinePort: cfg.MachinePort}
		if s := p.state[n]; s != nil {
			ts.Confirmed, ts.Error = s.Confirmed, s.Error
		}
		if cfg.LocalPort != 0 {
			ts.Local = strconv.Itoa(cfg.LocalPort)
		} else {
			ts.Local = "$" + cfg.LocalPortEnv
		}
		resp.Tunnels = append(resp.Tunnels, ts)
	}
	ports := make([]int, 0, len(p.forwards))
	for rp := range p.forwards {
		ports = append(ports, rp)
	}
	sort.Ints(ports)
	for _, rp := range ports {
		resp.Forwards = append(resp.Forwards, forwardStatus{MachinePort: rp, LocalPort: p.forwards[rp].Addr().(*net.TCPAddr).Port})
	}
	return resp
}

func (p *portProc) control(ctx context.Context, c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(60 * time.Second))
	var req portRequest
	if err := json.NewDecoder(c).Decode(&req); err != nil {
		return
	}
	resp, err := p.handle(ctx, req)
	if err != nil {
		resp.Error = err.Error()
	}
	json.NewEncoder(c).Encode(resp)
}

func (p *portProc) handle(ctx context.Context, req portRequest) (portResponse, error) {
	switch req.Op {
	case "ping":
		return portResponse{}, nil
	case "status":
		return p.status(), nil
	case "check":
		return p.checkLocal(p.status()), nil
	case "stop":
		p.stop()
		return portResponse{}, nil
	case "tunnel_add", "tunnel_set", "tunnel_remove":
		resp, err := p.changeTunnel(ctx, req)
		if err != nil {
			return resp, err
		}
		return p.checkLocal(resp), nil
	case "forward_add":
		return p.addForward(req)
	case "forward_stop":
		if req.MachinePort == nil {
			return portResponse{}, errors.New("forward stop needs a machine port")
		}
		p.mu.Lock()
		ln := p.forwards[*req.MachinePort]
		delete(p.forwards, *req.MachinePort)
		p.mu.Unlock()
		if ln == nil {
			return portResponse{}, fmt.Errorf("no forward for machine port %d", *req.MachinePort)
		}
		ln.Close()
		return p.status(), nil
	}
	return portResponse{}, fmt.Errorf("unknown request %q", req.Op)
}

// changeTunnel adds, changes, or removes a tunnel: it registers the new list
// at the server, opens the forward on the machine, and writes lease.env for new
// jobs. A refused forward undoes the registration.
func (p *portProc) changeTunnel(ctx context.Context, req portRequest) (portResponse, error) {
	p.mu.Lock()
	old := map[string]*TunnelConfig{}
	for n, c := range p.tunnels {
		cc := *c
		old[n] = &cc
	}
	next := map[string]*TunnelConfig{}
	for n, c := range old {
		cc := *c
		next[n] = &cc
	}
	p.mu.Unlock()
	switch req.Op {
	case "tunnel_add":
		if !config.ValidName(req.Name) {
			return portResponse{}, fmt.Errorf("tunnel name %q must match ^[a-z][a-z0-9-]*$", req.Name)
		}
		if next[req.Name] != nil {
			return portResponse{}, fmt.Errorf("tunnel %s exists; use \"hostel tunnel set\"", req.Name)
		}
		t := &TunnelConfig{LocalPortEnv: req.LocalPortEnv, EnvValue: req.EnvValue}
		if req.LocalPort != nil {
			t.LocalPort = *req.LocalPort
		}
		if req.MachinePort != nil {
			t.FixedMachinePort = *req.MachinePort
		}
		next[req.Name] = t
	case "tunnel_set":
		t := next[req.Name]
		if t == nil {
			return portResponse{}, fmt.Errorf("no tunnel %s", req.Name)
		}
		if req.LocalPort != nil {
			t.LocalPort, t.LocalPortEnv = *req.LocalPort, ""
		}
		if req.MachinePort != nil {
			t.FixedMachinePort = *req.MachinePort
		}
	case "tunnel_remove":
		if next[req.Name] == nil {
			return portResponse{}, fmt.Errorf("no tunnel %s", req.Name)
		}
		delete(next, req.Name)
	}
	needRegister := req.Op != "tunnel_set" || req.MachinePort != nil
	if needRegister {
		ports, err := p.register(ctx, next)
		if err != nil {
			return portResponse{}, err
		}
		for n, port := range ports {
			next[n].MachinePort = port
		}
	}
	p.mu.Lock()
	p.tunnels = next
	p.mu.Unlock()
	if req.Op == "tunnel_remove" {
		p.mu.Lock()
		if ln := p.tunLn[req.Name]; ln != nil {
			ln.Close()
			delete(p.tunLn, req.Name)
		}
		delete(p.state, req.Name)
		p.mu.Unlock()
	} else if needRegister {
		if err := p.listenTunnel(req.Name); err != nil {
			// Undo: the old list, with its forwards.
			p.mu.Lock()
			p.tunnels = old
			delete(p.state, req.Name)
			p.mu.Unlock()
			if _, rerr := p.register(ctx, old); rerr != nil {
				p.log.Printf("ports: undo the registration: %v", rerr)
			}
			if old[req.Name] != nil {
				p.listenTunnel(req.Name)
			}
			return portResponse{}, err
		}
	}
	if err := saveTunnels(p.st, next); err != nil {
		return portResponse{}, err
	}
	if needRegister {
		if err := p.writeLeaseEnv(ctx, next); err != nil {
			return portResponse{}, err
		}
	}
	return p.status(), nil
}

func (p *portProc) register(ctx context.Context, tunnels map[string]*TunnelConfig) (map[string]int, error) {
	req := []protocol.TunnelRequest{}
	for n, t := range tunnels {
		req = append(req, protocol.TunnelRequest{Name: n, MachinePort: t.FixedMachinePort})
	}
	var resp struct {
		TunnelPorts map[string]int `json:"tunnel_ports"`
	}
	if err := p.api.Do(ctx, "PUT", "/v1/leases/"+p.st.LeaseID+"/tunnels", req, &resp); err != nil {
		return nil, err
	}
	return resp.TunnelPorts, nil
}

func (p *portProc) writeLeaseEnv(ctx context.Context, tunnels map[string]*TunnelConfig) error {
	rc := &config.Repo{}
	if p.st.Root != "" {
		r := &Repo{Root: p.st.Root}
		var err error
		if rc, err = r.RepoConfig(); err != nil {
			return err
		}
	}
	conn, err := Dial(ctx, p.st, "cmd")
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Helper(ctx, []string{"set-env"}, map[string]any{"env": leaseEnv(p.st.leaseTools(), rc, tunnelPorts(tunnels))}, nil)
}

// addForward listens on the guest's 127.0.0.1 and sends each connection
// through the gateway to the machine's 127.0.0.1:<machine port>.
func (p *portProc) addForward(req portRequest) (portResponse, error) {
	if req.MachinePort == nil || *req.MachinePort < 1 || *req.MachinePort > 65535 {
		return portResponse{}, errors.New("forward needs a machine port between 1 and 65535")
	}
	machine := *req.MachinePort
	p.mu.Lock()
	exists := p.forwards[machine] != nil
	p.mu.Unlock()
	if exists {
		return portResponse{}, fmt.Errorf("a forward for machine port %d exists", machine)
	}
	local := machine
	if req.LocalPort != nil {
		local = *req.LocalPort
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(local)))
	if err != nil && req.LocalPort == nil {
		// The same number is in use: take a free port.
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return portResponse{}, fmt.Errorf("listen on local port %d: %w", local, err)
	}
	p.mu.Lock()
	p.forwards[machine] = ln
	p.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				p.mu.Lock()
				conn := p.conn
				p.mu.Unlock()
				if conn == nil {
					return
				}
				up, err := conn.client.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(machine)))
				if err != nil {
					p.log.Printf("ports: forward %d: %v", machine, err)
					return
				}
				defer up.Close()
				pipe(c, up)
			}()
		}
	}()
	resp := p.status()
	resp.LocalPort = ln.Addr().(*net.TCPAddr).Port
	return resp, nil
}
