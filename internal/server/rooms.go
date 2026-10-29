package server

import (
	"context"
	"encoding/json"
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

// miseInstallTimeout bounds the install of the machine-level tools.
const miseInstallTimeout = 20 * time.Minute

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
	return s.connectAs(ctx, r, "")
}

// connectAs connects to a room as user (a slot's own user), or as the
// provider's user if user is empty.
func (s *Server) connectAs(ctx context.Context, r store.Room, user string) (*roomops.Conn, error) {
	p := s.providers[r.Provider]
	if p == nil {
		return nil, fmt.Errorf("provider %q is not in the configuration", r.Provider)
	}
	access, err := p.Access(ctx, provider.RoomRef{Name: r.Name, Machine: r.Machine, ID: r.ProviderRef})
	if err != nil {
		return nil, fmt.Errorf("room access: %w", err)
	}
	if user != "" {
		access.User = user
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

func (s *Server) maxProvisionTimeout() time.Duration {
	var d time.Duration
	for _, p := range s.cfg.Platforms {
		d += p.ScriptTimeout(config.StepProvision)
	}
	return d
}

// doProvision probes a room and uploads what it needs; then it cleans dirty
// slots, runs the provision scripts that are not up to date (static rooms
// only), and the health scripts.
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
	// The helper and mise follow the probe. Platform scripts follow the
	// machine's os: an external machine can declare another OS (the E2E tests
	// run .macos.sh scripts in Linux containers this way).
	if m.Kind == config.ProviderStatic && room.OS != m.OS {
		return fmt.Errorf("the room runs %s, but the configuration says os = %q", room.OS, m.OS)
	}
	if err := s.store.SetRoomProbe(ctx, r.ID, room.OS, room.Arch, room.Home, room.Shell); err != nil {
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
	// Slot users first: the cleanup of a dirty slot runs as its user, with its
	// recorded home and its current helper.
	if err := s.provisionSlotUsers(ctx, r, m); err != nil {
		return err
	}
	if err := s.cleanDirtySlots(ctx, room, r.ID); err != nil {
		return err
	}
	// Machine-level tools, before the platform scripts, so the scripts can use
	// them. A failure is a room provisioning failure. It also runs without
	// tools and without agent-device, because it removes an agent-device
	// command that an earlier configuration installed.
	var resp protocol.RunScriptResponse
	req := protocol.MiseInstallRequest{TimeoutSeconds: int(miseInstallTimeout.Seconds()), AgentDevice: s.cfg.Mise.AgentDeviceEnabled()}
	if err := room.Helper(ctx, []string{"mise-install"}, req, &resp); err != nil {
		return err
	}
	if resp.TimedOut || resp.ExitCode != 0 {
		s.store.AddEvent(ctx, time.Now(), "", r.ID, "mise", fmt.Sprintf("mise install exited with code %d\n%s", resp.ExitCode, resp.OutputTail))
		return fmt.Errorf("machine-level tools: mise install failed (exit code %d); see the events", resp.ExitCode)
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
		// On-demand rooms run a platform provision script when a lease for the
		// platform first uses the room (design, section 7).
		if m.OnDemand() && s.cfg.Platforms[name].Kind == config.KindDevice {
			continue
		}
		if err := s.runProvision(ctx, room, r.ID, m.OS, s.cfg.Platforms[name], p); err != nil {
			return err
		}
		if err := s.store.SetRoomPlatform(ctx, r.ID, name, p.Hash, time.Now()); err != nil {
			return err
		}
	}
	return s.runHealth(ctx, room, r.ID)
}

// provisionSlotUsers prepares the slots that have their own OS user: each has
// its own hostel directory, helper, and machine-level tools (design, section
// 9). It records each user's home directory.
func (s *Server) provisionSlotUsers(ctx context.Context, r store.Room, m *config.Machine) error {
	for _, sl := range m.Slots {
		if sl.User == "" {
			continue
		}
		if err := s.provisionSlotUser(ctx, r, m, sl); err != nil {
			return fmt.Errorf("slot %s (user %s): %w", sl.Name, sl.User, err)
		}
	}
	return nil
}

func (s *Server) provisionSlotUser(ctx context.Context, r store.Room, m *config.Machine, sl config.Slot) error {
	conn, err := s.connectAs(ctx, r, sl.User)
	if err != nil {
		return err
	}
	defer conn.Close()
	room, err := roomops.Probe(ctx, conn)
	if err != nil {
		return err
	}
	if room.OS != r.OS {
		return fmt.Errorf("the user runs %s, the machine user %s", room.OS, r.OS)
	}
	if err := s.store.SetSlotHome(ctx, r.ID, sl.Name, room.Home); err != nil {
		return err
	}
	upload, err := s.roomUpload(room, m)
	if err != nil {
		return err
	}
	if _, err := room.Sync(ctx, upload); err != nil {
		return err
	}
	var resp protocol.RunScriptResponse
	req := protocol.MiseInstallRequest{TimeoutSeconds: int(miseInstallTimeout.Seconds()), AgentDevice: s.cfg.Mise.AgentDeviceEnabled()}
	if err := room.Helper(ctx, []string{"mise-install"}, req, &resp); err != nil {
		return err
	}
	if resp.TimedOut || resp.ExitCode != 0 {
		s.store.AddEvent(ctx, time.Now(), "", r.ID, "mise", fmt.Sprintf("slot %s: mise install exited with code %d\n%s", sl.Name, resp.ExitCode, resp.OutputTail))
		return fmt.Errorf("machine-level tools: mise install failed (exit code %d); see the events", resp.ExitCode)
	}
	return nil
}

// slotUser returns the slot's own OS user, or "" (the machine user).
func slotUser(m *config.Machine, slot string) string {
	if m == nil {
		return ""
	}
	for _, sl := range m.Slots {
		if sl.Name == slot {
			return sl.User
		}
	}
	return ""
}

// slotHome returns the home directory of a slot's user: its own user's, or
// the machine user's.
func (s *Server) slotHome(ctx context.Context, r store.Room, slot string) (string, error) {
	if slotUser(s.cfg.Machine(r.Provider+"/"+r.Machine), slot) == "" {
		return r.Home, nil
	}
	home, err := s.store.SlotHome(ctx, r.ID, slot)
	if err == nil && home == "" {
		err = fmt.Errorf("slot %s: the home directory of its user is not known; the room provisioning records it", slot)
	}
	return home, err
}

// healthPlatforms returns the platforms of a room that have a health script
// and that the room is ready for. An on-demand room is ready for a device
// platform only after the first lease of the platform ran its provision script
// (design, section 7), and a health script can need the result (for example the
// ios golden simulators).
func (s *Server) healthPlatforms(ctx context.Context, roomID int64) ([]string, error) {
	r, err := s.store.Room(ctx, roomID)
	if err != nil {
		return nil, err
	}
	m, err := s.machineOf(r)
	if err != nil {
		return nil, err
	}
	ready, err := s.readyPlatforms(ctx, roomID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range s.roomPlatformNames(m) {
		up := s.uploads[name][m.OS]
		if up.Hash != "" && s.cfg.Platforms[name].Script(m.OS, config.StepHealth) != "" && ready[name] == up.Hash {
			out = append(out, name)
		}
	}
	return out, nil
}

// runHealth runs the health scripts of healthPlatforms.
func (s *Server) runHealth(ctx context.Context, room *roomops.Room, roomID int64) error {
	names, err := s.healthPlatforms(ctx, roomID)
	if err != nil {
		return err
	}
	for _, name := range names {
		p := s.cfg.Platforms[name]
		timeout := p.ScriptTimeout(config.StepHealth)
		resp, err := room.RunScript(ctx, name, config.StepHealth, "", protocol.RunScriptRequest{
			Env: platformEnv(p), TimeoutSeconds: int(timeout / time.Second)})
		if err != nil {
			return err
		}
		if resp.TimedOut || resp.ExitCode != 0 {
			s.store.AddEvent(ctx, time.Now(), "", roomID, "script",
				fmt.Sprintf("%s health exited with code %d\n%s", name, resp.ExitCode, resp.OutputTail))
			return fmt.Errorf("platform %s: health script failed (exit code %d)", name, resp.ExitCode)
		}
	}
	return nil
}

func (s *Server) runProvision(ctx context.Context, room *roomops.Room, roomID int64, osName string, p *config.Platform, up roomops.Platform) error {
	if up.Hash == "" || p.Script(osName, config.StepProvision) == "" {
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
	goos, ext := room.OS, ""
	switch goos {
	case config.OSMacOS:
		goos = "darwin"
	case config.OSWindows:
		ext = ".exe"
	}
	u := roomops.Upload{
		HelperFile: filepath.Join(s.cfg.HelperDir, fmt.Sprintf("hostel-%s-%s%s", goos, room.Arch, ext)),
		MiseFile:   filepath.Join(s.cfg.HelperDir, fmt.Sprintf("mise-%s-%s%s", goos, room.Arch, ext)),
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
	for k, v := range s.miseEnv(room.OS, room.Home) {
		env[k] = v
	}
	re := protocol.RoomEnv{Env: env, Slots: map[string]map[string]string{}, OS: m.OS}
	for _, sl := range m.Slots {
		re.Slots[sl.Name] = sl.Env
	}
	if u.RoomEnv, err = json.Marshal(re); err != nil {
		return u, err
	}
	for _, name := range s.roomPlatformNames(m) {
		u.Platforms[name] = s.uploads[name][m.OS]
	}
	return u, nil
}

// miseEnv returns the MISE_* variables of all room-side processes (design,
// section 9, "mise on rooms").
func (s *Server) miseEnv(osName, home string) map[string]string {
	env := map[string]string{
		"MISE_DATA_DIR":             roomops.Path(osName, home, "cache", "mise"),
		"MISE_CACHE_DIR":            roomops.Path(osName, home, "cache", "mise-cache"),
		"MISE_CONFIG_DIR":           roomops.Path(osName, home, "mise"),
		"MISE_STATE_DIR":            roomops.Path(osName, home, "mise-state"),
		"MISE_TRUSTED_CONFIG_PATHS": roomops.Path(osName, home, "slots"),
		"MISE_YES":                  "1",
		"MISE_IDIOMATIC_VERSION_FILE_ENABLE_TOOLS": strings.Join(s.cfg.Mise.IdiomaticTools, ","),
	}
	if s.cfg.Mise.GithubToken != "" {
		env["MISE_GITHUB_TOKEN"] = s.cfg.Mise.GithubToken
	}
	return env
}
