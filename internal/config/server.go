package config

import (
	"errors"
	"fmt"
	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
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

	"github.com/csansoon/hostel/tools"
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

// ValidName reports whether s is a valid provider, machine, tool, option,
// room, or tunnel name.
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
	ServerID      string   `toml:"server_id"`
	Listen        string   `toml:"listen"`
	GatewayListen string   `toml:"gateway_listen"`
	GatewayPublic string   `toml:"gateway_public"`
	TLS           string   `toml:"tls"`
	PublicURLs    []string `toml:"public_urls"`
	DataDir       string   `toml:"data_dir"`
	HelperDir     string   `toml:"helper_dir"`
	// PlatformsDir is read only to ignore it: install.sh wrote it before
	// M10e, and the update must not stop the server (PlatformsDirWarning).
	// The schema omits it.
	PlatformsDir string    `toml:"platforms_dir,omitempty" schema:"-"`
	IdleTimeout  *Duration `toml:"idle_timeout"`
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
	DocName       string   `toml:"-"`
	ProviderOrder []string `toml:"-"`
	// Tools are the defined tools.
	Tools map[string]*Tool `toml:"-"`
}

// PlatformsDirWarning is the warning for platforms_dir in server.toml.
const PlatformsDirWarning = "server.toml has platforms_dir, which M10e removed: the server ignores it and uses the tools in its binary and its database (hostel config tools); delete the key"

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
	Kind string `toml:"kind"`
	// Label is the name that people see ("Office"); default: the key.
	Label       string               `toml:"label,omitempty"`
	MaxMachines *int                 `toml:"max_machines,omitempty"`
	Limits      []Limit              `toml:"limits,omitempty"`
	MaxLifetime *Duration            `toml:"max_lifetime,omitempty"`
	Tools       []string             `toml:"tools,omitempty"`
	Env         map[string]string    `toml:"env,omitempty"`
	MaxLease    *Duration            `toml:"max_lease"`
	WarmRunning *Duration            `toml:"warm_running,omitempty"`
	WarmStopped *Duration            `toml:"warm_stopped,omitempty"`
	Command     []string             `toml:"command,omitempty"`
	CommandEnv  map[string]string    `toml:"command_env,omitempty"`
	Timeouts    *Timeouts            `toml:"timeouts,omitempty"`
	Templates   map[string]*Template `toml:"templates"`
	// Machines are the real hosts of a static provider; resolve moves them
	// to Templates, which describes each machine for the rest of the server.
	Machines map[string]*Template `toml:"machines"`

	Name          string   `toml:"-"`
	TemplateOrder []string `toml:"-"`
}

type Limit struct {
	Templates   []string `toml:"templates"`
	MaxMachines int      `toml:"max_machines,omitempty"`
}

type Timeouts struct {
	Provision *Duration `toml:"provision"`
	Default   *Duration `toml:"default"`
}

type Template struct {
	Address string         `toml:"address,omitempty"`
	Connect bool           `toml:"connect,omitempty"`
	OS      string         `toml:"os"`
	Spec    map[string]any `toml:"spec,omitempty"`
	// Tools are "name" or "name:option" entries. After LoadServer, they
	// include the tools that the needs of the listed tools add.
	Tools       []string          `toml:"tools,omitempty"`
	Env         map[string]string `toml:"env,omitempty"`
	Rooms       []Room            `toml:"rooms"`
	MaxMachines *int              `toml:"max_machines,omitempty"`
	MaxLease    *Duration         `toml:"max_lease"`
	MaxLifetime *Duration         `toml:"max_lifetime,omitempty"`
	WarmRunning *Duration         `toml:"warm_running,omitempty"`
	WarmStopped *Duration         `toml:"warm_stopped,omitempty"`

	Name     string `toml:"-"`
	Provider string `toml:"-"`
	Kind     string `toml:"-"`
	// ToolList is Tools, resolved: each tool with its option, and whether
	// it is a mise tool or a need.
	ToolList []TemplateTool `toml:"-"`
}

type Room struct {
	Name  string            `toml:"name"`
	User  string            `toml:"user,omitempty"`
	Ports []int             `toml:"ports,omitempty"`
	Env   map[string]string `toml:"env,omitempty"`
}

// templatesKey is the table of the provider's entries: real machines of a
// static provider, or the templates of a provider that creates machines.
func (p *Provider) templatesKey() string { return TemplatesKey(p.Kind) }

// TemplatesKey is the configuration table of a provider kind.
func TemplatesKey(kind string) string {
	if kind == ProviderStatic {
		return "machines"
	}
	return "templates"
}

// Key returns "<provider>/<machine>".
func (m *Template) Key() string { return m.Provider + "/" + m.Name }

// OnDemand reports whether the provider creates machines of this machine when needed.
func (m *Template) OnDemand() bool { return m.Kind != ProviderStatic }

// DeclaredSize is the CPU and memory (in GB) that the machine's spec states
// ("<os>/<arch>:<cpu>x<mem>"); zero when it states none.
func (m *Template) DeclaredSize() (cores, memoryGB int) {
	mt, _ := m.Spec["machine_type"].(string)
	_, size, ok := strings.Cut(mt, ":")
	if !ok {
		return 0, 0
	}
	c, g, ok := strings.Cut(size, "x")
	if !ok {
		return 0, 0
	}
	cores, _ = strconv.Atoi(c)
	memoryGB, _ = strconv.Atoi(g)
	return cores, memoryGB
}

// Effective is the resolved configuration as TOML, with the keys of the file:
// a static provider has its machines again, without the limit that resolve set.
func (s *Server) Effective() ([]byte, error) {
	out := *s
	out.Providers = make(map[string]*Provider, len(s.Providers))
	for name, p := range s.Providers {
		c := *p
		if p.Kind == ProviderStatic {
			c.Machines, c.Templates = make(map[string]*Template, len(p.Templates)), nil
			for mn, m := range p.Templates {
				e := *m
				e.MaxMachines = nil
				c.Machines[mn] = &e
			}
		}
		out.Providers[name] = &c
	}
	return toml.Marshal(&out)
}

// Templates returns all machines in file order (providers in file order, then
// machines in file order).
func (s *Server) Templates() []*Template {
	var out []*Template
	for _, pn := range s.ProviderOrder {
		p := s.Providers[pn]
		for _, mn := range p.TemplateOrder {
			out = append(out, p.Templates[mn])
		}
	}
	return out
}

// Template returns the machine with the key "<provider>/<machine>", or nil.
func (s *Server) Template(key string) *Template {
	pn, mn, _ := strings.Cut(key, "/")
	p := s.Providers[pn]
	if p == nil {
		return nil
	}
	return p.Templates[mn]
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
// M9) with the default tools.
func LoadServer(path string) (*Server, error) { return load(path, nil) }

// LoadServerDocument validates server.toml (local keys only; data is its
// content, path its name) with the configuration document and the tools from
// the database.
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
	if err := checkOldKeys(path, data, ""); err != nil {
		return nil, err
	}
	if err := decodeData(path, data, s); err != nil {
		return nil, err
	}
	toolSet := DefaultTools()
	toolsDir := ""
	if doc != nil {
		if err := checkParts(path, data, *doc); err != nil {
			return nil, err
		}
		if err := checkOldKeys(path, doc.Data, doc.Name()); err != nil {
			return nil, err
		}
		if err := decodeData(doc.Name(), doc.Data, s); err != nil {
			return nil, err
		}
		data = doc.Data
		s.DocName = doc.Name()
		if doc.Tools != nil {
			toolSet = doc.Tools
		}
		toolsDir = doc.ToolsDir
	}
	order, err := keyOrder(data)
	if err != nil {
		return nil, positioned(path, err)
	}
	s.File = path
	s.ProviderOrder = childOrder(order, "providers")
	for _, pn := range s.ProviderOrder {
		if p := s.Providers[pn]; p != nil {
			p.TemplateOrder = childOrder(order, "providers", pn, p.templatesKey())
		}
	}
	tools, err := ParseTools(toolSet)
	if err != nil {
		return nil, err
	}
	for name, t := range tools {
		if toolsDir != "" {
			t.Dir = filepath.Join(toolsDir, name)
		}
	}
	s.Tools = tools
	if err := s.resolve(); err != nil {
		return nil, err
	}
	return s, nil
}

// DefaultTools returns the default tool definitions of this binary: name →
// file → content.
func DefaultTools() map[string]map[string]string {
	out := map[string]map[string]string{}
	for name, files := range tools.Defaults() {
		out[name] = map[string]string{}
		for f, data := range files {
			out[name][f] = string(data)
		}
	}
	return out
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
	if onDemand {
		if len(p.Machines) > 0 {
			el.add(key+".machines", "provider kind %q creates machines: use [%s.templates.<name>]", p.Kind, key)
		}
	} else {
		if len(p.Templates) > 0 {
			el.add(key+".templates", "a static provider has real machines: use [%s.machines.<name>]", key)
		}
		p.Templates, p.Machines = p.Machines, nil
	}
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
		if p.MaxMachines != nil || len(p.Limits) > 0 || p.MaxLifetime != nil || p.WarmRunning != nil || p.WarmStopped != nil {
			el.add(key, "max_machines, limits, max_lifetime, warm_running, and warm_stopped are only for on-demand providers")
		}
	} else {
		setDur(&p.WarmRunning, 5*time.Minute)
		setDur(&p.WarmStopped, 24*time.Hour)
	}
	setDur(&p.MaxLease, 2*time.Hour)
	if p.MaxMachines != nil && *p.MaxMachines < 1 {
		el.add(key+".max_machines", "must be at least 1")
	}
	for i, l := range p.Limits {
		lk := fmt.Sprintf("%s.limits[%d]", key, i)
		if l.MaxMachines < 1 {
			el.add(lk+".max_machines", "must be at least 1")
		}
		if len(l.Templates) == 0 {
			el.add(lk+".templates", "list at least one template")
		}
		for j, mn := range l.Templates {
			if p.Templates[mn] == nil {
				el.add(fmt.Sprintf("%s.templates[%d]", lk, j), "unknown template %q in this provider", mn)
			}
		}
	}
	for _, mn := range p.TemplateOrder {
		if m := p.Templates[mn]; m != nil {
			s.resolveTemplate(el, p, mn, m)
		}
	}
}

func (s *Server) resolveTemplate(el *errList, p *Provider, mn string, m *Template) {
	key := "providers." + p.Name + "." + p.templatesKey() + "." + mn
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
		if m.MaxMachines != nil || m.MaxLifetime != nil || m.WarmRunning != nil || m.WarmStopped != nil {
			el.add(key, "max_machines, max_lifetime, warm_running, and warm_stopped are only for on-demand machines")
		}
		one := 1
		m.MaxMachines = &one
	default:
		if m.Address != "" || m.Connect {
			el.add(key, "address and connect are only for static machines")
		}
		if m.Spec == nil {
			el.add(key+".spec", "required for an on-demand machine")
		}
		if m.MaxMachines == nil {
			one := 1
			m.MaxMachines = &one
		} else if *m.MaxMachines < 1 {
			el.add(key+".max_machines", "must be at least 1")
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

	if m.Tools == nil {
		m.Tools = slices.Clone(p.Tools)
	}
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

	s.resolveTools(el, key, m)
	if len(m.Rooms) == 0 {
		m.Rooms = []Room{{Name: "a"}}
	}
	names := map[string]bool{}
	users := map[string]bool{}
	ports := map[int]bool{}
	for i := range m.Rooms {
		sl := &m.Rooms[i]
		sk := fmt.Sprintf("%s.rooms[%d]", key, i)
		if !nameRE.MatchString(sl.Name) {
			el.add(sk+".name", "room name %q must match %s", sl.Name, nameRE)
		}
		if names[sl.Name] {
			el.add(sk+".name", "duplicate room %q", sl.Name)
		}
		names[sl.Name] = true
		if sl.User != "" {
			switch {
			case m.Kind != ProviderStatic:
				el.add(sk+".user", "a room user is only for static machines")
			case !userRE.MatchString(sl.User):
				el.add(sk+".user", "user %q must match %s", sl.User, userRE)
			case users[sl.User]:
				el.add(sk+".user", "user %q has two rooms; each room needs its own user", sl.User)
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

// resolveTools checks the tool entries of a machine and adds the tools that
// their needs name (design, section 8): m.ToolList has each tool once, the
// listed ones first.
func (s *Server) resolveTools(el *errList, key string, m *Template) {
	var list []TemplateTool
	for i, raw := range m.Tools {
		tk := fmt.Sprintf("%s.tools[%d]", key, i)
		e, err := ParseToolEntry(raw)
		if err != nil {
			el.add(tk, "%v", err)
			continue
		}
		t := s.Tools[e.Name]
		switch {
		case t == nil && isOS(e.Name):
			el.add(tk, "%q is an OS; a machine has its OS in os, not in tools", e.Name)
			continue
		case t == nil && e.Option == "":
			el.add(tk, "unknown tool %q: not a defined tool (hostel config tools lists them); a mise tool needs its version, for example \"node:22\"", e.Name)
			continue
		case t != nil && !t.Screen && e.Option != "":
			el.add(tk, "%q: %s is a defined software tool; its version is what its check script finds, so remove \":%s\"", raw, e.Name, e.Option)
			continue
		case t != nil && t.Screen && e.Option != "" && !slices.Contains(t.Options, e.Option):
			el.add(tk, "%q: %s has no option %q; options: %s", raw, e.Name, e.Option, strings.Join(t.Options, ", "))
			continue
		case t != nil && m.OS != "" && !t.AvailableOn(m.OS):
			el.add(tk, "%s has no scripts for %s; it works on %s", e.Name, m.OS, strings.Join(toolOSes(t), ", "))
			continue
		case slices.ContainsFunc(list, func(o TemplateTool) bool { return o.ToolEntry == e }):
			el.add(tk, "%s is listed twice", raw)
			continue
		case t != nil && slices.ContainsFunc(list, func(o TemplateTool) bool { return o.Name == e.Name && (o.Option == "") != (e.Option == "") }):
			el.add(tk, "%s is listed with and without an option; list all its options, or only the name for all of them", e.Name)
			continue
		}
		list = append(list, TemplateTool{ToolEntry: e, Mise: t == nil})
	}
	for i := 0; i < len(list); i++ {
		t := s.Tools[list[i].Name]
		if t == nil {
			continue
		}
		for _, n := range t.Needs {
			if slices.ContainsFunc(list, func(o TemplateTool) bool { return o.Name == n }) {
				continue
			}
			nt := s.Tools[n]
			if nt != nil && m.OS != "" && !nt.AvailableOn(m.OS) {
				el.add(key+".tools", "%s needs %s, which has no scripts for %s; it works on %s", list[i].Name, n, m.OS, strings.Join(toolOSes(nt), ", "))
				continue
			}
			list = append(list, TemplateTool{ToolEntry: ToolEntry{Name: n}, Mise: nt == nil, Need: true})
		}
	}
	m.ToolList = list
	m.Tools = make([]string, len(list))
	for i, t := range list {
		m.Tools[i] = t.String()
	}
}

// CheckToolList checks a machine's tool list before the machine's OS is known
// (a machine invite): the same rules as a machine in the configuration.
func (s *Server) CheckToolList(tools []string) error {
	el := &errList{}
	s.resolveTools(el, "tools", &Template{Tools: tools})
	return el.err()
}

func toolOSes(t *Tool) []string {
	var out []string
	for _, o := range OSNames {
		if t.AvailableOn(o) {
			out = append(out, o)
		}
	}
	return out
}

// Tool returns the machine's tool with this name, or nil. A screen tool that
// the machine lists with several options gives the first entry.
func (m *Template) Tool(name string) *TemplateTool {
	for i := range m.ToolList {
		if m.ToolList[i].Name == name {
			return &m.ToolList[i]
		}
	}
	return nil
}

// MiseTools returns the mise tools of the machine: name → versions ("latest"
// for an entry without a version).
func (m *Template) MiseTools() map[string][]string {
	out := map[string][]string{}
	for _, t := range m.ToolList {
		if !t.Mise {
			continue
		}
		v := t.Option
		if v == "" {
			v = "latest"
		}
		out[t.Name] = append(out[t.Name], v)
	}
	return out
}

// checkOldKeys reports the keys of the configuration before M10e with the new
// form (owner decision, 2026-11-05: no compatibility). doc names the
// configuration document ("" for server.toml).
func checkOldKeys(file string, data []byte, doc string) error {
	order, err := keyOrder(data)
	if err != nil {
		return nil // the decoder reports syntax errors with their position
	}
	name := file
	if doc != "" {
		name = doc
	}
	var errs []error
	add := func(path []string, msg string) {
		errs = append(errs, fmt.Errorf("%s: %s: %s", name, strings.Join(path, "."), msg))
	}
	var kinds struct {
		Providers map[string]struct {
			Kind string `toml:"kind"`
		} `toml:"providers"`
	}
	_ = toml.Unmarshal(data, &kinds) // the decoder reports syntax errors
	reported := map[string]bool{}
	for _, path := range order {
		switch {
		case len(path) == 1 && path[0] == "platforms_dir" && doc != "":
			add(path, "removed in M10e: the default tools are in the binary, and own tools are in the database (hostel config tools); delete the key")
		case len(path) == 1 && path[0] == "platforms":
			add(path, "removed in M10e: tools replace platforms; a tool is a directory with tool.toml (hostel config tools); delete the table")
		case len(path) == 3 && path[0] == "providers" && path[2] == "machines" && kinds.Providers[path[1]].Kind != ProviderStatic:
			add(path, "changed in M10g: a provider that creates machines has [providers.<name>.templates.<template>]")
		case !isTemplateOrProvider(path[:len(path)-1]):
		case path[len(path)-1] == "slots":
			add(path, "changed in M10g: slots are now rooms; rename the key")
		case path[len(path)-1] == "max_rooms":
			add(path, "changed in M10g: max_rooms is now max_machines; rename the key")
		case path[len(path)-1] == "platforms":
			add(path, "removed in M10e: list the tools of the machine instead, for example tools = [\"ios-simulator\", \"android-emulator\"]")
		case path[len(path)-1] == "labels":
			add(path, "removed in M10e: labels are gone; list tools instead (for example \"xcode\", \"macos-desktop\")")
		case path[len(path)-1] == "requires":
			add(path, "removed in M10e: requires is gone; list tools instead")
		}
		for i := range path {
			if path[i] == "tools" && i+1 < len(path) && isTemplateOrProvider(path[:i]) {
				if tk := strings.Join(path[:i+1], "."); !reported[tk] {
					reported[tk] = true
					add(path[:i+1], "the map form before M10e; tools is now a list of \"name\" or \"name:version\" entries, for example tools = [\"jdk:21\", \"node:22\"]")
				}
			}
		}
	}
	return errors.Join(errs...)
}

// OldForm reports the keys of the configuration before M10e in a
// configuration document, or nil.
func OldForm(data []byte) error { return checkOldKeys("", data, "the configuration") }

// UpgradeWords rewrites the keys of a stored configuration with the words
// before M10g. A server that started without its on-demand providers would
// retire their machines (cost: they stay until their provider-side deadline).
// It parses the document: values, and opaque tables such as spec, stay.
func UpgradeWords(doc []byte) []byte {
	var kinds struct {
		Providers map[string]struct {
			Kind string `toml:"kind"`
		} `toml:"providers"`
	}
	if toml.Unmarshal(doc, &kinds) != nil {
		return doc // the decoder reports the syntax error
	}
	newWord := func(path []string) string {
		n, last := len(path), path[len(path)-1]
		if n < 3 || path[0] != "providers" {
			return ""
		}
		entries := n == 5 && (path[2] == "machines" || path[2] == "templates")
		switch {
		case n == 3 && last == "machines" && kinds.Providers[path[1]].Kind != ProviderStatic,
			n == 4 && path[2] == "limits" && last == "machines":
			return "templates"
		case last == "max_rooms" && (n == 3 || n == 4 && path[2] == "limits" || entries):
			return "max_machines"
		case last == "slots" && entries:
			return "rooms"
		}
		return ""
	}
	type edit struct {
		start, end int
		word       string
	}
	var edits []edit
	walkKey := func(path []string, it unstable.Iterator) []string {
		path = slices.Clone(path)
		for it.Next() {
			n := it.Node()
			path = append(path, string(n.Data))
			if w := newWord(path); w != "" {
				edits = append(edits, edit{int(n.Raw.Offset), int(n.Raw.Offset + n.Raw.Length), w})
			}
		}
		return path
	}
	var walkValue func(path []string, v *unstable.Node)
	walkValue = func(path []string, v *unstable.Node) {
		for it := v.Children(); it.Next(); {
			switch c := it.Node(); {
			case v.Kind == unstable.Array:
				walkValue(path, c)
			case v.Kind == unstable.InlineTable && c.Kind == unstable.KeyValue:
				walkValue(walkKey(path, c.Key()), c.Value())
			}
		}
	}
	p := unstable.Parser{}
	p.Reset(doc)
	var table []string
	for p.NextExpression() {
		e := p.Expression()
		switch e.Kind {
		case unstable.Table, unstable.ArrayTable:
			table = walkKey(nil, e.Key())
		case unstable.KeyValue:
			walkValue(walkKey(table, e.Key()), e.Value())
		}
	}
	if p.Error() != nil {
		return doc
	}
	slices.SortFunc(edits, func(a, b edit) int { return b.start - a.start })
	out := slices.Clone(doc)
	for _, e := range edits {
		out = slices.Replace(out, e.start, e.end, []byte(e.word)...)
	}
	return out
}

// isTemplateOrProvider reports whether a key path is providers.<p> or
// providers.<p>.machines.<m>.
func isTemplateOrProvider(path []string) bool {
	return (len(path) == 2 && path[0] == "providers") ||
		(len(path) == 4 && path[0] == "providers" && (path[2] == "machines" || path[2] == "templates"))
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

// IsWebSocketURL reports whether a gateway address is a WebSocket URL (the
// gateway's /v1/gateway on the API) instead of a TCP host:port.
func IsWebSocketURL(addr string) bool {
	return strings.HasPrefix(addr, "wss://") || strings.HasPrefix(addr, "ws://")
}

// DefinedTools returns the defined tools of a machine in the order of their
// needs: each tool after the tools that it needs (the order of install and
// start).
func (s *Server) DefinedTools(m *Template) []*Tool {
	var out []*Tool
	var visit func(name string)
	visit = func(name string) {
		t := s.Tools[name]
		if t == nil || slices.Contains(out, t) {
			return
		}
		for _, n := range t.Needs {
			visit(n)
		}
		out = append(out, t)
	}
	for _, mt := range m.ToolList {
		visit(mt.Name)
	}
	return out
}

// ToolClosure returns the defined tool name and the defined tools that it
// needs, directly or through other tools.
func (s *Server) ToolClosure(name string) []string {
	var out []string
	var visit func(string)
	visit = func(n string) {
		t := s.Tools[n]
		if t == nil || slices.Contains(out, n) {
			return
		}
		out = append(out, n)
		for _, nn := range t.Needs {
			visit(nn)
		}
	}
	visit(name)
	return out
}
