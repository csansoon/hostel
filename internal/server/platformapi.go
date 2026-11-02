package server

import (
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/store"
	"github.com/csansoon/hostel/platforms"
)

func (s *Server) handleConfigPlatforms(w http.ResponseWriter, r *http.Request) {
	latest, err := s.store.LatestPlatforms(r.Context())
	if err != nil {
		s.internalError(w, "read platforms", err)
		return
	}
	cfg := s.config()
	out := []protocol.ConfigPlatform{}
	for _, name := range config.PlatformNames(cfg.Platforms) {
		p := cfg.Platforms[name]
		cp := protocol.ConfigPlatform{Name: name, Kind: p.Kind, Description: p.Description,
			Source: platformSource(name, latest), Machines: []string{}}
		if rev, ok := latest[name]; ok {
			cp.Revision, cp.User, cp.ChangedAt = rev.Revision, rev.User, rev.CreatedAt
			if rev.UserID == 0 {
				cp.User = "(import)"
			}
		}
		for _, m := range cfg.Machines() {
			if m.OS == name || slices.Contains(m.Platforms, name) {
				cp.Machines = append(cp.Machines, m.Key())
			}
		}
		out = append(out, cp)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetPlatformFiles(w http.ResponseWriter, r *http.Request) {
	latest, err := s.store.LatestPlatforms(r.Context())
	if err != nil {
		s.internalError(w, "read platforms", err)
		return
	}
	name := r.PathValue("name")
	files, ok := effectivePlatforms(latest)[name]
	if !ok {
		if slices.Contains(config.OSNames, name) {
			files = map[string]string{}
		} else {
			writeError(w, http.StatusNotFound, "", fmt.Sprintf("no platform %q; \"hostel config platforms\" lists them", name))
			return
		}
	}
	writeJSON(w, http.StatusOK, protocol.PlatformFiles{Name: name, Files: files})
}

func (s *Server) handlePutPlatform(w http.ResponseWriter, r *http.Request) {
	var req protocol.PlatformFiles
	if !decodeBody(w, r, &req) {
		return
	}
	name := r.PathValue("name")
	if !config.ValidName(name) {
		writeError(w, http.StatusBadRequest, "", fmt.Sprintf("%q is not a valid platform name: use lowercase letters, digits, and -, starting with a letter", name))
		return
	}
	for f := range req.Files {
		if !config.IsPlatformFile(f) {
			writeError(w, http.StatusUnprocessableEntity, "", fmt.Sprintf("%s: %q is not a platform file; a platform has platform.toml, README.md, and scripts named <step>.<os>.sh or .ps1", name, f))
			return
		}
	}
	state := store.PlatformFiles
	if isDefault(name, req.Files) {
		state = store.PlatformDefault
	}
	s.platformChange(w, r, name, state, req.Files)
}

func (s *Server) handleRemovePlatform(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if slices.Contains(config.OSNames, name) {
		writeError(w, http.StatusBadRequest, "", fmt.Sprintf("%s is an OS platform: it cannot be removed; use \"hostel config platforms reset %s\", or push a folder without scripts", name, name))
		return
	}
	if _, ok := s.config().Platforms[name]; !ok {
		writeError(w, http.StatusNotFound, "", fmt.Sprintf("no platform %q; \"hostel config platforms\" lists them", name))
		return
	}
	for _, m := range s.config().Machines() {
		if slices.Contains(m.Platforms, name) {
			writeError(w, http.StatusConflict, "", fmt.Sprintf("the machine %s lists the platform %s; remove it from the machine's platforms first", m.Key(), name))
			return
		}
	}
	s.platformChange(w, r, name, store.PlatformRemoved, nil)
}

func (s *Server) handleResetPlatform(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if platforms.Defaults()[name] == nil {
		writeError(w, http.StatusBadRequest, "", fmt.Sprintf("%s is not a default platform (defaults: android, ios, macos, windows)", name))
		return
	}
	s.platformChange(w, r, name, store.PlatformDefault, nil)
}

func (s *Server) platformChange(w http.ResponseWriter, r *http.Request, name, state string, files map[string]string) {
	rev, err := s.applyPlatform(r.Context(), name, state, files, requestToken(r).UserID)
	var cfgErr errConfig
	switch {
	case errors.As(err, &cfgErr):
		writeError(w, http.StatusUnprocessableEntity, "", cfgErr.msg)
	case err != nil:
		s.internalError(w, "change platform", err)
	default:
		latest, err := s.store.LatestPlatforms(r.Context())
		if err != nil {
			s.internalError(w, "read platforms", err)
			return
		}
		writeJSON(w, http.StatusOK, protocol.ConfigPlatform{Name: name, Revision: rev, Source: platformSource(name, latest)})
	}
}
