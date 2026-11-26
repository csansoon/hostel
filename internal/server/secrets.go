package server

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
)

// MaxSecretSize bounds a secret value (M14): a certificate or a key, not data.
const MaxSecretSize = 64 << 10

// secretFile is where the server keeps a secret that tools get in a lease. The
// value is never logged, never in an event, and never sent to a client.
func secretFile(dataDir, name string) string {
	return filepath.Join(dataDir, "secrets", name)
}

// secretTools returns the tools that declare a secret, sorted.
func secretTools(cfg *config.Server, name string) []string {
	out := []string{}
	for _, tn := range config.ToolNames(cfg.Tools) {
		if slices.Contains(cfg.Tools[tn].Secrets, name) {
			out = append(out, tn)
		}
	}
	return out
}

func (s *Server) handleSecrets(w http.ResponseWriter, r *http.Request) {
	cfg := s.config()
	des, err := os.ReadDir(filepath.Join(cfg.DataDir, "secrets"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.internalError(w, "list the secrets", err)
		return
	}
	out := []protocol.Secret{}
	seen := map[string]bool{}
	for _, de := range des {
		info, err := de.Info()
		if err != nil || !info.Mode().IsRegular() || !config.SecretNameRE.MatchString(de.Name()) {
			continue
		}
		seen[de.Name()] = true
		out = append(out, protocol.Secret{Name: de.Name(), Set: true, Size: info.Size(), SetAt: info.ModTime().UTC(), Tools: secretTools(cfg, de.Name())})
	}
	// Secrets that a tool declares but that are not set yet.
	for _, tn := range config.ToolNames(cfg.Tools) {
		for _, name := range cfg.Tools[tn].Secrets {
			if !seen[name] {
				seen[name] = true
				out = append(out, protocol.Secret{Name: name, Tools: secretTools(cfg, name)})
			}
		}
	}
	slices.SortFunc(out, func(a, b protocol.Secret) int { return strings.Compare(a.Name, b.Name) })
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSetSecret(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !config.SecretNameRE.MatchString(name) {
		writeError(w, http.StatusBadRequest, "", fmt.Sprintf("the secret name %q must match %s", name, config.SecretNameRE))
		return
	}
	var req protocol.SetSecret
	if !decodeBody(w, r, &req) {
		return
	}
	if len(req.Value) == 0 || len(req.Value) > MaxSecretSize {
		writeError(w, http.StatusBadRequest, "", fmt.Sprintf("the value must have 1 to %d bytes", MaxSecretSize))
		return
	}
	if err := writePrivate(secretFile(s.config().DataDir, name), string(req.Value)); err != nil {
		s.internalError(w, "store the secret", err)
		return
	}
	tok := requestToken(r)
	s.store.AddEventBy(r.Context(), time.Now(), "", 0, "config", "secret "+name+" set", tok.UserID)
	s.log.Info("secret set", "secret", name, "admin", tok.Name)
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *Server) handleRemoveSecret(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !config.SecretNameRE.MatchString(name) {
		writeError(w, http.StatusBadRequest, "", fmt.Sprintf("the secret name %q must match %s", name, config.SecretNameRE))
		return
	}
	cfg := s.config()
	if tools := secretTools(cfg, name); len(tools) > 0 {
		writeError(w, http.StatusConflict, "", fmt.Sprintf("the tool %s uses the secret %s; remove it from the tool's secrets first, or replace the value with \"hostel config secrets set %s <file>\"", strings.Join(tools, ", "), name, name))
		return
	}
	err := os.Remove(secretFile(cfg.DataDir, name))
	if errors.Is(err, fs.ErrNotExist) {
		writeError(w, http.StatusNotFound, "", fmt.Sprintf("no secret %q; \"hostel config secrets\" lists them", name))
		return
	}
	if err != nil {
		s.internalError(w, "remove the secret", err)
		return
	}
	tok := requestToken(r)
	s.store.AddEventBy(r.Context(), time.Now(), "", 0, "config", "secret "+name+" removed", tok.UserID)
	s.log.Info("secret removed", "secret", name, "admin", tok.Name)
	writeJSON(w, http.StatusOK, struct{}{})
}

// leaseSecretNames returns the secrets of a lease's tools and their needs.
func leaseSecretNames(cfg *config.Server, tools []string) []string {
	var out []string
	for _, t := range leaseClosure(cfg, tools) {
		for _, n := range t.Secrets {
			if !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}
	return out
}

// missingSecret returns the message for the first secret of a lease's tools
// that is not set, or "".
func missingSecret(cfg *config.Server, tools []string) string {
	for _, t := range leaseClosure(cfg, tools) {
		for _, n := range t.Secrets {
			if _, err := os.Stat(secretFile(cfg.DataDir, n)); err != nil {
				return fmt.Sprintf("the tool %s needs the secret %s, which is not set; an admin runs \"hostel config secrets set %s <file>\"", t.Name, n, n)
			}
		}
	}
	return ""
}

// leaseSecrets reads the secrets of a lease's tools.
func leaseSecrets(cfg *config.Server, tools []string) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, n := range leaseSecretNames(cfg, tools) {
		data, err := os.ReadFile(secretFile(cfg.DataDir, n))
		if err != nil {
			return nil, fmt.Errorf("read the secret %s (an admin sets it with \"hostel config secrets set %s <file>\"): %w", n, n, err)
		}
		out[n] = data
	}
	return out, nil
}
