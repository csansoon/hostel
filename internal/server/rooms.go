package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/provider"
	"github.com/csansoon/hostel/internal/roomops"
	"github.com/csansoon/hostel/internal/store"
)

// probeTimeout bounds the probe and the upload of one room.
const probeTimeout = 10 * time.Minute

func slotNames(m *config.Machine) []string {
	names := make([]string, len(m.Slots))
	for i, sl := range m.Slots {
		names[i] = sl.Name
	}
	return names
}

// ensureStaticRooms gives each static machine a room row and marks rooms of
// machines that left the configuration as retired. At start, each static room is
// probed again (state provisioning), except rooms that only the owner can repair.
func (s *Server) ensureStaticRooms(ctx context.Context) error {
	rooms, err := s.store.Rooms(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	byMachine := map[string]store.Room{}
	for _, r := range rooms {
		if p := s.cfg.Providers[r.Provider]; (p != nil && p.Kind == config.ProviderStatic) || r.State == protocol.RoomRetired {
			byMachine[r.Provider+"/"+r.Machine] = r
		}
	}
	for _, m := range s.cfg.Machines() {
		if m.Kind != config.ProviderStatic {
			continue
		}
		r, ok := byMachine[m.Key()]
		delete(byMachine, m.Key())
		if !ok {
			if _, err := s.store.InsertRoom(ctx, store.Room{Name: m.Key(), Provider: m.Provider, Machine: m.Name,
				State: protocol.RoomProvisioning, OS: m.OS}, s.cfg.ServerID, slotNames(m), now); err != nil {
				return err
			}
			continue
		}
		if err := s.store.EnsureSlots(ctx, r.ID, slotNames(m)); err != nil {
			return err
		}
		probeAgain := []string{protocol.RoomRunning, protocol.RoomRetired}
		if r.State == protocol.RoomBroken && r.BrokenReason == reasonUnreachable {
			probeAgain = append(probeAgain, protocol.RoomBroken)
		}
		if _, err := s.store.SetRoomState(ctx, r.ID, probeAgain, protocol.RoomProvisioning, "", now); err != nil {
			return err
		}
	}
	for _, r := range byMachine {
		if r.State != protocol.RoomRetired {
			if _, err := s.store.SetRoomState(ctx, r.ID, []string{r.State}, protocol.RoomRetired, "machine removed from the configuration", now); err != nil {
				return err
			}
		}
	}
	return nil
}

const reasonUnreachable = "unreachable"

// connect opens an SSH connection to a room and pins its host key at the first
// connection.
func (s *Server) connect(ctx context.Context, r store.Room) (*roomops.Conn, error) {
	p := s.providers[r.Provider]
	if p == nil {
		return nil, fmt.Errorf("provider %q is not in the configuration", r.Provider)
	}
	access, err := p.Access(ctx, provider.RoomRef{Name: r.Name, Machine: r.Machine, ID: r.ProviderRef})
	if err != nil {
		return nil, fmt.Errorf("room access: %w", err)
	}
	conn, err := roomops.Connect(ctx, access, r.SSHHostKey)
	if err != nil {
		return nil, err
	}
	if r.SSHHostKey == "" {
		if err := s.store.PinHostKey(ctx, r.ID, conn.HostKey); err != nil {
			conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

// provisionRoom probes a room, uploads the helper, mise, and the platform
// scripts, runs the provision script of each platform that is not up to date,
// and sets the room to running. Each step is safe to repeat.
func (s *Server) provisionRoom(ctx context.Context, id int64) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout+s.maxProvisionTimeout())
	defer cancel()
	r, err := s.store.Room(ctx, id)
	if err != nil || r.State != protocol.RoomProvisioning {
		return
	}
	m := s.cfg.Machine(r.Provider + "/" + r.Machine)
	if m == nil {
		return
	}
	if err := s.doProvision(ctx, r, m); err != nil {
		reason := err.Error()
		switch {
		case errors.Is(err, roomops.ErrUnreachable):
			reason = reasonUnreachable
		case errors.Is(err, roomops.ErrHostKeyChanged):
			reason = "host key changed"
		}
		s.log.Warn("room provisioning failed", "room_id", id, "err", err)
		s.store.SetRoomState(ctx, id, []string{protocol.RoomProvisioning}, protocol.RoomBroken, reason, time.Now())
		return
	}
	if _, err := s.store.SetRoomState(ctx, id, []string{protocol.RoomProvisioning}, protocol.RoomRunning, "", time.Now()); err != nil {
		s.log.Error("set room running", "room_id", id, "err", err)
	}
	s.kickScheduler()
}

func (s *Server) maxProvisionTimeout() time.Duration {
	var d time.Duration
	for _, p := range s.cfg.Platforms {
		d += p.ScriptTimeout(config.StepProvision)
	}
	return d
}

func (s *Server) doProvision(ctx context.Context, r store.Room, m *config.Machine) error {
	conn, err := s.connect(ctx, r)
	if err != nil {
		return err
	}
	defer conn.Close()
	room, err := roomops.Probe(ctx, conn)
	if err != nil {
		return err
	}
	if room.OS != m.OS {
		return fmt.Errorf("the room runs %s, but the configuration says os = %q", room.OS, m.OS)
	}
	if err := s.store.SetRoomProbe(ctx, r.ID, room.OS, room.Arch, room.Home); err != nil {
		return err
	}
	upload, err := s.roomUpload(room, m)
	if err != nil {
		return err
	}
	helperSum, err := room.Sync(ctx, upload)
	if err != nil {
		return err
	}
	if err := s.store.SetHelperSHA256(ctx, r.ID, helperSum); err != nil {
		return err
	}
	ready, err := s.readyPlatforms(ctx, r.ID)
	if err != nil {
		return err
	}
	for _, name := range s.roomPlatformNames(m) {
		p := upload.Platforms[name]
		if have, ok := ready[name]; ok && have == p.Hash {
			continue
		}
		if err := s.runProvision(ctx, room, r.ID, s.cfg.Platforms[name], p); err != nil {
			return err
		}
		if err := s.store.SetRoomPlatform(ctx, r.ID, name, p.Hash, time.Now()); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) runProvision(ctx context.Context, room *roomops.Room, roomID int64, p *config.Platform, up roomops.Platform) error {
	if up.Hash == "" || p.Script(room.OS, config.StepProvision) == "" {
		return nil
	}
	timeout := p.ScriptTimeout(config.StepProvision)
	sctx, cancel := context.WithTimeout(ctx, timeout+time.Minute)
	defer cancel()
	resp, err := room.RunScript(sctx, p.Name, config.StepProvision, "", protocol.RunScriptRequest{
		Env:            platformEnv(p),
		TimeoutSeconds: int(timeout / time.Second),
	})
	if err != nil {
		return err
	}
	s.store.AddEvent(ctx, time.Now(), "", roomID, "script",
		fmt.Sprintf("%s provision exited with code %d\n%s", p.Name, resp.ExitCode, resp.OutputTail))
	if resp.TimedOut {
		return fmt.Errorf("platform %s: provision script timed out after %s", p.Name, timeout)
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("platform %s: provision script exited with code %d", p.Name, resp.ExitCode)
	}
	return nil
}

func platformEnv(p *config.Platform) map[string]string {
	return map[string]string{"HOSTEL_PLATFORM": p.Name, "HOSTEL_PLATFORM_DEVICES": strings.Join(p.Devices, ",")}
}

// roomPlatformNames returns the OS platform and the device platforms of a machine.
func (s *Server) roomPlatformNames(m *config.Machine) []string {
	return append([]string{m.OS}, m.Platforms...)
}

func (s *Server) readyPlatforms(ctx context.Context, roomID int64) (map[string]string, error) {
	rps, err := s.store.RoomPlatforms(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, rp := range rps {
		if rp.RoomID == roomID {
			out[rp.Platform] = rp.ScriptsSHA256
		}
	}
	return out, nil
}

// roomUpload computes what the room must have: the helper and mise for its OS
// and architecture, the machine-level mise tools, the room environment, and the
// platform directories.
func (s *Server) roomUpload(room *roomops.Room, m *config.Machine) (roomops.Upload, error) {
	goos := room.OS
	if goos == config.OSMacOS {
		goos = "darwin"
	}
	u := roomops.Upload{
		HelperFile: filepath.Join(s.cfg.HelperDir, fmt.Sprintf("hostel-%s-%s", goos, room.Arch)),
		MiseFile:   filepath.Join(s.cfg.HelperDir, fmt.Sprintf("mise-%s-%s", goos, room.Arch)),
		Platforms:  map[string]roomops.Platform{},
	}
	var err error
	if u.MiseConfig, err = toml.Marshal(map[string]any{"tools": m.Tools}); err != nil {
		return u, err
	}
	env := map[string]string{}
	for k, v := range m.Env {
		env[k] = v
	}
	for k, v := range s.miseEnv(room.Home) {
		env[k] = v
	}
	re := protocol.RoomEnv{Env: env, Slots: map[string]map[string]string{}}
	for _, sl := range m.Slots {
		re.Slots[sl.Name] = sl.Env
	}
	if u.RoomEnv, err = json.Marshal(re); err != nil {
		return u, err
	}
	for _, name := range s.roomPlatformNames(m) {
		u.Platforms[name] = s.uploads[name][room.OS]
	}
	return u, nil
}

// miseEnv returns the MISE_* variables of all room-side processes (design,
// section 9, "mise on rooms").
func (s *Server) miseEnv(home string) map[string]string {
	root := home + "/hostel"
	env := map[string]string{
		"MISE_DATA_DIR":             root + "/cache/mise",
		"MISE_CACHE_DIR":            root + "/cache/mise-cache",
		"MISE_CONFIG_DIR":           root + "/mise",
		"MISE_STATE_DIR":            root + "/mise-state",
		"MISE_TRUSTED_CONFIG_PATHS": root + "/slots",
		"MISE_YES":                  "1",
		"MISE_IDIOMATIC_VERSION_FILE_ENABLE_TOOLS": strings.Join(s.cfg.Mise.IdiomaticTools, ","),
	}
	if s.cfg.Mise.GithubToken != "" {
		env["MISE_GITHUB_TOKEN"] = s.cfg.Mise.GithubToken
	}
	return env
}
