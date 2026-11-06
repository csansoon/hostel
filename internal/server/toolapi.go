package server

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/store"
)

func (s *Server) handleConfigTools(w http.ResponseWriter, r *http.Request) {
	latest, err := s.store.LatestTools(r.Context())
	if err != nil {
		s.internalError(w, "read tools", err)
		return
	}
	cfg := s.config()
	out := []protocol.ConfigTool{}
	for _, name := range config.ToolNames(cfg.Tools) {
		t := cfg.Tools[name]
		ct := protocol.ConfigTool{Name: name, Screen: t.Screen, Description: t.Description,
			Source: toolSource(name, latest), Machines: []string{}}
		if rev, ok := latest[name]; ok {
			ct.Revision, ct.User, ct.ChangedAt = rev.Revision, rev.User, rev.CreatedAt
		}
		for _, m := range cfg.Machines() {
			if m.Tool(name) != nil {
				ct.Machines = append(ct.Machines, m.Key())
			}
		}
		out = append(out, ct)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetToolFiles(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	files, ok := s.live.Load().tools[name]
	if !ok {
		writeError(w, http.StatusNotFound, "", fmt.Sprintf("no defined tool %q; \"hostel config tools\" lists them", name))
		return
	}
	writeJSON(w, http.StatusOK, protocol.ToolFiles{Name: name, Files: files})
}

func (s *Server) handlePutTool(w http.ResponseWriter, r *http.Request) {
	var req protocol.ToolFiles
	if !decodeBody(w, r, &req) {
		return
	}
	name := r.PathValue("name")
	if !config.ValidName(name) {
		writeError(w, http.StatusBadRequest, "", fmt.Sprintf("%q is not a valid tool name: use lowercase letters, digits, and -, starting with a letter", name))
		return
	}
	for f := range req.Files {
		if !config.IsToolFile(f) {
			writeError(w, http.StatusUnprocessableEntity, "", fmt.Sprintf("tool %s: %q is not a tool file; a tool has %s, %s, and scripts named <step>.<os>.sh or .ps1", name, f, config.ToolFile, config.GuideFile))
			return
		}
	}
	state := store.ToolFiles
	if isDefault(name, req.Files) {
		state = store.ToolDefault
	}
	s.toolChange(w, r, name, state, req.Files)
}

func (s *Server) handleRemoveTool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg := s.config()
	if cfg.Tools[name] == nil {
		writeError(w, http.StatusNotFound, "", fmt.Sprintf("no defined tool %q; \"hostel config tools\" lists them", name))
		return
	}
	for _, m := range cfg.Machines() {
		if mt := m.Tool(name); mt != nil {
			what := "lists"
			if mt.Need {
				what = "needs (through another tool)"
			}
			writeError(w, http.StatusConflict, "", fmt.Sprintf("the machine %s %s the tool %s; remove it from the machine's tools first", m.Key(), what, name))
			return
		}
	}
	for _, other := range config.ToolNames(cfg.Tools) {
		for _, n := range cfg.Tools[other].Needs {
			if n == name {
				writeError(w, http.StatusConflict, "", fmt.Sprintf("the tool %s needs %s; change or remove %s first", other, name, other))
				return
			}
		}
	}
	s.toolChange(w, r, name, store.ToolRemoved, nil)
}

func (s *Server) handleResetTool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	defaults := config.DefaultTools()
	if defaults[name] == nil {
		writeError(w, http.StatusBadRequest, "", fmt.Sprintf("%s is not a default tool (defaults: %s)", name, strings.Join(sortedNames(defaults), ", ")))
		return
	}
	s.toolChange(w, r, name, store.ToolDefault, nil)
}

func sortedNames(m map[string]map[string]string) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (s *Server) toolChange(w http.ResponseWriter, r *http.Request, name, state string, files map[string]string) {
	rev, err := s.applyTool(r.Context(), name, state, files, requestToken(r).UserID)
	var cfgErr errConfig
	switch {
	case errors.As(err, &cfgErr):
		writeError(w, http.StatusUnprocessableEntity, "", cfgErr.msg)
	case err != nil:
		s.internalError(w, "change tool", err)
	default:
		latest, err := s.store.LatestTools(r.Context())
		if err != nil {
			s.internalError(w, "read tools", err)
			return
		}
		writeJSON(w, http.StatusOK, protocol.ConfigTool{Name: name, Revision: rev, Source: toolSource(name, latest)})
	}
}
