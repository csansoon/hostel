package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/provider"
	"github.com/csansoon/hostel/internal/roomops"
	"github.com/csansoon/hostel/internal/store"
)

// liveConfig is one consistent view of the configuration: the server swaps it
// as a whole when a new revision applies (multiplayer.md, section 7).
type liveConfig struct {
	cfg       *config.Server
	revision  int
	document  string
	providers map[string]provider.Provider
	// tools are the files of the defined tools (name → file → content), and
	// toolsDir has them on disk.
	tools    map[string]map[string]string
	toolsDir string
	// uploads has the tool files and hashes for each tool and OS.
	uploads map[string]map[string]roomops.Tool
	// warning is a problem of the stored configuration that an admin must
	// fix (GET /v1/status); a new configuration clears it.
	warning string
}

func (s *Server) config() *config.Server { return s.live.Load().cfg }

func (s *Server) providerMap() map[string]provider.Provider { return s.live.Load().providers }

func (s *Server) toolUploads() map[string]map[string]roomops.Tool {
	return s.live.Load().uploads
}

// buildLive validates a configuration and builds its providers and uploads.
// Providers whose settings did not change are reused (no login check again).
func (s *Server) buildLive(ctx context.Context, cfg *config.Server, revision int, document string, tools map[string]map[string]string, toolsDir string, old *liveConfig) (*liveConfig, error) {
	lc := &liveConfig{cfg: cfg, revision: revision, document: document, tools: tools, toolsDir: toolsDir, providers: map[string]provider.Provider{}}
	fresh := &config.Server{ProviderOrder: nil, Providers: map[string]*config.Provider{}}
	for _, name := range cfg.ProviderOrder {
		if old != nil && old.providers[name] != nil && reflect.DeepEqual(old.cfg.Providers[name], cfg.Providers[name]) {
			lc.providers[name] = old.providers[name]
			continue
		}
		fresh.ProviderOrder = append(fresh.ProviderOrder, name)
		fresh.Providers[name] = cfg.Providers[name]
	}
	built, err := newProviders(ctx, fresh, s.serverKey, s.rooms.Dial, s.log)
	if err != nil {
		return nil, err
	}
	for name, p := range built {
		lc.providers[name] = p
	}
	uploads, err := toolUploads(cfg)
	if err != nil {
		return nil, err
	}
	lc.uploads = uploads
	return lc, nil
}

// loadConfig reads server.toml and the latest document. At the first start of
// M9, it imports the document keys of server.toml (revision 1) and writes
// server.toml again with the local keys only. The steps can repeat after a
// crash: the copy of the old file first, then the revision, then the new
// file; a start that finds revision 1 of the import and the old file finishes
// the import. It returns the local part, which stays until a restart.
func loadConfig(ctx context.Context, file string, st *store.Store, now time.Time) (*startConfig, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	paths, err := config.ReadPaths(file)
	if err != nil {
		return nil, err
	}
	has, err := config.HasDocumentKeys(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	latest, err := st.LatestConfig(ctx)
	none := errors.Is(err, store.ErrNotFound)
	if err != nil && !none {
		return nil, err
	}
	if has {
		local, doc, err := config.SplitServerFile(data)
		if err != nil {
			return nil, err
		}
		finish := latest.Revision == 1 && latest.UserID == 0 && latest.Document == string(doc)
		if none {
			if err := writeOnce(file+".pre-m9", data); err != nil {
				return nil, err
			}
			if _, err := config.LoadServer(file); err != nil {
				return nil, err
			}
			if _, err := st.AddConfig(ctx, 0, string(doc), 0, now); err != nil {
				return nil, err
			}
			latest = store.ConfigRevision{Revision: 1, Document: string(doc)}
			finish = true
		}
		// finish: the import (or the end of an import that stopped before
		// server.toml was written again). Otherwise the document keys in
		// server.toml are an error (LoadServerDocument).
		if finish {
			data = append([]byte(importNote), local...)
			if err := os.WriteFile(file+".tmp", data, 0o644); err != nil {
				return nil, err
			}
			if err := os.Rename(file+".tmp", file); err != nil {
				return nil, err
			}
		}
	}
	latestTools, err := st.LatestTools(ctx)
	if err != nil {
		return nil, err
	}
	tools := effectiveTools(latestTools)
	dir, err := writeTools(paths.DataDir, tools)
	if err != nil {
		return nil, fmt.Errorf("write the tools: %w", err)
	}
	removeOldToolDirs(paths.DataDir, dir)
	document, warning := latest.Document, ""
	if oldErr := config.OldForm([]byte(document)); oldErr != nil {
		// No compatibility with the configuration before M10e (owner
		// decision, 2026-11-05), but the server stays reachable: it starts
		// without providers and machines, and an admin writes the new form.
		warning = fmt.Sprintf("the stored configuration (revision %d) has the form before M10e, so the server runs without providers and machines; write the configuration again in the new form with \"hostel config edit\" (machines list tools): %v",
			latest.Revision, strings.ReplaceAll(oldErr.Error(), "\n", "; "))
		document = ""
	}
	cfg, err := config.LoadServerDocument(file, data, config.Document{Revision: latest.Revision, Data: []byte(document), Tools: tools, ToolsDir: dir})
	if err != nil {
		return nil, err
	}
	return &startConfig{cfg: cfg, revision: latest.Revision, document: document, tools: tools, toolsDir: dir, local: data, warning: warning}, nil
}

// startConfig is the configuration at the start.
type startConfig struct {
	cfg      *config.Server
	revision int
	document string
	tools    map[string]map[string]string
	toolsDir string
	// local is server.toml: its keys change only with a restart.
	local   []byte
	warning string
}

// writeOnce writes a file unless it exists.
func writeOnce(file string, data []byte) error {
	if _, err := os.Stat(file); err == nil {
		return nil
	}
	return os.WriteFile(file, data, 0o600)
}

const importNote = `# Since M9, the providers, machines, and settings are in the server's
# database: see them with "hostel config" and change them with
# "hostel config edit". server.toml.pre-m9 is the file before the import.
`

// errConfig is a configuration change that the server refuses; the message
// is for the admin.
type errConfig struct{ msg string }

func (e errConfig) Error() string { return e.msg }

// applyConfig validates a new document, saves it as the revision after base,
// and applies it at once.
func (s *Server) applyConfig(ctx context.Context, base int, document string, userID int64) (int, error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	old := s.live.Load()
	if old.revision != base {
		return 0, fmt.Errorf("%w: revision %d is the latest, not %d", store.ErrConflict, old.revision, base)
	}
	cfg, err := config.LoadServerDocument(old.cfg.File, s.localData, config.Document{Revision: base + 1, Data: []byte(document), Tools: old.tools, ToolsDir: old.toolsDir})
	if err != nil {
		return 0, errConfig{err.Error()}
	}
	if err := s.checkRemovals(ctx, cfg); err != nil {
		return 0, err
	}
	lc, err := s.buildLive(ctx, cfg, base+1, document, old.tools, old.toolsDir, old)
	if err != nil {
		return 0, errConfig{err.Error()}
	}
	// The scheduler must not assign a lease to a removed machine between the
	// check and the swap.
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	if err := s.checkRemovals(ctx, cfg); err != nil {
		return 0, err
	}
	rev, err := s.store.AddConfig(ctx, base, document, userID, time.Now())
	if err != nil {
		return 0, err
	}
	s.live.Store(lc)
	// The room tokens of a removed machine stop working.
	for _, m := range old.cfg.Machines() {
		if m.Connect && cfg.Machine(m.Key()) == nil {
			if err := s.revokeMachineTokens(ctx, m.Key(), time.Now()); err != nil {
				s.log.Error("revoke the room tokens of a removed machine", "machine", m.Key(), "err", err)
			}
		}
	}
	// The loop adds the rooms of new static machines (refreshRooms).
	s.kickScheduler()
	return rev, nil
}

// applyTool saves a tool change (new files, removed, or back to the default)
// and applies it at once. The whole configuration is validated with it: a
// tool that a machine lists cannot be removed.
func (s *Server) applyTool(ctx context.Context, name, state string, files map[string]string, userID int64) (int, error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	old := s.live.Load()
	latest, err := s.store.LatestTools(ctx)
	if err != nil {
		return 0, err
	}
	latest[name] = store.ToolRevision{Tool: name, State: state, Files: files}
	tools := effectiveTools(latest)
	dir, err := writeTools(old.cfg.DataDir, tools)
	if err != nil {
		return 0, err
	}
	applied := false
	defer func() {
		// A refused change leaves no directory; an applied one stays until the
		// next start (a room upload can still read the previous one).
		if !applied && dir != old.toolsDir {
			os.RemoveAll(dir)
		}
	}()
	cfg, err := config.LoadServerDocument(old.cfg.File, s.localData,
		config.Document{Revision: old.revision, Data: []byte(old.document), Tools: tools, ToolsDir: dir})
	if err != nil {
		return 0, errConfig{err.Error()}
	}
	lc, err := s.buildLive(ctx, cfg, old.revision, old.document, tools, dir, old)
	if err != nil {
		return 0, errConfig{err.Error()}
	}
	lc.warning = old.warning
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	if err := s.checkActiveTools(ctx, cfg); err != nil {
		return 0, err
	}
	// Cleanup runs the stop script of the version that started the lease.
	leases, err := s.store.ActiveLeases(ctx)
	if err != nil {
		return 0, err
	}
	for _, l := range leases {
		if !leaseUses(l, name) {
			continue
		}
		if l.State != protocol.LeaseQueued {
			return 0, errConfig{fmt.Sprintf("the lease %s (%s) uses the tool %s; change the tool when its leases have ended", l.ID, l.State, name)}
		}
		// A queued lease was checked against the old version: its options
		// must fit the new one.
		if _, _, msg := checkLeaseTools(cfg, l.Tools); msg != "" {
			return 0, errConfig{fmt.Sprintf("the queued lease %s does not fit the new version of %s (%s); change the tool when the lease has ended, or release it", l.ID, name, msg)}
		}
	}
	rev, err := s.store.AddToolRevision(ctx, name, state, files, userID, time.Now())
	if err != nil {
		return 0, err
	}
	s.live.Store(lc)
	applied = true
	s.kickScheduler()
	return rev, nil
}

// checkActiveTools refuses a configuration without a tool of a lease that
// has not ended (the lease steps need the tool).
func (s *Server) checkActiveTools(ctx context.Context, cfg *config.Server) error {
	leases, err := s.store.ActiveLeases(ctx)
	if err != nil {
		return err
	}
	for _, l := range leases {
		for _, name := range toolNamesOf(l.Tools) {
			if cfg.Tools[name] == nil && !listedBy(cfg, name) {
				return errConfig{fmt.Sprintf("the lease %s (%s) uses the tool %s; remove the tool when its leases have ended", l.ID, l.State, name)}
			}
		}
	}
	return nil
}

// checkRemovals refuses a configuration that removes a machine whose room has
// an active lease, or a provider that still has rooms of its own.
func (s *Server) checkRemovals(ctx context.Context, cfg *config.Server) error {
	if err := s.checkActiveTools(ctx, cfg); err != nil {
		return err
	}
	rooms, err := s.store.Rooms(ctx)
	if err != nil {
		return err
	}
	leases, err := s.store.ActiveLeases(ctx)
	if err != nil {
		return err
	}
	var errs []string
	for _, r := range rooms {
		if r.State == protocol.RoomRetired || r.State == protocol.RoomDestroyed {
			continue
		}
		key := r.Provider + "/" + r.Machine
		// A room keeps its provider's reference: the provider's kind must not
		// change under it.
		if op, np := s.config().Providers[r.Provider], cfg.Providers[r.Provider]; op != nil && np != nil &&
			(op.Kind != np.Kind || !slices.Equal(op.Command, np.Command) || !maps.Equal(op.CommandEnv, np.CommandEnv)) {
			errs = append(errs, fmt.Sprintf("providers.%s: kind, command, and command_env stay while the provider has the room %s (its rooms belong to that backend); remove its machines, and change them when its rooms are destroyed or retired", r.Provider, r.Name))
			continue
		}
		if p := cfg.Providers[r.Provider]; p == nil && s.config().Providers[r.Provider] != nil && s.config().Providers[r.Provider].Kind != config.ProviderStatic {
			errs = append(errs, fmt.Sprintf("providers.%s: has the room %s; remove its machines first, and remove the provider when its rooms are destroyed", r.Provider, r.Name))
			continue
		}
		if nm, om := cfg.Machine(key), s.config().Machine(key); nm != nil && om != nil && nm.OnDemand() &&
			fmt.Sprint(om.Spec["product"]) != fmt.Sprint(nm.Spec["product"]) {
			// The room's reference belongs to its product.
			errs = append(errs, fmt.Sprintf("providers.%s.machines.%s.spec.product: stays while the machine has the room %s; remove the machine, and add it again with the new product when its rooms are destroyed", r.Provider, r.Machine, r.Name))
			continue
		}
		if nm := cfg.Machine(key); nm != nil {
			// Cleanup runs as the slot's user: the slots of a room with a
			// lease must not change under it.
			// The server reaches the room of a lease at the same place.
			if om := s.config().Machine(key); om != nil {
				for _, l := range leases {
					changed := !reflect.DeepEqual(om.Slots, nm.Slots) || om.Address != nm.Address || om.Connect != nm.Connect ||
						om.OS != nm.OS || slices.ContainsFunc(toolNamesOf(l.Tools), func(t string) bool { return nm.Tool(t) == nil })
					if changed && l.RoomID == r.ID && l.State != protocol.LeaseQueued {
						errs = append(errs, fmt.Sprintf("providers.%s.machines.%s: slots, address, connect, os, and the lease's tools stay while the room %s has the active lease %s; drain the room (hostel config rooms drain %s) and change them when the lease ends", r.Provider, r.Machine, r.Name, l.ID, r.Name))
						break
					}
				}
			}
			continue
		}
		// The provider destroys a room through its machine's settings (for
		// Namespace, the product): an on-demand machine stays until its rooms
		// are destroyed.
		if om := s.config().Machine(key); om != nil && om.OnDemand() {
			errs = append(errs, fmt.Sprintf("providers.%s.machines.%s: the machine has the room %s (%s); destroy it first (hostel config rooms destroy %s; drain it if a lease uses it), then remove the machine", r.Provider, r.Machine, r.Name, r.State, r.Name))
			continue
		}
		for _, l := range leases {
			if l.RoomID == r.ID && l.State != protocol.LeaseQueued {
				errs = append(errs, fmt.Sprintf("providers.%s.machines.%s: the room %s has the active lease %s; drain the room (hostel config rooms drain %s) and remove the machine when the lease ends", r.Provider, r.Machine, r.Name, l.ID, r.Name))
			}
		}
	}
	if len(errs) > 0 {
		return errConfig{strings.Join(errs, "\n")}
	}
	return nil
}

// machineHash is the hash of what a room of the machine is provisioned with:
// the settings that provisioning uses, the machine's tools, and the files of
// its defined tools (for an on-demand room, its software tools only: it
// installs a screen tool again when a lease uses it). A free room with another
// hash provisions again or is destroyed.
func machineHash(lc *liveConfig, m *config.Machine) string {
	hashes := map[string]string{}
	for _, t := range lc.cfg.DefinedTools(m) {
		if m.OnDemand() && t.Screen {
			continue
		}
		hashes[t.Name] = lc.uploads[t.Name][m.OS].Hash
	}
	data, _ := json.Marshal(struct {
		Address, OS string
		Connect     bool
		Spec        map[string]any
		Tools       []string
		Env         map[string]string
		Slots       []config.Slot
		Mise        *config.Mise
		Hashes      map[string]string
	}{m.Address, m.OS, m.Connect, m.Spec, m.Tools, m.Env, m.Slots, lc.cfg.Mise, hashes})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// refreshRooms makes rooms follow the configuration: it adds the rooms of new
// static machines; a free room whose machine changed provisions again
// (static) or is destroyed (on demand); a free room whose machine is gone is
// retired (static) or destroyed (on demand).
func (s *Server) refreshRooms(ctx context.Context) error {
	if err := s.ensureStaticRooms(ctx, false); err != nil {
		return err
	}
	rooms, err := s.store.Rooms(ctx)
	if err != nil {
		return err
	}
	leases, err := s.store.ActiveLeases(ctx)
	if err != nil {
		return err
	}
	busy := map[int64]bool{}
	for _, l := range leases {
		if l.RoomID != 0 {
			busy[l.RoomID] = true
		}
	}
	lc := s.live.Load()
	now := time.Now()
	for _, r := range rooms {
		if busy[r.ID] {
			continue
		}
		m := lc.cfg.Machine(r.Provider + "/" + r.Machine)
		p := lc.cfg.Providers[r.Provider]
		static := p != nil && p.Kind == config.ProviderStatic
		switch {
		case m == nil && (p == nil || static):
			if r.State != protocol.RoomRetired {
				_, err = s.store.SetRoomState(ctx, r.ID, []string{r.State}, protocol.RoomRetired, "machine removed from the configuration", now)
			}
		case m == nil:
			_, err = s.store.SetRoomState(ctx, r.ID, []string{protocol.RoomCreating, protocol.RoomProvisioning,
				protocol.RoomRunning, protocol.RoomResuming, protocol.RoomStopping, protocol.RoomStopped, protocol.RoomBroken},
				protocol.RoomDestroying, "machine removed from the configuration", now)
		case r.ConfigSHA256 == "" && (r.State == protocol.RoomRunning || r.State == protocol.RoomStopped):
			// A room from before M9 (also a stopped one, which can resume):
			// its settings are the current ones at the first pass.
			err = s.store.SetConfigSHA256(ctx, r.ID, machineHash(lc, m))
		case r.State != protocol.RoomRunning:
		case r.ConfigSHA256 != machineHash(lc, m) && static:
			if err = s.store.EnsureSlots(ctx, r.ID, slotNames(m)); err == nil {
				err = s.store.RemoveFreeSlots(ctx, r.ID, slotNames(m))
			}
			if err == nil {
				_, err = s.store.SetRoomState(ctx, r.ID, []string{protocol.RoomRunning}, protocol.RoomProvisioning, "configuration changed", now)
			}
		case r.ConfigSHA256 != machineHash(lc, m):
			_, err = s.store.SetRoomState(ctx, r.ID, []string{protocol.RoomRunning}, protocol.RoomDestroying, "configuration changed", now)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
