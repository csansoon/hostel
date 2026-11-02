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
	"path/filepath"
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
	// uploads has the platform files and hashes for each platform and OS.
	uploads map[string]map[string]roomops.Platform
}

func (s *Server) config() *config.Server { return s.live.Load().cfg }

func (s *Server) providerMap() map[string]provider.Provider { return s.live.Load().providers }

func (s *Server) platformUploads() map[string]map[string]roomops.Platform {
	return s.live.Load().uploads
}

// buildLive validates a configuration and builds its providers and uploads.
// Providers whose settings did not change are reused (no login check again).
func (s *Server) buildLive(ctx context.Context, cfg *config.Server, revision int, document string, old *liveConfig) (*liveConfig, error) {
	lc := &liveConfig{cfg: cfg, revision: revision, document: document, providers: map[string]provider.Provider{}}
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
	uploads, err := platformUploads(cfg)
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
			if err := importPlatforms(ctx, st, paths.PlatformsDir, now); err != nil {
				return nil, fmt.Errorf("import the platforms of %s: %w", paths.PlatformsDir, err)
			}
			imported, err := st.LatestPlatforms(ctx)
			if err != nil {
				return nil, err
			}
			dir, err := writePlatforms(paths.DataDir, effectivePlatforms(imported))
			if err != nil {
				return nil, err
			}
			if _, err := config.LoadServerFile(file, dir); err != nil {
				// The imported platforms come from platforms_dir: name it.
				return nil, errors.New(strings.ReplaceAll(err.Error(), dir, paths.PlatformsDir))
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
	latestPlatforms, err := st.LatestPlatforms(ctx)
	if err != nil {
		return nil, err
	}
	dir, err := writePlatforms(paths.DataDir, effectivePlatforms(latestPlatforms))
	if err != nil {
		return nil, fmt.Errorf("write the platforms: %w", err)
	}
	removeOldPlatformDirs(paths.DataDir, dir)
	cfg, err := config.LoadServerDocument(file, data, config.Document{Revision: latest.Revision, Data: []byte(latest.Document), PlatformsDir: dir})
	if err != nil {
		return nil, err
	}
	return &startConfig{cfg: cfg, revision: latest.Revision, document: latest.Document, local: data}, nil
}

// startConfig is the configuration at the start.
type startConfig struct {
	cfg      *config.Server
	revision int
	document string
	// local is server.toml: its keys change only with a restart.
	local []byte
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
	cfg, err := config.LoadServerDocument(old.cfg.File, s.localData, config.Document{Revision: base + 1, Data: []byte(document), PlatformsDir: old.cfg.PlatformsDir})
	if err != nil {
		return 0, errConfig{platformPaths(err.Error(), old.cfg.PlatformsDir)}
	}
	if err := s.checkRemovals(ctx, cfg); err != nil {
		return 0, err
	}
	lc, err := s.buildLive(ctx, cfg, base+1, document, old)
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

// applyPlatform saves a platform change (new files, removed, or back to the
// default) and applies it at once. The whole configuration is validated with
// it: a platform that a machine lists cannot be removed.
func (s *Server) applyPlatform(ctx context.Context, name, state string, files map[string]string, userID int64) (int, error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	old := s.live.Load()
	latest, err := s.store.LatestPlatforms(ctx)
	if err != nil {
		return 0, err
	}
	latest[name] = store.PlatformRevision{Platform: name, State: state, Files: files}
	dir, err := writePlatforms(old.cfg.DataDir, effectivePlatforms(latest))
	if err != nil {
		return 0, err
	}
	applied := false
	defer func() {
		// A refused change leaves no directory; an applied one stays until the
		// next start (a room upload can still read the previous one).
		if !applied && dir != old.cfg.PlatformsDir {
			os.RemoveAll(dir)
		}
	}()
	cfg, err := config.LoadServerDocument(old.cfg.File, s.localData,
		config.Document{Revision: old.revision, Data: []byte(old.document), PlatformsDir: dir})
	if err != nil {
		return 0, errConfig{platformPaths(err.Error(), dir)}
	}
	lc, err := s.buildLive(ctx, cfg, old.revision, old.document, old)
	if err != nil {
		return 0, errConfig{err.Error()}
	}
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	if err := s.checkLeasePlatforms(ctx, cfg); err != nil {
		return 0, err
	}
	// Cleanup runs the reset script of the version that prepared the lease.
	leases, err := s.store.ActiveLeases(ctx)
	if err != nil {
		return 0, err
	}
	for _, l := range leases {
		if l.Platform != name {
			continue
		}
		if l.State != protocol.LeaseQueued {
			return 0, errConfig{fmt.Sprintf("the lease %s (%s) uses the platform %s; change the platform when its leases have ended", l.ID, l.State, name)}
		}
		// A queued lease was checked against the old version: its devices
		// and labels must fit the new one.
		if np := cfg.Platforms[name]; np != nil {
			for _, d := range l.Devices {
				if np.Kind == config.KindDevice && !slices.Contains(np.Devices, d) {
					return 0, errConfig{fmt.Sprintf("the queued lease %s asks for the device %s, which the new version of %s does not have; change the platform when the lease has ended, or release it", l.ID, d, name)}
				}
			}
			if np.Kind == config.KindDevice && len(l.Devices) > np.MaxDevices {
				return 0, errConfig{fmt.Sprintf("the queued lease %s asks for %d devices, more than max_devices of the new version of %s", l.ID, len(l.Devices), name)}
			}
			for _, label := range np.Requires {
				if !slices.Contains(l.RequiredLabels, label) {
					return 0, errConfig{fmt.Sprintf("the new version of %s requires the label %s, which the queued lease %s does not have; change the platform when the lease has ended, or release it", name, label, l.ID)}
				}
			}
		}
	}
	rev, err := s.store.AddPlatformRevision(ctx, name, state, files, userID, time.Now())
	if err != nil {
		return 0, err
	}
	s.live.Store(lc)
	applied = true
	s.kickScheduler()
	return rev, nil
}

// platformPaths replaces the server's internal platform directory in an error
// with the platform names ("platform ios: ...").
func platformPaths(msg, dir string) string {
	return strings.ReplaceAll(msg, dir+string(filepath.Separator), "platform ")
}

// checkLeasePlatforms refuses a configuration without the platform of a lease
// that has not ended (the lease steps need the platform).
func (s *Server) checkLeasePlatforms(ctx context.Context, cfg *config.Server) error {
	leases, err := s.store.ActiveLeases(ctx)
	if err != nil {
		return err
	}
	for _, l := range leases {
		if cfg.Platforms[l.Platform] == nil {
			return errConfig{fmt.Sprintf("the lease %s (%s) uses the platform %s; remove the platform when its leases have ended", l.ID, l.State, l.Platform)}
		}
	}
	return nil
}

// checkRemovals refuses a configuration that removes a machine whose room has
// an active lease, or a provider that still has rooms of its own.
func (s *Server) checkRemovals(ctx context.Context, cfg *config.Server) error {
	if err := s.checkLeasePlatforms(ctx, cfg); err != nil {
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
						om.OS != nm.OS || (l.Platform != nm.OS && !slices.Contains(nm.Platforms, l.Platform))
					if changed && l.RoomID == r.ID && l.State != protocol.LeaseQueued {
						errs = append(errs, fmt.Sprintf("providers.%s.machines.%s: slots, address, connect, os, and the lease's platform stay while the room %s has the active lease %s; drain the room (hostel config rooms drain %s) and change them when the lease ends", r.Provider, r.Machine, r.Name, l.ID, r.Name))
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
// the settings that provisioning uses, the machine-level tool settings, and
// the platforms (for an on-demand room, its OS platform only: it provisions a
// device platform again when a lease uses it). A free room with another hash
// provisions again or is destroyed.
func machineHash(lc *liveConfig, m *config.Machine) string {
	hashes := map[string]string{}
	for _, name := range append([]string{m.OS}, m.Platforms...) {
		if m.OnDemand() && lc.cfg.Platforms[name].Kind == config.KindDevice {
			continue
		}
		hashes[name] = lc.uploads[name][m.OS].Hash
	}
	data, _ := json.Marshal(struct {
		Address, OS string
		Connect     bool
		Spec        map[string]any
		Platforms   []string
		Tools, Env  map[string]string
		Slots       []config.Slot
		Mise        *config.Mise
		Hashes      map[string]string
	}{m.Address, m.OS, m.Connect, m.Spec, m.Platforms, m.Tools, m.Env, m.Slots, lc.cfg.Mise, hashes})
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
