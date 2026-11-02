package server

import (
	"errors"
	"net/http"

	"github.com/pelletier/go-toml/v2"

	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/store"
)

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	doc := protocol.ConfigDocument{}
	c, err := s.store.LatestConfig(r.Context())
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, "read configuration", err)
		return
	}
	doc.Revision, doc.Document = c.Revision, c.Document
	if requestToken(r).Role != protocol.RoleAdmin {
		hidden, err := hideSecrets(c.Document)
		if err != nil {
			s.internalError(w, "read configuration", err)
			return
		}
		doc.Document, doc.Hidden = hidden, true
	}
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	var req protocol.PutConfigRequest
	if !decodeBody(w, r, &req) {
		return
	}
	rev, err := s.applyConfig(r.Context(), req.BaseRevision, req.Document, requestToken(r).UserID)
	var cfgErr errConfig
	switch {
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, protocol.CodeConflict, err.Error()+"; get the latest configuration and change it again")
		return
	case errors.As(err, &cfgErr):
		writeError(w, http.StatusUnprocessableEntity, "", cfgErr.msg)
		return
	case err != nil:
		s.internalError(w, "change configuration", err)
		return
	}
	writeJSON(w, http.StatusOK, protocol.ConfigDocument{Revision: rev, Document: req.Document})
}

func (s *Server) handleConfigHistory(w http.ResponseWriter, r *http.Request) {
	revs, err := s.store.ConfigHistory(r.Context(), 100)
	if err != nil {
		s.internalError(w, "read configuration history", err)
		return
	}
	out := []protocol.ConfigRevision{}
	for _, c := range revs {
		user := c.User
		if c.UserID == 0 {
			user = "(server: import of server.toml)"
		}
		out = append(out, protocol.ConfigRevision{Revision: c.Revision, User: user, CreatedAt: c.CreatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

// secretKeys are keys whose values a member does not see.
var secretKeys = map[string]bool{"env": true, "command_env": true, "github_token": true}

// hideSecrets replaces the secret values of a document with "***". The
// result is encoded again (comments and the key order are lost).
func hideSecrets(document string) (string, error) {
	var doc map[string]any
	if err := toml.Unmarshal([]byte(document), &doc); err != nil {
		return "", err
	}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, child := range t {
				if !secretKeys[k] {
					walk(child)
					continue
				}
				if m, ok := child.(map[string]any); ok {
					for name := range m {
						m[name] = "***"
					}
				} else {
					t[k] = "***"
				}
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		case []map[string]any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(doc)
	out, err := toml.Marshal(doc)
	return string(out), err
}
