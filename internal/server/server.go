// Package server is the Hostel server: the HTTP API, the workers, and the gateway.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/provider"
	"github.com/csansoon/hostel/internal/store"
	"github.com/csansoon/hostel/internal/version"
)

// Server holds the running server state.
type Server struct {
	cfg        *config.Server
	store      *store.Store
	log        *slog.Logger
	gatewayKey ssh.Signer
	serverKey  ssh.Signer
	providers  map[string]provider.Provider
	kick       chan struct{}
	workers    workers
	// lastReconcile is used only by the loop goroutine.
	lastReconcile time.Time
}

func newProviders(cfg *config.Server, serverKey ssh.Signer) (map[string]provider.Provider, error) {
	out := map[string]provider.Provider{}
	for _, name := range cfg.ProviderOrder {
		p := cfg.Providers[name]
		switch p.Kind {
		case config.ProviderStatic:
			for _, mn := range p.MachineOrder {
				if p.Machines[mn].Connect {
					return nil, fmt.Errorf("providers.%s.machines.%s: connect = true is not supported yet (milestone M5)", name, mn)
				}
			}
			out[name] = provider.NewStatic(p, serverKey)
		default:
			return nil, fmt.Errorf("providers.%s: kind %q is not supported yet", name, p.Kind)
		}
	}
	return out, nil
}

// Run starts the server and blocks until ctx ends.
func Run(ctx context.Context, cfg *config.Server, log *slog.Logger) error {
	gatewayKey, err := LoadKey(filepath.Join(cfg.DataDir, GatewayHostKeyFile))
	if err != nil {
		return err
	}
	serverKey, err := LoadKey(filepath.Join(cfg.DataDir, ServerKeyFile))
	if err != nil {
		return err
	}
	providers, err := newProviders(cfg, serverKey)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, filepath.Join(cfg.DataDir, "hostel.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	s := &Server{cfg: cfg, store: st, log: log, gatewayKey: gatewayKey, serverKey: serverKey,
		providers: providers, kick: make(chan struct{}, 1)}
	if err := s.ensureStaticRooms(ctx); err != nil {
		return fmt.Errorf("static rooms: %w", err)
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}
	httpServer := &http.Server{Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- httpServer.Serve(ln) }()

	if err := writeJSONFile(filepath.Join(cfg.DataDir, "server.json"), protocol.Addresses{Listen: ln.Addr().String()}); err != nil {
		httpServer.Close()
		return err
	}
	log.Info("server started", "listen", ln.Addr().String(), "server_id", cfg.ServerID, "version", version.Version)

	loopCtx, stopLoop := context.WithCancel(ctx)
	defer s.workers.wg.Wait()
	defer stopLoop()
	s.workers.wg.Add(1)
	go func() {
		defer s.workers.wg.Done()
		s.loop(loopCtx)
	}()

	select {
	case <-ctx.Done():
	case err := <-errc:
		return fmt.Errorf("http server: %w", err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("stop http server: %w", err)
	}
	log.Info("server stopped")
	return nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/info", s.auth(s.handleInfo))
	mux.HandleFunc("GET /v1/status", s.auth(s.handleStatus))
	mux.HandleFunc("POST /v1/tokens", s.auth(s.admin(s.handleCreateToken)))
	mux.HandleFunc("DELETE /v1/tokens/{id}", s.auth(s.admin(s.handleRevokeToken)))
	return mux
}

type tokenKey struct{}

// auth checks the bearer token and passes it in the request context.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || secret == "" {
			writeError(w, http.StatusUnauthorized, "", "missing token: send \"Authorization: Bearer <token>\"")
			return
		}
		tok, err := s.store.TokenBySecret(r.Context(), secret)
		if errors.Is(err, store.ErrNotFound) || (err == nil && tok.Role == protocol.RoleRoom) {
			writeError(w, http.StatusUnauthorized, "", "invalid or revoked token")
			return
		}
		if err != nil {
			s.log.Error("read token", "err", err)
			writeError(w, http.StatusInternalServerError, "", "internal error; see the server log")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), tokenKey{}, tok)))
	}
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, protocol.Info{
		Version:         version.Version,
		GatewayAddress:  s.cfg.GatewayPublic,
		GatewayHostKey:  strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.gatewayKey.PublicKey()))),
		ServerPublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.serverKey.PublicKey()))),
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, protocol.Error{Error: msg, Code: code})
}

// writeJSONFile writes a file with a temporary file and a rename, so a reader
// never sees a partial file.
func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
