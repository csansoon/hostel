package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/provider"
)

// providerSecretFile is where the server keeps the client secret of an azure
// provider: in the data directory, not in the configuration document, which
// members can read.
func providerSecretFile(dataDir, provider string) string {
	return filepath.Join(dataDir, "provider-secrets", provider)
}

// handleProviderSecret stores the client secret of an azure provider, then
// tries it. The secret is never logged, never in an event, and never sent back.
func (s *Server) handleProviderSecret(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p := s.config().Providers[name]
	switch {
	case p == nil:
		writeError(w, http.StatusNotFound, "", fmt.Sprintf("no provider %q; \"hostel config providers\" lists them", name))
		return
	case p.Kind != config.ProviderAzure:
		writeError(w, http.StatusBadRequest, "", fmt.Sprintf("the provider %s is %s; only azure providers take a client secret", name, p.Kind))
		return
	}
	var req protocol.ProviderSecret
	if !decodeBody(w, r, &req) {
		return
	}
	secret := strings.TrimSpace(req.Secret)
	if secret == "" || len(secret) > 1024 || strings.ContainsAny(secret, "\r\n") {
		writeError(w, http.StatusBadRequest, "", "the client secret must be one line of at most 1024 characters")
		return
	}
	file := providerSecretFile(s.config().DataDir, name)
	if err := writePrivate(file, secret+"\n"); err != nil {
		s.internalError(w, "store the client secret", err)
		return
	}
	tok := requestToken(r)
	s.store.AddEventBy(r.Context(), time.Now(), "", 0, "config", "client secret of provider "+name+" set", tok.UserID)
	s.log.Info("provider client secret set", "provider", name, "admin", tok.Name)
	var out protocol.ProviderSecretResult
	if prov := s.providerMap()[name]; prov != nil {
		if a, ok := prov.(*provider.Azure); ok {
			a.SignOut()
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		_, err := prov.List(ctx, "")
		cancel()
		if err != nil {
			out.Error = err.Error()
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// writePrivate writes a file that only the server's user can read, through a
// new file and a rename, in a directory that only that user can open.
func writePrivate(file, content string) error {
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".secret-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}
