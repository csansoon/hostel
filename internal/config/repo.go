package config

import (
	"fmt"
	"path"
	"regexp"
	"sort"
)

// Repo is the repository file hostel.toml (design, section 9).
type Repo struct {
	Include   []string                 `toml:"include"`
	Mise      *bool                    `toml:"mise"`
	Tunnels   map[string]*Tunnel       `toml:"tunnels"`
	Platforms map[string]*RepoPlatform `toml:"platforms"`

	File string `toml:"-"`
}

// Tunnel is a room → guest tunnel (design, section 14).
type Tunnel struct {
	LocalPort    int    `toml:"local_port"`
	LocalPortEnv string `toml:"local_port_env"`
	RoomPort     int    `toml:"room_port"`
}

type RepoPlatform struct {
	Devices  []string          `toml:"devices"`
	Requires []string          `toml:"requires"`
	Setup    []string          `toml:"setup"`
	Env      map[string]string `toml:"env"`
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
	if _, err := decodeFile(file, r); err != nil {
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
		if other, ok := fixed[t.RoomPort]; ok && t.RoomPort != 0 {
			el.add(key+".room_port", "room port %d is also the room port of tunnel %s", t.RoomPort, other)
		}
		if t.RoomPort != 0 {
			fixed[t.RoomPort] = name
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
		if t.RoomPort != 0 && (t.RoomPort < 1024 || t.RoomPort > 65535) {
			el.add(key+".room_port", "port %d is not between 1024 and 65535", t.RoomPort)
		}
	}
	for pn, p := range r.Platforms {
		key := "platforms." + pn
		if !nameRE.MatchString(pn) {
			el.add(key, "platform name must match %s", nameRE)
		}
		for k, v := range p.Env {
			if k == "" {
				el.add(key+".env", "empty variable name")
			}
			for _, m := range placeholderRE.FindAllStringSubmatch(v, -1) {
				if r.Tunnels[m[1]] == nil {
					el.add(key+".env."+k, "placeholder %s names an unknown tunnel %q", m[0], m[1])
				}
			}
		}
	}
	return r, el.err()
}

// ExpandEnv replaces each {tunnels.<name>.port} placeholder with the room port of
// that tunnel.
func ExpandEnv(env map[string]string, roomPorts map[string]int) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = placeholderRE.ReplaceAllStringFunc(v, func(m string) string {
			name := placeholderRE.FindStringSubmatch(m)[1]
			return fmt.Sprint(roomPorts[name])
		})
	}
	return out
}
