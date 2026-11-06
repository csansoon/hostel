package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/invite"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/store"
)

// joinProvider is the static provider of machines that join with a code.
const joinProvider = "home"

// machineInvite is the machine of a machine invite.
type machineInvite struct {
	Provider string   `json:"provider"`
	Name     string   `json:"name"`
	Tools    []string `json:"tools"`
}

func (s *Server) handleMachineInvite(w http.ResponseWriter, r *http.Request) {
	var req protocol.MachineInviteRequest
	if !decodeBody(w, r, &req) {
		return
	}
	m := machineInvite{Provider: req.Provider, Name: req.Name, Tools: req.Tools}
	if m.Provider == "" {
		m.Provider = joinProvider
	}
	cfg := s.config()
	switch p := cfg.Providers[m.Provider]; {
	case !config.ValidName(m.Name) || !config.ValidName(m.Provider):
		writeError(w, http.StatusBadRequest, "", fmt.Sprintf("%q is not a valid machine name: use lowercase letters, digits, and -, starting with a letter", m.Provider+"/"+m.Name))
		return
	case p != nil && p.Kind != config.ProviderStatic:
		writeError(w, http.StatusBadRequest, "", fmt.Sprintf("the provider %s is %s; a machine joins a static provider", m.Provider, p.Kind))
		return
	case cfg.Machine(m.Provider+"/"+m.Name) != nil:
		writeError(w, http.StatusConflict, "", fmt.Sprintf("the machine %s/%s exists already", m.Provider, m.Name))
		return
	}
	if err := cfg.CheckToolList(m.Tools); err != nil {
		writeError(w, http.StatusBadRequest, "", err.Error())
		return
	}
	data, _ := json.Marshal(m)
	now := time.Now()
	inv := store.Invite{Kind: store.InviteMachine, Machine: string(data), CreatedBy: requestToken(r).UserID, ExpiresAt: now.Add(inviteTTL)}
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
	base, curl := s.publicURLs[0], s.curlPin()
	writeJSON(w, http.StatusCreated, protocol.MachineInviteResponse{
		Machine: m.Provider + "/" + m.Name, Code: code, ExpiresAt: inv.ExpiresAt,
		Unix: fmt.Sprintf("curl -fsS %s%s/v1/install.sh | sudo sh -s -- %s", curl, base, code),
		Windows: fmt.Sprintf(`$a = $env:PROCESSOR_ARCHITECTURE.ToLower(); curl.exe -fsS %s-o "$env:TEMP\hostel.exe" "%s/v1/download/hostel-windows-$a.exe"; & "$env:TEMP\hostel.exe" machine join %s`,
			curl, base, code),
	})
}

// curlPin returns the curl options that pin the server's own TLS key, or ""
// behind a proxy with TLS.
func (s *Server) curlPin() string {
	if s.tlsKeyHash == nil {
		return ""
	}
	return "-k --pinnedpubkey sha256//" + base64.StdEncoding.EncodeToString(s.tlsKeyHash) + " "
}

func (s *Server) handleMachineJoin(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	src := s.clientAddr(r)
	if s.connectLimit.blocked(src, now) {
		writeError(w, http.StatusTooManyRequests, "", "too many failed attempts; wait a minute")
		return
	}
	var req protocol.MachineJoinRequest
	if !decodeBody(w, r, &req) {
		return
	}
	id, data, createdBy, err := s.store.MachineInvite(r.Context(), req.Secret, now)
	if errors.Is(err, store.ErrInviteInvalid) {
		s.connectLimit.fail(src, now)
		writeError(w, http.StatusForbidden, "", "the machine code is not valid: it was used, it expired, or it is wrong; ask an admin for a new one (hostel config rooms add)")
		return
	}
	if err != nil {
		s.internalError(w, "read invite", err)
		return
	}
	var m machineInvite
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		s.internalError(w, "read invite", err)
		return
	}
	if !config.IsOSName(req.OS) || (req.Arch != "amd64" && req.Arch != "arm64") {
		writeError(w, http.StatusBadRequest, "", fmt.Sprintf("Hostel rooms run on linux, macos, or windows with amd64 or arm64, not %s/%s", req.OS, req.Arch))
		return
	}
	key := m.Provider + "/" + m.Name
	// One join at a time: the check that the machine is new, the token, and
	// the configuration change belong together.
	s.joinMu.Lock()
	defer s.joinMu.Unlock()
	// The code is used before the configuration changes, so two joins with
	// one code cannot both add the machine; a failed join reopens it.
	if err := s.store.UseMachineInvite(r.Context(), id, now); errors.Is(err, store.ErrInviteInvalid) {
		writeError(w, http.StatusForbidden, "", "the machine code is not valid: it was used, it expired, or it is wrong; ask an admin for a new one (hostel config rooms add)")
		return
	} else if err != nil {
		s.internalError(w, "use invite", err)
		return
	}
	lc := s.live.Load()
	var doc []byte
	var token string
	if lc.cfg.Machine(key) != nil {
		err = errConfig{fmt.Sprintf("the machine %s exists already; remove it first (hostel config rooms remove %s), or ask for a code with another name", key, key)}
	} else {
		doc, err = addJoinedMachine([]byte(lc.document), lc.cfg, m, req.OS)
	}
	// The token is made before the configuration changes: an added machine
	// always has its token. Old tokens of the name stop working, and a retired
	// room of the name forgets its host key (the code proves the new host).
	if err == nil {
		err = s.revokeMachineTokens(r.Context(), key, now)
	}
	if err == nil {
		err = s.store.ResetRetiredRoom(r.Context(), m.Provider, m.Name)
	}
	var tok store.Token
	if err == nil {
		tok, token, err = s.store.CreateRoomToken(r.Context(), key, key, now)
	}
	if err == nil {
		_, err = s.applyConfig(r.Context(), lc.revision, string(doc), createdBy)
	}
	if err != nil {
		if rerr := s.store.ReopenMachineInvite(r.Context(), id, now); rerr != nil {
			s.log.Error("reopen machine invite", "err", rerr)
		}
		if tok.ID != 0 {
			if rerr := s.store.RevokeToken(r.Context(), tok.ID, now); rerr != nil {
				s.log.Error("revoke the token of a failed join", "err", rerr)
			}
		}
	}
	var cfgErr errConfig
	switch {
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, protocol.CodeConflict, "the configuration changed at the same time; run the command again")
		return
	case errors.As(err, &cfgErr):
		// The caller has only the code: the details go to the log.
		s.log.Warn("machine join refused", "machine", key, "err", cfgErr.msg)
		msg := "the server cannot add this machine; the admin sees why in the server log, or with \"hostel config edit\""
		if strings.Contains(cfgErr.msg, "exists already") {
			msg = cfgErr.msg
		}
		writeError(w, http.StatusUnprocessableEntity, "", msg)
		return
	case err != nil:
		s.internalError(w, "add machine", err)
		return
	}
	s.store.AddEventBy(r.Context(), now, "", 0, "config", fmt.Sprintf("machine %s joined (%s/%s, host %q)", key, req.OS, req.Arch, req.Hostname), createdBy)
	writeJSON(w, http.StatusOK, protocol.MachineJoinResponse{ServerID: lc.cfg.ServerID, Machine: key, Token: token,
		ServerPublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.serverKey.PublicKey())))})
}

// revokeMachineTokens revokes the room tokens of a machine and closes their
// connections.
func (s *Server) revokeMachineTokens(ctx context.Context, machine string, now time.Time) error {
	ids, err := s.store.RevokeMachineTokens(ctx, machine, now)
	for _, id := range ids {
		s.rooms.CloseToken(id)
	}
	return err
}

// addJoinedMachine adds a machine that joins (connect = true) to a document,
// and its static provider if the document has none of that name.
func addJoinedMachine(doc []byte, cfg *config.Server, m machineInvite, osName string) ([]byte, error) {
	var err error
	if cfg.Providers[m.Provider] == nil {
		if doc, err = config.SetKey(doc, []string{"providers", m.Provider, "kind"}, `"static"`); err != nil {
			return nil, err
		}
	}
	quote := func(items []string) string {
		q := make([]string, len(items))
		for i, it := range items {
			q[i] = fmt.Sprintf("%q", it)
		}
		return "[" + strings.Join(q, ", ") + "]"
	}
	path := []string{"providers", m.Provider, "machines", m.Name}
	keys := [][2]string{{"address", `"hostel@localhost"`}, {"connect", "true"}, {"os", fmt.Sprintf("%q", osName)}}
	if len(m.Tools) > 0 {
		keys = append(keys, [2]string{"tools", quote(m.Tools)})
	}
	for _, kv := range keys {
		if doc, err = config.SetKey(doc, append(slices.Clone(path), kv[0]), kv[1]); err != nil {
			return nil, err
		}
	}
	return doc, nil
}

var downloadRE = regexp.MustCompile(`^hostel-(linux|darwin|windows)-(amd64|arm64)(\.exe)?$`)

// handleDownload serves the hostel binaries of helper_dir (no token: they are
// the release binaries). "hostel machine join" installs from here.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if m := downloadRE.FindStringSubmatch(name); m == nil || (m[1] == "windows") != (m[3] == ".exe") {
		writeError(w, http.StatusNotFound, "", "no such file; the files are hostel-<linux|darwin|windows>-<amd64|arm64>[.exe]")
		return
	}
	http.ServeFile(w, r, filepath.Join(s.config().HelperDir, name))
}

// handleInstallScript is the script that the Unix command of a machine invite
// runs: it installs hostel from this server and runs "hostel machine join".
func (s *Server) handleInstallScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, `#!/bin/sh
# Installs Hostel from %[1]s and adds this machine as a room
# ("hostel machine join"). Run it as root.
set -eu
case "$(uname -s)" in Linux) os=linux ;; Darwin) os=darwin ;; *) echo "install: unsupported OS $(uname -s)" >&2; exit 1 ;; esac
case "$(uname -m)" in x86_64 | amd64) arch=amd64 ;; arm64 | aarch64) arch=arm64 ;; *) echo "install: unsupported architecture $(uname -m)" >&2; exit 1 ;; esac
tmp=$(mktemp -d)
curl -fsS %[2]s-o "$tmp/hostel" "%[1]s/v1/download/hostel-$os-$arch"
mkdir -p /usr/local/bin
install -m 755 "$tmp/hostel" /usr/local/bin/hostel
rm -rf "$tmp"
exec /usr/local/bin/hostel machine join "$@"
`, s.publicURLs[0], s.curlPin())
}
