package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/invite"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/store"
	"github.com/csansoon/hostel/internal/tlspin"
)

// inviteTTL is how long an invite code works.
const inviteTTL = 24 * time.Hour

// userNameRE: a person's name for the status (letters, digits, spaces, and
// a few signs).
var userNameRE = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} .'_-]{0,59}$`)

// NameProblem describes what is wrong with a user name, or returns "".
func NameProblem(name string) string {
	if userNameRE.MatchString(name) {
		return ""
	}
	return fmt.Sprintf("%q is not a valid name: use 1 to 60 letters, digits, spaces, and . ' _ -, starting with a letter or digit", name)
}

// connectLimiter allows a few failed connect attempts per source address and
// minute, so nobody can guess invite secrets.
type connectLimiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
}

const connectFailures = 10

func (c *connectLimiter) blocked(addr string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	var recent []time.Time
	for _, t := range c.failures[addr] {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	if len(recent) == 0 {
		delete(c.failures, addr)
		return false
	}
	c.failures[addr] = recent
	return len(recent) >= connectFailures
}

// clientAddr is the address that the limit counts: behind a proxy (TLS off),
// the client address that the proxy added last to X-Forwarded-For (a client
// can write the earlier entries itself); else the connection's address.
func (s *Server) clientAddr(r *http.Request) string {
	if s.cfg.TLS == config.TLSOff {
		if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
			parts := strings.Split(xff[len(xff)-1], ",")
			if a := strings.TrimSpace(parts[len(parts)-1]); a != "" {
				return a
			}
		}
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func (c *connectLimiter) fail(addr string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failures == nil {
		c.failures = map[string][]time.Time{}
	}
	c.failures[addr] = append(c.failures[addr], now)
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	src := s.clientAddr(r)
	if s.connectLimit.blocked(src, now) {
		writeError(w, http.StatusTooManyRequests, "", "too many failed attempts; wait a minute")
		return
	}
	var req protocol.ConnectRequest
	if !decodeBody(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if p := NameProblem(req.Name); p != "" {
		writeError(w, http.StatusBadRequest, "", p)
		return
	}
	if !config.ValidName(req.Computer) {
		writeError(w, http.StatusBadRequest, "", fmt.Sprintf("%q is not a valid computer name", req.Computer))
		return
	}
	u, _, secret, err := s.store.UseInvite(r.Context(), req.Secret, req.Name, req.Computer, now)
	switch {
	case errors.Is(err, store.ErrInviteInvalid):
		s.connectLimit.fail(src, now)
		writeError(w, http.StatusForbidden, "", "the invite code is not valid: it was used, it expired, or it is wrong; ask an admin for a new one")
		return
	case errors.Is(err, store.ErrNameTaken):
		writeError(w, http.StatusConflict, "name_taken", fmt.Sprintf("another user has the name %q; connect with another name (--name)", req.Name))
		return
	case errors.Is(err, store.ErrInviteNameTaken):
		writeError(w, http.StatusConflict, "name_taken", "the name in this invite belongs to another user now; ask the admin for a new invite")
		return
	case err != nil:
		s.internalError(w, "use invite", err)
		return
	}
	s.store.AddEventBy(r.Context(), now, "", 0, "user", fmt.Sprintf("%s connected from %s", u.Name, req.Computer), u.ID)
	writeJSON(w, http.StatusOK, protocol.ConnectResponse{ServerID: s.cfg.ServerID, User: u.Name, Role: u.Role, Token: secret})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	tok := requestToken(r)
	writeJSON(w, http.StatusOK, protocol.Me{ServerID: s.cfg.ServerID, User: tok.Name, Role: tok.Role, Computer: tok.Computer})
}

// handleDisconnect revokes the caller's token (hostel disconnect).
func (s *Server) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	tok := requestToken(r)
	if err := s.store.RevokeToken(r.Context(), tok.ID, time.Now()); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, "revoke token", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.Users(r.Context())
	if err != nil {
		s.internalError(w, "read users", err)
		return
	}
	out := []protocol.User{}
	for _, u := range users {
		out = append(out, protocol.User{Name: u.Name, Role: u.Role, Computers: append([]string{}, u.Computers...), CreatedAt: u.CreatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

// ResolveURLs replaces the port 0 of public URLs with the port of listen (the
// address that the listener got; tests listen on port 0).
func ResolveURLs(urls []string, listen string) []string {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return urls
	}
	out := make([]string, len(urls))
	for i, u := range urls {
		pu, err := url.Parse(u)
		if err != nil || pu.Port() != "0" {
			out[i] = u
			continue
		}
		pu.Host = net.JoinHostPort(pu.Hostname(), port)
		out[i] = pu.String()
	}
	return out
}

// LocalInvite makes an invite on the server machine ("hostel server init" and
// "hostel server invite"), without the API: the first admin, or a new admin
// when nobody can sign in as admin. It returns the code and the key
// fingerprint ("" with TLS off).
func LocalInvite(ctx context.Context, paths config.Paths, inv store.Invite) (string, string, error) {
	inv.Name = strings.TrimSpace(inv.Name)
	if inv.Name != "" {
		if p := NameProblem(inv.Name); p != "" {
			return "", "", errors.New(p)
		}
	}
	// ReadPaths does not validate: check what goes into the code.
	if p := config.TLSProblem(paths.TLS); paths.TLS != "" && p != "" {
		return "", "", fmt.Errorf("server.toml: tls: %s", p)
	}
	for i, u := range paths.PublicURLs {
		if p := config.PublicURLProblem(u, paths.TLS); p != "" {
			return "", "", fmt.Errorf("server.toml: public_urls[%d]: %s", i, p)
		}
	}
	urls := paths.PublicURLs
	if data, err := os.ReadFile(filepath.Join(paths.DataDir, "server.json")); err == nil {
		var a protocol.Addresses
		if json.Unmarshal(data, &a) == nil {
			urls = ResolveURLs(urls, a.Listen)
			if len(urls) == 0 && a.URL != "" {
				urls = []string{a.URL}
			}
		}
	}
	if len(urls) == 0 {
		return "", "", errors.New("server.toml has no public_urls: add the addresses that clients use, for example public_urls = [\"https://100.80.1.2:7410\"], or start the server first")
	}
	for _, u := range urls {
		if pu, err := url.Parse(u); err == nil && pu.Port() == "0" {
			return "", "", fmt.Errorf("public URL %s has port 0: start the server first, or set a fixed port", u)
		}
	}
	var hash []byte
	fingerprint := ""
	if paths.TLS == config.TLSSelfSigned {
		var err error
		if _, hash, err = tlspin.Load(paths.DataDir); err != nil {
			return "", "", err
		}
		fingerprint = tlspin.Fingerprint(hash)
	}
	st, err := store.Open(ctx, filepath.Join(paths.DataDir, "hostel.db"))
	if err != nil {
		return "", "", err
	}
	defer st.Close()
	if inv.ExpiresAt.IsZero() {
		inv.ExpiresAt = time.Now().Add(inviteTTL)
	}
	secret, err := st.CreateInvite(ctx, inv, time.Now())
	if err != nil {
		return "", "", err
	}
	code, err := invite.Encode(invite.Code{Addresses: urls, KeyHash: hash, Secret: secret})
	return code, fingerprint, err
}

func validRole(role string) bool { return role == protocol.RoleAdmin || role == protocol.RoleMember }

// handleInvite makes an invite code: a new user (admin), or with Self a code
// that adds a computer for the caller.
func (s *Server) handleInvite(w http.ResponseWriter, r *http.Request) {
	var req protocol.InviteRequest
	if !decodeBody(w, r, &req) {
		return
	}
	tok := requestToken(r)
	now := time.Now()
	inv := store.Invite{Kind: store.InviteUser, Role: req.Role, Name: strings.TrimSpace(req.Name), CreatedBy: tok.UserID, ExpiresAt: now.Add(inviteTTL)}
	if req.Self {
		inv = store.Invite{Kind: store.InviteComputer, UserID: tok.UserID, CreatedBy: tok.UserID, ExpiresAt: now.Add(inviteTTL)}
	} else {
		if tok.Role != protocol.RoleAdmin {
			writeError(w, http.StatusForbidden, "", "only an admin can invite users; ask an admin of the server")
			return
		}
		if inv.Role == "" {
			inv.Role = protocol.RoleMember
		}
		if !validRole(inv.Role) {
			writeError(w, http.StatusBadRequest, "", fmt.Sprintf("unknown role %q; use member or admin", inv.Role))
			return
		}
		if p := NameProblem(inv.Name); inv.Name != "" && p != "" {
			writeError(w, http.StatusBadRequest, "", p)
			return
		}
	}
	secret, err := s.store.CreateInvite(r.Context(), inv, now)
	if err != nil {
		s.internalError(w, "create invite", err)
		return
	}
	code, err := invite.Encode(invite.Code{Addresses: s.publicURLs, KeyHash: s.tlsKeyHash, Secret: secret})
	if err != nil {
		s.internalError(w, "encode invite", err)
		return
	}
	out := protocol.InviteResponse{Code: code, ExpiresAt: inv.ExpiresAt}
	if s.tlsKeyHash != nil {
		out.Fingerprint = tlspin.Fingerprint(s.tlsKeyHash)
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) userFor(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	u, err := s.store.UserByName(r.Context(), r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "", fmt.Sprintf("no user %q; \"hostel config users\" lists them", r.PathValue("name")))
		return u, false
	}
	if err != nil {
		s.internalError(w, "read user", err)
		return u, false
	}
	return u, true
}

func (s *Server) handleSetUser(w http.ResponseWriter, r *http.Request) {
	var req protocol.SetUserRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if !validRole(req.Role) {
		writeError(w, http.StatusBadRequest, "", fmt.Sprintf("unknown role %q; use member or admin", req.Role))
		return
	}
	u, ok := s.userFor(w, r)
	if !ok {
		return
	}
	err := s.store.SetUserRole(r.Context(), u.ID, req.Role)
	if errors.Is(err, store.ErrLastAdmin) {
		writeError(w, http.StatusConflict, "last_admin", fmt.Sprintf("%s is the last admin; make another user admin first", u.Name))
		return
	}
	if err != nil {
		s.internalError(w, "set role", err)
		return
	}
	tok := requestToken(r)
	s.store.AddEventBy(r.Context(), time.Now(), "", 0, "user", fmt.Sprintf("%s made %s %s", tok.Name, u.Name, req.Role), tok.UserID)
	w.WriteHeader(http.StatusNoContent)
}

// handleRemoveUser removes a user: tokens revoked, queued leases cancelled,
// active leases ended (with cleanup).
func (s *Server) handleRemoveUser(w http.ResponseWriter, r *http.Request) {
	u, ok := s.userFor(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	err := s.store.RemoveUser(ctx, u.ID, time.Now())
	if errors.Is(err, store.ErrLastAdmin) {
		writeError(w, http.StatusConflict, "last_admin", fmt.Sprintf("%s is the last admin; make another user admin first", u.Name))
		return
	}
	if err != nil {
		s.internalError(w, "remove user", err)
		return
	}
	tok := requestToken(r)
	reason := fmt.Sprintf("user %s removed by %s (admin)", u.Name, tok.Name)
	s.store.AddEventBy(ctx, time.Now(), "", 0, "user", reason, tok.UserID)
	// The user is removed already (no token works), so this must not stop at
	// the client's deadline or at the first error.
	if err := s.endUserLeases(u.ID, reason); err != nil {
		s.internalError(w, "end the removed user's leases", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRemoveComputer revokes the tokens of one computer of a user, for a
// computer that cannot disconnect itself (lost, or offline at a replace).
func (s *Server) handleRemoveComputer(w http.ResponseWriter, r *http.Request) {
	u, ok := s.userFor(w, r)
	if !ok {
		return
	}
	computer := r.PathValue("computer")
	err := s.store.RevokeComputer(r.Context(), u.ID, computer, time.Now())
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "", fmt.Sprintf("%s has no computer %q; \"hostel config users\" lists them", u.Name, computer))
		return
	}
	if err != nil {
		s.internalError(w, "remove computer", err)
		return
	}
	tok := requestToken(r)
	s.store.AddEventBy(r.Context(), time.Now(), "", 0, "user", fmt.Sprintf("computer %s of %s removed by %s (admin)", computer, u.Name, tok.Name), tok.UserID)
	w.WriteHeader(http.StatusNoContent)
}

// endUserLeaseTimeout bounds the end of a removed user's leases.
const endUserLeaseTimeout = time.Minute

func (s *Server) endUserLeases(userID int64, reason string) error {
	ctx, cancel := context.WithTimeout(context.Background(), endUserLeaseTimeout)
	defer cancel()
	ids, err := s.store.UserTokenIDs(ctx, userID)
	if err != nil {
		return err
	}
	leases, err := s.store.ActiveLeases(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, l := range leases {
		if slices.Contains(ids, l.TokenID) {
			if err := s.endLease(ctx, l.ID, reason); err != nil {
				s.log.Error("end a removed user's lease", "lease_id", l.ID, "err", err)
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (s *Server) handleMove(w http.ResponseWriter, r *http.Request) {
	var req protocol.MoveRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.First == (req.Before != "") {
		writeError(w, http.StatusBadRequest, "", "give first or before (a lease ID)")
		return
	}
	l, ok := s.leaseFor(w, r)
	if !ok {
		return
	}
	if req.Before == l.ID {
		writeError(w, http.StatusBadRequest, "", "a lease cannot move before itself")
		return
	}
	moved, err := s.store.PlaceLease(r.Context(), l.ID, req.Before, time.Now())
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "", err.Error())
		return
	}
	if err != nil {
		s.internalError(w, "move lease", err)
		return
	}
	if !moved {
		writeError(w, http.StatusConflict, "not_queued", fmt.Sprintf("lease %s is not queued", l.ID))
		return
	}
	tok := requestToken(r)
	where := "to the front of the queue"
	if req.Before != "" {
		where = "before lease " + req.Before
	}
	s.store.AddEventBy(r.Context(), time.Now(), l.ID, 0, "queue", fmt.Sprintf("moved %s by %s (admin)", where, tok.Name), tok.UserID)
	s.kickScheduler()
	s.writeLease(w, r, l.ID)
}

func (s *Server) handlePriority(w http.ResponseWriter, r *http.Request) {
	var req protocol.PriorityRequest
	if !decodeBody(w, r, &req) {
		return
	}
	prio, ok := protocol.PriorityNames[req.Priority]
	if !ok {
		writeError(w, http.StatusBadRequest, "", fmt.Sprintf("unknown priority %q; use low, normal, or high", req.Priority))
		return
	}
	l, ok := s.leaseFor(w, r)
	if !ok {
		return
	}
	changed, err := s.store.SetPriority(r.Context(), l.ID, prio)
	if err != nil {
		s.internalError(w, "set priority", err)
		return
	}
	if !changed {
		writeError(w, http.StatusConflict, "not_queued", fmt.Sprintf("lease %s is not queued", l.ID))
		return
	}
	tok := requestToken(r)
	s.store.AddEventBy(r.Context(), time.Now(), l.ID, 0, "queue", fmt.Sprintf("priority set to %s by %s (admin)", req.Priority, tok.Name), tok.UserID)
	s.kickScheduler()
	s.writeLease(w, r, l.ID)
}

func (s *Server) writeLease(w http.ResponseWriter, r *http.Request, id string) {
	l, err := s.store.Lease(r.Context(), id)
	if err != nil {
		s.internalError(w, "read lease", err)
		return
	}
	writeJSON(w, http.StatusOK, s.apiLease(r, l))
}
