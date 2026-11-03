package config

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Provider kinds.
const (
	ProviderStatic    = "static"
	ProviderNamespace = "namespace"
	ProviderExternal  = "external"
)

var (
	nameRE     = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	userRE     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)
	serverIDRE = regexp.MustCompile(`^[a-z0-9]{1,20}$`)
)

// ValidName reports whether s is a valid provider, machine, platform, device,
// slot, or tunnel name.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// TLSProblem describes what is wrong with a tls value, or returns "".
func TLSProblem(mode string) string {
	if mode == TLSOff || mode == TLSSelfSigned {
		return ""
	}
	return fmt.Sprintf("%q must be %q (the server's own certificate, pinned by clients) or %q (behind a proxy with TLS)", mode, TLSSelfSigned, TLSOff)
}

// PublicURLProblem describes what is wrong with a public_urls entry for the
// tls mode, or returns "". With tls = "off" the server is behind a proxy with
// TLS, so the addresses are https:// too, except on loopback.
func PublicURLProblem(u, tlsMode string) string {
	pu, err := url.Parse(u)
	switch {
	case err != nil || pu.Host == "" || (pu.Path != "" && pu.Path != "/") || pu.User != nil || strings.ContainsAny(u, "?#"):
		return fmt.Sprintf("%q is not a server address such as https://hostel.example.com or https://100.80.1.2:7410", u)
	case pu.Scheme != "https" && !(pu.Scheme == "http" && tlsMode == TLSOff && isLoopback(pu.Hostname())):
		// Clients send the invite secret and their token to this address: in
		// clear text only on this computer.
		return fmt.Sprintf("%q must start with https:// (http:// only for a local server on 127.0.0.1 or localhost)", u)
	}
	return ""
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ValidServerID reports whether s is a valid server_id.
func ValidServerID(s string) bool { return serverIDRE.MatchString(s) }

// Server is server.toml. After LoadServer, all defaults are set and each machine
// has the values that it inherits from its provider.
type Server struct {
	ServerID      string    `toml:"server_id"`
	Listen        string    `toml:"listen"`
	GatewayListen string    `toml:"gateway_listen"`
	GatewayPublic string    `toml:"gateway_public"`
	TLS           string    `toml:"tls"`
	PublicURLs    []string  `toml:"public_urls"`
	DataDir       string    `toml:"data_dir"`
	HelperDir     string    `toml:"helper_dir"`
	PlatformsDir  string    `toml:"platforms_dir"`
	IdleTimeout   *Duration `toml:"idle_timeout"`
	// Aging is read only to reject it (removed in M8); the schema omits it.
	Aging              *Duration            `toml:"aging" schema:"-"`
	PollTimeout        *Duration            `toml:"poll_timeout"`
	LimitRetry         *Duration            `toml:"limit_retry"`
	PortProcessOffline *Duration            `toml:"port_process_offline"`
	Intervals          *Intervals           `toml:"intervals"`
	Mise               *Mise                `toml:"mise"`
	Providers          map[string]*Provider `toml:"providers"`

	File string `toml:"-"`
	// DocName names the configuration document in errors ("" before M9).
	DocName       string               `toml:"-"`
	ProviderOrder []string             `toml:"-"`
	Platforms     map[string]*Platform `toml:"-"`
}

// GatewayPath is the gateway (SSH in a WebSocket) on the API listener.
const GatewayPath = "/v1/gateway"

// TLS modes of the API listener.
const (
	TLSOff        = "off"
	TLSSelfSigned = "self-signed"
)

type Intervals struct {
	Scheduler   *Duration `toml:"scheduler"`
	Reconcile   *Duration `toml:"reconcile"`
	TunnelCheck *Duration `toml:"tunnel_check"`
	// Presence is how often the server checks that the idle static
	// machines answer, and that the offline ones are back (M10b).
	Presence *Duration `toml:"presence"`
}

type Mise struct {
	IdiomaticTools []string `toml:"idiomatic_tools"`
	GithubToken    string   `toml:"github_token,omitempty"`
	AgentDevice    *bool    `toml:"agent_device,omitempty"`
}

type Provider struct {
	Kind        string              `toml:"kind"`
	MaxRooms    *int                `toml:"max_rooms,omitempty"`
	Limits      []Limit             `toml:"limits,omitempty"`
	MaxLifetime *Duration           `toml:"max_lifetime,omitempty"`
	Tools       map[string]string   `toml:"tools,omitempty"`
	Env         map[string]string   `toml:"env,omitempty"`
	MaxLease    *Duration           `toml:"max_lease"`
	WarmRunning *Duration           `toml:"warm_running,omitempty"`
	WarmStopped *Duration           `toml:"warm_stopped,omitempty"`
	Command     []string            `toml:"command,omitempty"`
	CommandEnv  map[string]string   `toml:"command_env,omitempty"`
	Timeouts    *Timeouts           `toml:"timeouts,omitempty"`
	Machines    map[string]*Machine `toml:"machines"`

	Name         string   `toml:"-"`
	MachineOrder []string `toml:"-"`
}

type Limit struct {
	Machines []string `toml:"machines"`
	MaxRooms int      `toml:"max_rooms,omitempty"`
}

type Timeouts struct {
	Provision *Duration `toml:"provision"`
	Default   *Duration `toml:"default"`
}

type Machine struct {
	Address     string            `toml:"address,omitempty"`
	Connect     bool              `toml:"connect,omitempty"`
	OS          string            `toml:"os"`
	Spec        map[string]any    `toml:"spec,omitempty"`
	Platforms   []string          `toml:"platforms,omitempty"`
	Labels      []string          `toml:"labels,omitempty"`
	Tools       map[string]string `toml:"tools,omitempty"`
	Env         map[string]string `toml:"env,omitempty"`
	Slots       []Slot            `toml:"slots"`
	MaxRooms    *int              `toml:"max_rooms,omitempty"`
	MaxLease    *Duration         `toml:"max_lease"`
	MaxLifetime *Duration         `toml:"max_lifetime,omitempty"`
	WarmRunning *Duration         `toml:"warm_running,omitempty"`
	WarmStopped *Duration         `toml:"warm_stopped,omitempty"`

	Name     string `toml:"-"`
	Provider string `toml:"-"`
	Kind     string `toml:"-"`
}

type Slot struct {
	Name  string            `toml:"name"`
	User  string            `toml:"user,omitempty"`
	Ports []int             `toml:"ports,omitempty"`
	Env   map[string]string `toml:"env,omitempty"`
}

// Key returns "<provider>/<machine>".
func (m *Machine) Key() string { return m.Provider + "/" + m.Name }

// OnDemand reports whether the provider creates rooms of this machine when needed.
func (m *Machine) OnDemand() bool { return m.Kind != ProviderStatic }

// Machines returns all machines in file order (providers in file order, then
// machines in file order).
func (s *Server) Machines() []*Machine {
	var out []*Machine
	for _, pn := range s.ProviderOrder {
		p := s.Providers[pn]
		for _, mn := range p.MachineOrder {
			out = append(out, p.Machines[mn])
		}
	}
	return out
}

// Machine returns the machine with the key "<provider>/<machine>", or nil.
func (s *Server) Machine(key string) *Machine {
	pn, mn, _ := strings.Cut(key, "/")
	p := s.Providers[pn]
	if p == nil {
		return nil
	}
	return p.Machines[mn]
}

// DefaultServerPath returns ~/.config/hostel/server.toml.
func DefaultServerPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".config", "hostel", "server.toml"), nil
}

// LoadServer reads and validates a server.toml with all keys (the form before
// M9) and all platform directories.
func LoadServer(path string) (*Server, error) { return load(path, nil) }

// LoadServerFile is LoadServer with the platforms of platformsDir, not those of
// platforms_dir: the import of a server.toml from before M9 checks it with the
// platforms of the database.
func LoadServerFile(path, platformsDir string) (*Server, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return loadData(path, data, &Document{PlatformsDir: platformsDir, legacy: true})
}

// LoadServerDocument validates server.toml (local keys only; data is its
// content, path its name) with the configuration document from the database.
func LoadServerDocument(path string, data []byte, doc Document) (*Server, error) {
	return loadData(path, data, &doc)
}

func load(path string, doc *Document) (*Server, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return loadData(path, data, doc)
}

func loadData(path string, data []byte, doc *Document) (*Server, error) {
	s := &Server{}
	if err := decodeData(path, data, s); err != nil {
		return nil, err
	}
	if doc != nil && !doc.legacy {
		if err := checkParts(path, data, *doc); err != nil {
			return nil, err
		}
		if err := decodeData(doc.Name(), doc.Data, s); err != nil {
			return nil, err
		}
		data = doc.Data
		s.DocName = doc.Name()
	}
	order, err := keyOrder(data)
	if err != nil {
		return nil, positioned(path, err)
	}
	s.File = path
	s.ProviderOrder = childOrder(order, "providers")
	for _, pn := range s.ProviderOrder {
		if p := s.Providers[pn]; p != nil {
			p.MachineOrder = childOrder(order, "providers", pn, "machines")
		}
	}
	resolveErr := s.resolve()
	if doc != nil && doc.PlatformsDir != "" {
		s.PlatformsDir = doc.PlatformsDir
	}
	platforms, err := LoadPlatforms(s.PlatformsDir)
	if err != nil {
		return nil, errors.Join(resolveErr, err)
	}
	s.Platforms = platforms
	if err := errors.Join(resolveErr, s.validatePlatforms()); err != nil {
		return nil, err
	}
	return s, nil
}

// checkParts reports keys of the document in server.toml and local keys in the
// document.
func checkParts(path string, file []byte, doc Document) error {
	var errs []error
	keys, err := topKeys(file)
	if err != nil {
		return positioned(path, err)
	}
	for _, k := range keys {
		if documentKeys[k] {
			errs = append(errs, fmt.Errorf("%s: %s: is in the configuration in the database since M9; change it with \"hostel config edit\" and delete it here", path, k))
		}
	}
	if keys, err = topKeys(doc.Data); err != nil {
		return positioned(doc.Name(), err)
	}
	for _, k := range keys {
		if localKeys[k] {
			errs = append(errs, fmt.Errorf("%s: %s: belongs in server.toml on the server machine, not in the configuration", doc.Name(), k))
		}
	}
	return errors.Join(errs...)
}

type errList struct {
	file string
	// doc names the configuration document, the source of its keys.
	doc  string
	errs []error
}

func (l *errList) add(key, format string, args ...any) {
	file := l.file
	if top, _, _ := strings.Cut(key, "."); l.doc != "" && documentKeys[strings.SplitN(top, "[", 2)[0]] {
		file = l.doc
	}
	l.errs = append(l.errs, fmt.Errorf("%s: %s: %s", file, key, fmt.Sprintf(format, args...)))
}

func (l *errList) err() error { return errors.Join(l.errs...) }

func (s *Server) resolve() error {
	el := &errList{file: s.File, doc: s.DocName}
	base := filepath.Dir(s.File)
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("find home directory: %w", err)
	}
	if s.ServerID == "" {
		el.add("server_id", "required; for example \"%s\"", "mymac")
	} else if !ValidServerID(s.ServerID) {
		el.add("server_id", "%q must match %s", s.ServerID, serverIDRE)
	}
	setStr(&s.Listen, "127.0.0.1:7410")
	setStr(&s.GatewayListen, "127.0.0.1:7422")
	// With its own TLS, the server gives guests the gateway on the API address
	// that they use (the path only; the client adds its server address).
	if s.TLS == TLSSelfSigned {
		setStr(&s.GatewayPublic, GatewayPath)
	}
	setStr(&s.GatewayPublic, s.GatewayListen)
	for _, a := range []struct{ key, v string }{{"listen", s.Listen}, {"gateway_listen", s.GatewayListen}, {"gateway_public", s.GatewayPublic}} {
		if a.key == "gateway_public" && a.v == GatewayPath {
			continue
		}
		if a.key == "gateway_public" && IsWebSocketURL(a.v) {
			if u, err := url.Parse(a.v); err != nil || u.Host == "" {
				el.add(a.key, "%q is not a ws:// or wss:// URL", a.v)
			} else if !strings.HasSuffix(u.Path, "/v1/gateway") {
				el.add(a.key, "%q must end with the path /v1/gateway (the gateway on the API)", a.v)
			}
			continue
		}
		if _, _, err := net.SplitHostPort(a.v); err != nil {
			el.add(a.key, "%q is not a host:port address or a wss:// URL", a.v)
		}
	}
	setStr(&s.DataDir, filepath.Join(home, ".local", "share", "hostel"))
	s.DataDir = absPath(s.DataDir, base, home)
	setStr(&s.HelperDir, filepath.Join(s.DataDir, "helpers"))
	s.HelperDir = absPath(s.HelperDir, base, home)
	setStr(&s.PlatformsDir, filepath.Join(home, ".config", "hostel", "platforms"))
	s.PlatformsDir = absPath(s.PlatformsDir, base, home)

	setDur(&s.IdleTimeout, 10*time.Minute)
	// Queues have no aging since M8 (owner decision, 2026-10-30).
	if s.Aging != nil {
		el.add("aging", "removed in M8: each queue is first in, first out inside its priority lane; delete the key")
	}
	if s.TLS == "" {
		s.TLS = TLSOff
	} else if p := TLSProblem(s.TLS); p != "" {
		el.add("tls", "%s", p)
	}
	for i, u := range s.PublicURLs {
		if p := PublicURLProblem(u, s.TLS); p != "" {
			el.add(fmt.Sprintf("public_urls[%d]", i), "%s", p)
		}
	}
	setDur(&s.PollTimeout, 5*time.Minute)
	setDur(&s.LimitRetry, 2*time.Minute)
	setDur(&s.PortProcessOffline, 10*time.Minute)
	if s.Intervals == nil {
		s.Intervals = &Intervals{}
	}
	setDur(&s.Intervals.Scheduler, 10*time.Second)
	setDur(&s.Intervals.Reconcile, 5*time.Minute)
	setDur(&s.Intervals.TunnelCheck, 30*time.Second)
	setDur(&s.Intervals.Presence, 30*time.Second)
	for _, d := range []struct {
		key string
		v   *Duration
	}{
		{"idle_timeout", s.IdleTimeout}, {"poll_timeout", s.PollTimeout},
		{"limit_retry", s.LimitRetry}, {"port_process_offline", s.PortProcessOffline},
		{"intervals.scheduler", s.Intervals.Scheduler}, {"intervals.reconcile", s.Intervals.Reconcile},
		{"intervals.tunnel_check", s.Intervals.TunnelCheck}, {"intervals.presence", s.Intervals.Presence},
	} {
		if d.v.D() <= 0 {
			el.add(d.key, "must be longer than 0")
		}
	}
	if s.Mise == nil {
		s.Mise = &Mise{}
	}
	// Omitted gets the default; an empty list turns idiomatic files off.
	if s.Mise.IdiomaticTools == nil {
		s.Mise.IdiomaticTools = []string{"node", "python"}
	}

	for _, pn := range s.ProviderOrder {
		if p := s.Providers[pn]; p != nil {
			s.resolveProvider(el, pn, p)
		}
	}
	return el.err()
}

func (s *Server) resolveProvider(el *errList, pn string, p *Provider) {
	key := "providers." + pn
	p.Name = pn
	if !nameRE.MatchString(pn) {
		el.add(key, "provider name must match %s", nameRE)
	}
	onDemand := p.Kind != ProviderStatic
	switch p.Kind {
	case ProviderStatic, ProviderNamespace, ProviderExternal:
	case "":
		el.add(key+".kind", "required: %q, %q, or %q", ProviderStatic, ProviderNamespace, ProviderExternal)
	default:
		el.add(key+".kind", "unknown kind %q; use %q, %q, or %q", p.Kind, ProviderStatic, ProviderNamespace, ProviderExternal)
	}
	if p.Kind == ProviderExternal {
		if len(p.Command) == 0 {
			el.add(key+".command", "required for an external provider")
		}
		if p.Timeouts == nil {
			p.Timeouts = &Timeouts{}
		}
		setDur(&p.Timeouts.Provision, 5*time.Minute)
		setDur(&p.Timeouts.Default, time.Minute)
	} else {
		if len(p.Command) > 0 || p.CommandEnv != nil || p.Timeouts != nil {
			el.add(key, "command, command_env, and timeouts are only for external providers")
		}
	}
	if !onDemand {
		if p.MaxRooms != nil || len(p.Limits) > 0 || p.MaxLifetime != nil || p.WarmRunning != nil || p.WarmStopped != nil {
			el.add(key, "max_rooms, limits, max_lifetime, warm_running, and warm_stopped are only for on-demand providers")
		}
	} else {
		setDur(&p.WarmRunning, 5*time.Minute)
		setDur(&p.WarmStopped, 24*time.Hour)
	}
	setDur(&p.MaxLease, 2*time.Hour)
	if p.MaxRooms != nil && *p.MaxRooms < 1 {
		el.add(key+".max_rooms", "must be at least 1")
	}
	for i, l := range p.Limits {
		lk := fmt.Sprintf("%s.limits[%d]", key, i)
		if l.MaxRooms < 1 {
			el.add(lk+".max_rooms", "must be at least 1")
		}
		if len(l.Machines) == 0 {
			el.add(lk+".machines", "list at least one machine")
		}
		for j, mn := range l.Machines {
			if p.Machines[mn] == nil {
				el.add(fmt.Sprintf("%s.machines[%d]", lk, j), "unknown machine %q in this provider", mn)
			}
		}
	}
	for _, mn := range p.MachineOrder {
		if m := p.Machines[mn]; m != nil {
			s.resolveMachine(el, p, mn, m)
		}
	}
}

func (s *Server) resolveMachine(el *errList, p *Provider, mn string, m *Machine) {
	key := "providers." + p.Name + ".machines." + mn
	m.Name, m.Provider, m.Kind = mn, p.Name, p.Kind
	if !nameRE.MatchString(mn) {
		el.add(key, "machine name must match %s", nameRE)
	}
	onDemand := m.OnDemand()

	switch p.Kind {
	case ProviderStatic:
		if m.Address == "" {
			el.add(key+".address", "required for a static machine, for example \"hostel@macbook.local\"")
		} else if _, _, _, err := ParseAddress(m.Address); err != nil {
			el.add(key+".address", "%v", err)
		}
		if m.Spec != nil {
			el.add(key+".spec", "a static machine has no spec")
		}
		if m.MaxRooms != nil || m.MaxLifetime != nil || m.WarmRunning != nil || m.WarmStopped != nil {
			el.add(key, "max_rooms, max_lifetime, warm_running, and warm_stopped are only for on-demand machines")
		}
		one := 1
		m.MaxRooms = &one
	default:
		if m.Address != "" || m.Connect {
			el.add(key, "address and connect are only for static machines")
		}
		if m.Spec == nil {
			el.add(key+".spec", "required for an on-demand machine")
		}
		if m.MaxRooms == nil {
			one := 1
			m.MaxRooms = &one
		} else if *m.MaxRooms < 1 {
			el.add(key+".max_rooms", "must be at least 1")
		}
	}

	switch p.Kind {
	case ProviderStatic, ProviderExternal:
		if m.OS == "" {
			el.add(key+".os", "required for %s machines: %s", p.Kind, strings.Join(OSNames, ", "))
		} else if !isOS(m.OS) {
			el.add(key+".os", "unknown OS %q; use %s", m.OS, strings.Join(OSNames, ", "))
		}
	case ProviderNamespace:
		if m.OS != "" {
			el.add(key+".os", "a namespace machine gets its OS from spec; remove os")
		}
		m.OS = namespaceOS(el, key, m.Spec)
	}

	m.Tools = mergeMap(p.Tools, m.Tools)
	m.Env = mergeMap(p.Env, m.Env)
	inherit(&m.MaxLease, p.MaxLease)
	if onDemand {
		inherit(&m.MaxLifetime, p.MaxLifetime)
		inherit(&m.WarmRunning, p.WarmRunning)
		inherit(&m.WarmStopped, p.WarmStopped)
	}
	if m.MaxLease.D() <= 0 {
		el.add(key+".max_lease", "must be longer than 0")
	}
	if product, _ := m.Spec["product"].(string); p.Kind == ProviderNamespace && product == "devbox" && m.MaxLifetime.D() <= 0 {
		el.add(key+".max_lifetime", "required for a devbox: the maximum duration of the Namespace plan, for example \"3h\"")
	}
	if lt := m.MaxLifetime.D(); lt > 0 && m.MaxLease.D()+5*time.Minute > lt {
		el.add(key+".max_lifetime", "%s is shorter than max_lease (%s) + 5m; lower max_lease or raise max_lifetime", lt, m.MaxLease.D())
	}

	for i, l := range m.Labels {
		if l == "" {
			el.add(fmt.Sprintf("%s.labels[%d]", key, i), "empty label")
		}
	}
	for tool := range m.Tools {
		if tool == "" {
			el.add(key+".tools", "empty tool name")
		}
	}
	if len(m.Slots) == 0 {
		m.Slots = []Slot{{Name: "a"}}
	}
	names := map[string]bool{}
	users := map[string]bool{}
	ports := map[int]bool{}
	for i := range m.Slots {
		sl := &m.Slots[i]
		sk := fmt.Sprintf("%s.slots[%d]", key, i)
		if !nameRE.MatchString(sl.Name) {
			el.add(sk+".name", "slot name %q must match %s", sl.Name, nameRE)
		}
		if names[sl.Name] {
			el.add(sk+".name", "duplicate slot %q", sl.Name)
		}
		names[sl.Name] = true
		if sl.User != "" {
			switch {
			case m.Kind != ProviderStatic:
				el.add(sk+".user", "a slot user is only for static machines")
			case !userRE.MatchString(sl.User):
				el.add(sk+".user", "user %q must match %s", sl.User, userRE)
			case users[sl.User]:
				el.add(sk+".user", "user %q has two slots; each slot needs its own user", sl.User)
			}
			users[sl.User] = true
		}
		if sl.Ports == nil {
			for port := 7500 + 10*i; port <= 7509+10*i; port++ {
				sl.Ports = append(sl.Ports, port)
			}
		}
		for j, port := range sl.Ports {
			if port < 1024 || port > 65535 {
				el.add(fmt.Sprintf("%s.ports[%d]", sk, j), "port %d is not between 1024 and 65535", port)
			}
			if ports[port] {
				el.add(fmt.Sprintf("%s.ports[%d]", sk, j), "port %d is used twice on this machine", port)
			}
			ports[port] = true
		}
	}
}

func namespaceOS(el *errList, key string, spec map[string]any) string {
	product, _ := spec["product"].(string)
	switch product {
	case "devbox":
		for _, k := range []string{"size", "image"} {
			if v, _ := spec[k].(string); v == "" {
				el.add(key+".spec."+k, "required for a devbox, for example size = \"m\" and image = \"tahoe-slim\"")
			}
		}
		platform, _ := spec["platform"].(string)
		if platform != OSLinux && platform != OSMacOS {
			el.add(key+".spec.platform", "a devbox needs platform \"linux\" or \"macos\"")
			return ""
		}
		return platform
	case "instance":
		mt, _ := spec["machine_type"].(string)
		osName, _, _ := strings.Cut(mt, "/")
		if !isOS(osName) {
			el.add(key+".spec.machine_type", "an instance needs machine_type \"<os>/<arch>:<cpu>x<mem>\", for example \"linux/amd64:4x16\"")
			return ""
		}
		return osName
	case "":
		if spec != nil {
			el.add(key+".spec.product", "required: \"devbox\" or \"instance\"")
		}
	default:
		el.add(key+".spec.product", "unknown product %q; use \"devbox\" or \"instance\"", product)
	}
	return ""
}

func (s *Server) validatePlatforms() error {
	el := &errList{file: s.File, doc: s.DocName}
	for _, m := range s.Machines() {
		key := "providers." + m.Provider + ".machines." + m.Name
		seen := map[string]bool{}
		for i, pn := range m.Platforms {
			pk := fmt.Sprintf("%s.platforms[%d]", key, i)
			p := s.Platforms[pn]
			switch {
			case seen[pn]:
				el.add(pk, "platform %q is listed twice", pn)
			case p == nil:
				if s.DocName != "" {
					el.add(pk, "unknown platform %q; \"hostel config platforms\" lists the platforms", pn)
				} else {
					el.add(pk, "unknown platform %q; platforms_dir %s has no directory with this name", pn, s.PlatformsDir)
				}
			case p.Kind == KindOS:
				el.add(pk, "%q is an OS platform; a machine gets its OS platform automatically, so remove it", pn)
			case m.OS != "" && p.Script(m.OS, StepPrepare) == "":
				where := filepath.Join(p.Dir, StepPrepare+"."+m.OS+ScriptExt(m.OS))
				if s.DocName != "" {
					where = StepPrepare + "." + m.OS + ScriptExt(m.OS) + " (hostel config platforms push)"
				}
				el.add(pk, "platform %q has no prepare script for %s; add %s", pn, m.OS, where)
			}
			seen[pn] = true
		}
	}
	return el.err()
}

// ParseAddress parses a static machine address "user@host[:port]". The default
// user is "hostel" and the default port is 22.
func ParseAddress(addr string) (user, host string, port int, err error) {
	user, hostPort, ok := strings.Cut(addr, "@")
	if !ok {
		user, hostPort = "hostel", addr
	}
	if user == "" {
		return "", "", 0, fmt.Errorf("address %q has an empty user", addr)
	}
	host, portStr, splitErr := net.SplitHostPort(hostPort)
	if splitErr != nil {
		host, portStr = strings.Trim(hostPort, "[]"), "22"
	}
	if host == "" {
		return "", "", 0, fmt.Errorf("address %q has no host", addr)
	}
	port, err = strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", "", 0, fmt.Errorf("address %q has an invalid port", addr)
	}
	return user, host, port, nil
}

func setStr(p *string, v string) {
	if *p == "" {
		*p = v
	}
}

func setDur(p **Duration, v time.Duration) {
	if *p == nil {
		*p = dur(v)
	}
}

func inherit(dst **Duration, src *Duration) {
	if *dst == nil && src != nil {
		*dst = dur(src.D())
	}
}

func mergeMap(base, over map[string]string) map[string]string {
	out := map[string]string{}
	maps.Copy(out, base)
	maps.Copy(out, over)
	return out
}

// absPath expands "~/" and makes a path relative to the configuration file
// directory absolute.
func absPath(p, base, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	if !filepath.IsAbs(p) {
		return filepath.Join(base, p)
	}
	return filepath.Clean(p)
}

// HasLabels reports whether the machine has all the labels.
func (m *Machine) HasLabels(labels []string) bool {
	for _, l := range labels {
		if !slices.Contains(m.Labels, l) {
			return false
		}
	}
	return true
}

// IsWebSocketURL reports whether a gateway address is a WebSocket URL (the
// gateway's /v1/gateway on the API) instead of a TCP host:port.
func IsWebSocketURL(addr string) bool {
	return strings.HasPrefix(addr, "wss://") || strings.HasPrefix(addr, "ws://")
}
