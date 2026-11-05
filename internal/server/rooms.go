package server

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
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

// ensureStaticRooms gives each static machine a room row. At start, it marks
// rooms of machines that left the configuration as retired, and each static
// room is probed again (state provisioning), except rooms that only the owner
// can repair. After a configuration change (start false), it only adds rooms
// and probes a retired room whose machine is back; refreshRooms does the rest
// when the rooms are free.
func (s *Server) ensureStaticRooms(ctx context.Context, start bool) error {
	rooms, err := s.store.Rooms(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	byMachine := map[string]store.Room{}
	for _, r := range rooms {
		if p := s.config().Providers[r.Provider]; (p != nil && p.Kind == config.ProviderStatic) || r.State == protocol.RoomRetired {
			byMachine[r.Provider+"/"+r.Machine] = r
		}
	}
	for _, m := range s.config().Machines() {
		if m.Kind != config.ProviderStatic {
			continue
		}
		r, ok := byMachine[m.Key()]
		delete(byMachine, m.Key())
		if !ok {
			if _, err := s.store.InsertRoom(ctx, store.Room{Name: m.Key(), Provider: m.Provider, Machine: m.Name,
				State: protocol.RoomProvisioning, OS: m.OS}, s.config().ServerID, slotNames(m), now); err != nil {
				return err
			}
			continue
		}
		if !start && r.State != protocol.RoomRetired {
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
		if start && r.State != protocol.RoomRetired {
			if _, err := s.store.SetRoomState(ctx, r.ID, []string{r.State}, protocol.RoomRetired, "machine removed from the configuration", now); err != nil {
				return err
			}
		}
	}
	return nil
}

const reasonUnreachable = "unreachable"

// reasonHostKeyChanged: the room answered with another SSH host key; an
// admin repairs it and accepts the new key.
const reasonHostKeyChanged = "host key changed"

// connect opens an SSH connection to a room and pins its host key at the first
// connection.
func (s *Server) connect(ctx context.Context, r store.Room) (*roomops.Conn, error) {
	return s.connectAs(ctx, r, "")
}

// connectAs connects to a room as user (a slot's own user), or as the
// provider's user if user is empty.
func (s *Server) connectAs(ctx context.Context, r store.Room, user string) (*roomops.Conn, error) {
	p := s.providerMap()[r.Provider]
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
	for _, t := range s.config().Tools {
		d += t.ScriptTimeout(config.StepInstall) + t.ScriptTimeout(config.StepCheck)
	}
	return d
}

// doProvision probes a room and uploads what it needs; then it cleans dirty
// slots, installs the mise tools, runs the install and check scripts of the
// defined tools that are not up to date (on-demand rooms: software tools
// only), and the check scripts of the installed tools.
func (s *Server) doProvision(ctx context.Context, r store.Room, m *config.Machine) error {
	// One configuration for the whole step: the scripts that run and the
	// hashes that the room records are of one version (the next provisioning
	// follows a newer configuration).
	lc := s.live.Load()
	conn, err := s.connect(ctx, r)
	if err != nil {
		return err
	}
	defer conn.Close()
	room, err := roomops.Probe(ctx, conn)
	if err != nil {
		return err
	}
	// The helper and mise follow the probe. Tool scripts follow the
	// machine's os: an external machine can declare another OS (the E2E tests
	// run .macos.sh scripts in Linux containers this way).
	if m.Kind == config.ProviderStatic && room.OS != m.OS {
		return fmt.Errorf("the room runs %s, but the configuration says os = %q", room.OS, m.OS)
	}
	if err := s.store.SetRoomProbe(ctx, r.ID, room.OS, room.Arch, room.Home, room.Shell); err != nil {
		return err
	}
	upload, err := s.roomUpload(lc, room, m)
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
	// Machine-level mise tools, before the tool scripts, so the scripts can use
	// them. A failure is a room provisioning failure. It also runs without
	// tools and without agent-device, because it removes an agent-device
	// command that an earlier configuration installed.
	var resp protocol.RunScriptResponse
	req := protocol.MiseInstallRequest{TimeoutSeconds: int(miseInstallTimeout.Seconds()), AgentDevice: s.config().Mise.AgentDeviceEnabled()}
	if err := room.Helper(ctx, []string{"mise-install"}, req, &resp); err != nil {
		return err
	}
	if resp.TimedOut || resp.ExitCode != 0 {
		s.store.AddEvent(ctx, time.Now(), "", r.ID, "mise", fmt.Sprintf("mise install exited with code %d\n%s", resp.ExitCode, resp.OutputTail))
		return fmt.Errorf("machine-level tools: mise install failed (exit code %d); see the events", resp.ExitCode)
	}
	ready, err := s.installedTools(ctx, r.ID)
	if err != nil {
		return err
	}
	for _, t := range lc.cfg.DefinedTools(m) {
		up := upload.Tools[t.Name]
		if ready[t.Name] == up.Hash {
			continue
		}
		// On-demand rooms install a screen tool when a lease for it first
		// uses the room (design, section 7).
		if m.OnDemand() && t.Screen {
			continue
		}
		if err := s.installTool(ctx, room, r.ID, m.OS, t, up); err != nil {
			return err
		}
	}
	// The provisioning reached the room: a "last seen" for the presence.
	if err := s.store.SetLastSeen(ctx, r.ID, time.Now()); err != nil {
		s.log.Warn("save the last seen time", "room", r.Name, "err", err)
	}
	// The hardware is for the status view only: an error does not stop the
	// provisioning.
	var hw protocol.Hardware
	if err := room.Helper(ctx, []string{"hardware"}, nil, &hw); err != nil {
		s.log.Warn("read room hardware", "room", r.Name, "err", err)
	} else if data, err := json.Marshal(hw); err == nil {
		if err := s.store.SetHardware(ctx, r.ID, string(data)); err != nil {
			s.log.Warn("save room hardware", "room", r.Name, "err", err)
		}
	}
	return s.runChecks(ctx, room, r.ID)
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
	upload, err := s.roomUpload(s.live.Load(), room, m)
	if err != nil {
		return err
	}
	if _, err := room.Sync(ctx, upload); err != nil {
		return err
	}
	var resp protocol.RunScriptResponse
	req := protocol.MiseInstallRequest{TimeoutSeconds: int(miseInstallTimeout.Seconds()), AgentDevice: s.config().Mise.AgentDeviceEnabled()}
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
	if slotUser(s.config().Machine(r.Provider+"/"+r.Machine), slot) == "" {
		return r.Home, nil
	}
	home, err := s.store.SlotHome(ctx, r.ID, slot)
	if err == nil && home == "" {
		err = fmt.Errorf("slot %s: the home directory of its user is not known; the room provisioning records it", slot)
	}
	return home, err
}

// checkTools returns the defined tools of a room that have a check script and
// that the room has installed with the current files. An on-demand room
// installs a screen tool only when the first lease for it uses the room
// (design, section 7), and a check script can need the result of install (for
// example the ios-simulator golden simulators).
func (s *Server) checkTools(ctx context.Context, roomID int64) ([]string, *config.Machine, error) {
	r, err := s.store.Room(ctx, roomID)
	if err != nil {
		return nil, nil, err
	}
	m, err := s.machineOf(r)
	if err != nil {
		return nil, nil, err
	}
	installed, err := s.installedTools(ctx, roomID)
	if err != nil {
		return nil, nil, err
	}
	var out []string
	for _, t := range s.config().DefinedTools(m) {
		up := s.toolUploads()[t.Name][m.OS]
		if up.Hash != "" && t.Script(m.OS, config.StepCheck) != "" && installed[t.Name] == up.Hash {
			out = append(out, t.Name)
		}
	}
	return out, m, nil
}

// runChecks runs the check scripts of checkTools and records the option that
// each one reports.
func (s *Server) runChecks(ctx context.Context, room *roomops.Room, roomID int64) error {
	names, m, err := s.checkTools(ctx, roomID)
	if err != nil {
		return err
	}
	for _, name := range names {
		t := s.config().Tools[name]
		if t == nil {
			continue
		}
		if err := s.runCheck(ctx, room, roomID, m.OS, t); err != nil {
			return err
		}
	}
	return nil
}

// runCheck runs the check script of a tool and writes its inventory row with
// the option that the last line of the output reports. A tool without a check
// script has no option. osName is the machine's OS, which selects the scripts.
func (s *Server) runCheck(ctx context.Context, room *roomops.Room, roomID int64, osName string, t *config.Tool) error {
	sum := s.toolUploads()[t.Name][osName].Hash
	row := store.RoomTool{RoomID: roomID, Tool: t.Name, ScriptsSHA256: sum, State: store.ToolReady,
		Env: sortedKeys(t.Env), CheckedAt: time.Now()}
	if t.Script(osName, config.StepCheck) != "" {
		timeout := t.ScriptTimeout(config.StepCheck)
		resp, err := room.RunScript(ctx, t.Name, config.StepCheck, "", protocol.RunScriptRequest{TimeoutSeconds: int(timeout / time.Second)})
		if err != nil {
			return err
		}
		if resp.TimedOut || resp.ExitCode != 0 {
			s.store.AddEvent(ctx, time.Now(), "", roomID, "script",
				fmt.Sprintf("%s check exited with code %d\n%s", t.Name, resp.ExitCode, resp.OutputTail))
			row.State = store.ToolFailed
			if err := s.store.SetRoomTool(ctx, row); err != nil {
				return err
			}
			return fmt.Errorf("tool %s: check script failed (exit code %d)", t.Name, resp.ExitCode)
		}
		row.Option = resp.LastLine
	}
	return s.store.SetRoomTool(ctx, row)
}

// installTool runs the install script of a tool and then its check script,
// which records the tool in the room's inventory.
func (s *Server) installTool(ctx context.Context, room *roomops.Room, roomID int64, osName string, t *config.Tool, up roomops.Tool) error {
	if t.Script(osName, config.StepInstall) != "" {
		if err := s.store.SetRoomTool(ctx, store.RoomTool{RoomID: roomID, Tool: t.Name, ScriptsSHA256: up.Hash,
			State: store.ToolInstalling, Env: sortedKeys(t.Env), CheckedAt: time.Now()}); err != nil {
			return err
		}
		timeout := t.ScriptTimeout(config.StepInstall)
		sctx, cancel := context.WithTimeout(ctx, timeout+time.Minute)
		defer cancel()
		resp, err := room.RunScript(sctx, t.Name, config.StepInstall, "", protocol.RunScriptRequest{TimeoutSeconds: int(timeout / time.Second)})
		if err != nil {
			return err
		}
		s.store.AddEvent(ctx, time.Now(), "", roomID, "script",
			fmt.Sprintf("%s install exited with code %d\n%s", t.Name, resp.ExitCode, resp.OutputTail))
		if resp.TimedOut || resp.ExitCode != 0 {
			if err := s.store.SetRoomTool(ctx, store.RoomTool{RoomID: roomID, Tool: t.Name, ScriptsSHA256: up.Hash,
				State: store.ToolFailed, Env: sortedKeys(t.Env), CheckedAt: time.Now()}); err != nil {
				return err
			}
			if resp.TimedOut {
				return fmt.Errorf("tool %s: install script timed out after %s", t.Name, timeout)
			}
			return fmt.Errorf("tool %s: install script exited with code %d", t.Name, resp.ExitCode)
		}
	}
	return s.runCheck(ctx, room, roomID, osName, t)
}

// installedTools returns the scripts hash of each tool that the room has
// installed and checked (state ready).
func (s *Server) installedTools(ctx context.Context, roomID int64) (map[string]string, error) {
	rts, err := s.store.RoomTools(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, rt := range rts {
		if rt.RoomID == roomID && rt.State == store.ToolReady {
			out[rt.Tool] = rt.ScriptsSHA256
		}
	}
	return out, nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// roomUpload computes what the room must have: the helper and mise for its OS
// and architecture, the machine-level mise tools, the room environment, and the
// directories of the machine's defined tools.
func (s *Server) roomUpload(lc *liveConfig, room *roomops.Room, m *config.Machine) (roomops.Upload, error) {
	goos, ext := room.OS, ""
	switch goos {
	case config.OSMacOS:
		goos = "darwin"
	case config.OSWindows:
		ext = ".exe"
	}
	u := roomops.Upload{
		HelperFile: filepath.Join(lc.cfg.HelperDir, fmt.Sprintf("hostel-%s-%s%s", goos, room.Arch, ext)),
		MiseFile:   filepath.Join(lc.cfg.HelperDir, fmt.Sprintf("mise-%s-%s%s", goos, room.Arch, ext)),
		Tools:      map[string]roomops.Tool{},
	}
	var err error
	if u.MiseConfig, err = toml.Marshal(map[string]any{"tools": miseConfigTools(m)}); err != nil {
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
	for _, t := range lc.cfg.DefinedTools(m) {
		u.Tools[t.Name] = lc.uploads[t.Name][m.OS]
	}
	return u, nil
}

// miseConfigTools returns the [tools] of hostel/mise/config.toml: each mise
// tool of the machine with its version, or its versions.
func miseConfigTools(m *config.Machine) map[string]any {
	out := map[string]any{}
	for name, versions := range m.MiseTools() {
		// mise names the JDK "java"; Hostel's tool name is jdk (design,
		// section 3).
		if name == "jdk" {
			name = "java"
		}
		if len(versions) == 1 {
			out[name] = versions[0]
		} else {
			out[name] = versions
		}
	}
	return out
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
		"MISE_IDIOMATIC_VERSION_FILE_ENABLE_TOOLS": strings.Join(s.config().Mise.IdiomaticTools, ","),
	}
	if s.config().Mise.GithubToken != "" {
		env["MISE_GITHUB_TOKEN"] = s.config().Mise.GithubToken
	}
	return env
}
