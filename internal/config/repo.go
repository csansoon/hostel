package config

import (
	"fmt"
	"maps"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
)

// Repo is the repository file hostel.toml (design, section 9).
type Repo struct {
	// Tools are what "hostel acquire" asks for when no tools are given.
	Tools   []string             `toml:"tools"`
	Include []string             `toml:"include"`
	Mise    *bool                `toml:"mise"`
	Setup   []string             `toml:"setup"`
	Env     map[string]string    `toml:"env"`
	Tunnels map[string]*Tunnel   `toml:"tunnels"`
	Tool    map[string]*RepoTool `toml:"tool"`

	File string `toml:"-"`
}

// Tunnel is a machine → guest tunnel (design, section 14).
type Tunnel struct {
	LocalPort    int    `toml:"local_port"`
	LocalPortEnv string `toml:"local_port_env"`
	MachinePort  int    `toml:"machine_port"`
}

// RepoTool holds what changes when the lease has the tool: its setup runs
// after the top-level setup, and its env replaces keys of the top-level env.
type RepoTool struct {
	Setup []string          `toml:"setup"`
	Env   map[string]string `toml:"env"`
}

var placeholderRE = regexp.MustCompile(`\{tunnels\.([^{}]*)\.port\}`)

// MiseEnabled reports whether the setup job runs the repository-level mise steps.
func (r *Repo) MiseEnabled() bool { return r.Mise == nil || *r.Mise }

// TunnelNames returns the tunnel names in name order.
func (r *Repo) TunnelNames() []string {
	names := make([]string, 0, len(r.Tunnels))
	for n := range r.Tunnels {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// LoadRepo reads and validates a repository file.
func LoadRepo(file string) (*Repo, error) {
	r := &Repo{}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	if keys, err := topKeys(data); err == nil && slices.Contains(keys, "platforms") {
		return nil, fmt.Errorf("%s: platforms: the form before M10e; use the top-level keys tools, setup, and env, and [tool.<name>] for what changes with a tool (docs/design.md, section 9)", file)
	}
	if err := decodeData(file, data, r); err != nil {
		return nil, err
	}
	r.File = file
	el := &errList{file: file}
	for i, pattern := range r.Include {
		if _, err := path.Match(pattern, ""); err != nil || pattern == "" {
			el.add(fmt.Sprintf("include[%d]", i), "invalid glob pattern %q", pattern)
		}
	}
	fixed := map[int]string{}
	for _, name := range r.TunnelNames() {
		t := r.Tunnels[name]
		key := "tunnels." + name
		if other, ok := fixed[t.MachinePort]; ok && t.MachinePort != 0 {
			el.add(key+".machine_port", "machine port %d is also the machine port of tunnel %s", t.MachinePort, other)
		}
		if t.MachinePort != 0 {
			fixed[t.MachinePort] = name
		}
		if !nameRE.MatchString(name) {
			el.add(key, "tunnel name must match %s", nameRE)
		}
		if (t.LocalPort == 0) == (t.LocalPortEnv == "") {
			el.add(key, "set exactly one of local_port and local_port_env")
		}
		if t.LocalPort != 0 && (t.LocalPort < 1 || t.LocalPort > 65535) {
			el.add(key+".local_port", "port %d is not between 1 and 65535", t.LocalPort)
		}
		if t.MachinePort != 0 && (t.MachinePort < 1024 || t.MachinePort > 65535) {
			el.add(key+".machine_port", "port %d is not between 1024 and 65535", t.MachinePort)
		}
	}
	for i, t := range r.Tools {
		if _, err := ParseToolEntry(t); err != nil {
			el.add(fmt.Sprintf("tools[%d]", i), "%v", err)
		}
	}
	r.checkEnv(el, "env", r.Env)
	for _, name := range sortedKeys(r.Tool) {
		key := "tool." + name
		if !nameRE.MatchString(name) {
			el.add(key, "tool name must match %s", nameRE)
		}
		if t := r.Tool[name]; t != nil {
			r.checkEnv(el, key+".env", t.Env)
		}
	}
	return r, el.err()
}

func (r *Repo) checkEnv(el *errList, key string, env map[string]string) {
	for k, v := range env {
		if k == "" {
			el.add(key, "empty variable name")
		}
		for _, m := range placeholderRE.FindAllStringSubmatch(v, -1) {
			if r.Tunnels[m[1]] == nil {
				el.add(key+"."+k, "placeholder %s names an unknown tunnel %q", m[0], m[1])
			}
		}
	}
}

// LeaseSetup returns the setup commands and the env for a lease with these
// tools: the top level, then the [tool.<name>] of each tool of the lease, in
// the lease's order.
func (r *Repo) LeaseSetup(tools []string) ([]string, map[string]string) {
	setup := slices.Clone(r.Setup)
	env := maps.Clone(r.Env)
	if env == nil {
		env = map[string]string{}
	}
	for _, raw := range tools {
		e, _ := ParseToolEntry(raw)
		if t := r.Tool[e.Name]; t != nil {
			setup = append(setup, t.Setup...)
			maps.Copy(env, t.Env)
		}
	}
	return setup, env
}

// ExpandEnv replaces each {tunnels.<name>.port} placeholder with the machine port of
// that tunnel.
func ExpandEnv(env map[string]string, machinePorts map[string]int) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = placeholderRE.ReplaceAllStringFunc(v, func(m string) string {
			name := placeholderRE.FindStringSubmatch(m)[1]
			return fmt.Sprint(machinePorts[name])
		})
	}
	return out
}
