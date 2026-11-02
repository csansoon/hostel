// Package server is the Hostel server: the HTTP API, the workers, and the gateway.
package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/gateway"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/provider"
	"github.com/csansoon/hostel/internal/roomops"
	"github.com/csansoon/hostel/internal/store"
	"github.com/csansoon/hostel/internal/tlspin"
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
	notify     notifier
	full       fullFlags
	gateway    *gateway.Gateway
	// rooms are the connections of rooms behind NAT.
	rooms *gateway.RoomConns
	// gatewayAddr is the gateway address for guests.
	gatewayAddr string
	// tlsKeyHash is the hash of the own TLS key (TLS self-signed), for invites.
	tlsKeyHash []byte
	// publicURLs are the public_urls with the real listen port.
	publicURLs []string
	// connectLimit limits failed POST /v1/connect attempts.
	connectLimit connectLimiter
	// uploads has the platform files and hashes for each platform and OS.
	uploads map[string]map[string]roomops.Platform
	// lastReconcile is used only by the loop goroutine.
	lastReconcile time.Time
}

func newProviders(ctx context.Context, cfg *config.Server, serverKey ssh.Signer, dialRoom provider.RoomDialer, log *slog.Logger) (map[string]provider.Provider, error) {
	out := map[string]provider.Provider{}
	for _, name := range cfg.ProviderOrder {
		p := cfg.Providers[name]
		switch p.Kind {
		case config.ProviderStatic:
			out[name] = provider.NewStatic(p, serverKey, dialRoom)
		case config.ProviderNamespace:
			for _, mn := range p.MachineOrder {
				m := p.Machines[mn]
				if product, _ := m.Spec["product"].(string); product == "instance" && m.OS != config.OSLinux {
					return nil, fmt.Errorf("providers.%s.machines.%s: %s instances are not supported yet; use a linux machine_type", name, mn, m.OS)
				}
			}
			ns := provider.NewNamespace(p, serverKey, log)
			if err := ns.CheckLogin(ctx); err != nil {
				return nil, fmt.Errorf("providers.%s: %w", name, err)
			}
			out[name] = ns
		case config.ProviderExternal:
			ext := provider.NewExternal(p, serverKey, log)
			if err := ext.LoadCapabilities(ctx); err != nil {
				return nil, fmt.Errorf("providers.%s: %w", name, err)
			}
			out[name] = ext
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
	rooms := gateway.NewRoomConns()
	providers, err := newProviders(ctx, cfg, serverKey, rooms.Dial, log)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, filepath.Join(cfg.DataDir, "hostel.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	s := &Server{cfg: cfg, store: st, log: log, gatewayKey: gatewayKey, serverKey: serverKey,
		providers: providers, rooms: rooms, kick: make(chan struct{}, 1)}
	st.SetLeaseHook(s.notify.notify)
	if err := s.loadPlatformUploads(); err != nil {
		return err
	}
	if err := st.ResetTimers(ctx, time.Now()); err != nil {
		return err
	}
	if err := s.ensureStaticRooms(ctx); err != nil {
		return fmt.Errorf("static rooms: %w", err)
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}
	if cfg.TLS == config.TLSSelfSigned {
		if err := tlspin.EnsureCert(cfg.DataDir); err != nil {
			ln.Close()
			return err
		}
		cert, hash, err := tlspin.Load(cfg.DataDir)
		if err != nil {
			ln.Close()
			return err
		}
		s.tlsKeyHash = hash
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	}
	gwLn, err := net.Listen("tcp", cfg.GatewayListen)
	if err != nil {
		ln.Close()
		return fmt.Errorf("listen on %s: %w", cfg.GatewayListen, err)
	}
	listenURL := "http://" + ln.Addr().String()
	if cfg.TLS == config.TLSSelfSigned {
		listenURL = "https://" + ln.Addr().String()
	}
	s.publicURLs = ResolveURLs(cfg.PublicURLs, ln.Addr().String())
	if len(s.publicURLs) == 0 {
		log.Warn("server.toml has no public_urls; invite codes have only the listen address", "url", listenURL)
		s.publicURLs = []string{listenURL}
	}
	s.gatewayAddr = cfg.GatewayPublic
	if cfg.GatewayPublic != config.GatewayPath {
		s.gatewayAddr = publicAddr(cfg.GatewayPublic, gwLn.Addr(), ln.Addr())
	}
	s.gateway = gateway.New(gatewayKey, gwBackend{s}, rooms, log)
	httpServer := &http.Server{Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 2)
	go func() { errc <- httpServer.Serve(ln) }()
	go func() { errc <- s.gateway.Serve(gwLn) }()
	defer gwLn.Close()

	if err := writeJSONFile(filepath.Join(cfg.DataDir, "server.json"),
		protocol.Addresses{Listen: ln.Addr().String(), Gateway: gwLn.Addr().String(), URL: listenURL}); err != nil {
		httpServer.Close()
		return err
	}
	log.Info("server started", "listen", ln.Addr().String(), "gateway", gwLn.Addr().String(),
		"server_id", cfg.ServerID, "version", version.Version)

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
		return fmt.Errorf("listener stopped: %w", err)
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
	// "hostel room connect" reads the gateway address with a room token.
	mux.HandleFunc("GET /v1/info", s.authRoom(s.handleInfo))
	mux.HandleFunc("GET /v1/status", s.auth(s.handleStatus))
	mux.HandleFunc("GET /v1/platforms", s.auth(s.handlePlatforms))
	mux.HandleFunc("POST /v1/leases", s.auth(s.handleCreateLease))
	mux.HandleFunc("GET /v1/leases/{id}", s.auth(s.handleGetLease))
	mux.HandleFunc("POST /v1/leases/{id}/touch", s.auth(s.handleTouch))
	mux.HandleFunc("POST /v1/leases/{id}/release", s.auth(s.handleRelease))
	mux.HandleFunc("PUT /v1/leases/{id}/tunnels", s.auth(s.handleSetTunnels))
	mux.HandleFunc("POST /v1/rooms/{id}/drain", s.auth(s.admin(s.handleRoomDrain(true))))
	mux.HandleFunc("POST /v1/rooms/{id}/undrain", s.auth(s.admin(s.handleRoomDrain(false))))
	mux.HandleFunc("POST /v1/rooms/{id}/repair", s.auth(s.admin(s.handleRoomRepair)))
	mux.HandleFunc("POST /v1/rooms/{id}/destroy", s.auth(s.admin(s.handleRoomDestroy)))
	mux.HandleFunc("POST /v1/connect", s.handleConnect)
	mux.HandleFunc("GET /v1/me", s.auth(s.handleMe))
	mux.HandleFunc("DELETE /v1/me", s.auth(s.handleDisconnect))
	mux.HandleFunc("GET /v1/users", s.auth(s.handleUsers))
	mux.HandleFunc("POST /v1/invites", s.auth(s.handleInvite))
	mux.HandleFunc("PATCH /v1/users/{name}", s.auth(s.admin(s.handleSetUser)))
	mux.HandleFunc("DELETE /v1/users/{name}", s.auth(s.admin(s.handleRemoveUser)))
	mux.HandleFunc("DELETE /v1/users/{name}/computers/{computer}", s.auth(s.admin(s.handleRemoveComputer)))
	mux.HandleFunc("POST /v1/leases/{id}/move", s.auth(s.admin(s.handleMove)))
	mux.HandleFunc("POST /v1/leases/{id}/priority", s.auth(s.admin(s.handlePriority)))
	mux.HandleFunc("POST /v1/tokens", s.auth(s.admin(s.handleCreateToken)))
	mux.HandleFunc("DELETE /v1/tokens/{id}", s.auth(s.admin(s.handleRevokeToken)))
	// The gateway's SSH over a WebSocket; SSH authenticates inside.
	mux.Handle("GET /v1/gateway", s.gateway)
	return mux
}

type tokenKey struct{}

// auth checks the bearer token and passes it in the request context. Room
// tokens are refused.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc { return s.authWith(next, false) }

// authRoom is auth that also accepts room tokens.
func (s *Server) authRoom(next http.HandlerFunc) http.HandlerFunc { return s.authWith(next, true) }

func (s *Server) authWith(next http.HandlerFunc, allowRoom bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || secret == "" {
			writeError(w, http.StatusUnauthorized, "", "missing token: send \"Authorization: Bearer <token>\"")
			return
		}
		tok, err := s.store.TokenBySecret(r.Context(), secret)
		if errors.Is(err, store.ErrNotFound) || (err == nil && tok.Role == protocol.RoleRoom && !allowRoom) {
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
		GatewayAddress:  s.gatewayAddr,
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

// publicAddr returns the gateway address for guests: gateway_public, with the
// real port when it has port 0 (the API's port for a WebSocket URL).
func publicAddr(public string, real, api net.Addr) string {
	// A WebSocket gateway is on the API listener.
	if config.IsWebSocketURL(public) {
		u, err := url.Parse(public)
		if err != nil || u.Port() != "0" {
			return public
		}
		u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(api.(*net.TCPAddr).Port))
		return u.String()
	}
	host, port, err := net.SplitHostPort(public)
	if err != nil || port != "0" {
		return public
	}
	return net.JoinHostPort(host, strconv.Itoa(real.(*net.TCPAddr).Port))
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
