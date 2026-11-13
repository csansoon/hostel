package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/machineops"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/provider"
	"github.com/csansoon/hostel/internal/store"
)

// probeTimeout bounds the probe and the upload of one machine.
const probeTimeout = 10 * time.Minute

// miseInstallTimeout bounds the install of the machine-level tools.
const miseInstallTimeout = 20 * time.Minute

func roomNames(m *config.Template) []string {
	names := make([]string, len(m.Rooms))
	for i, sl := range m.Rooms {
		names[i] = sl.Name
	}
	return names
}

// ensureStaticMachines gives each static machine a machine row. At start, it marks
// machines of machines that left the configuration as retired, and each static
// machine is probed again (state provisioning), except machines that only the owner
// can repair. After a configuration change (start false), it only adds machines
// and probes a retired machine whose machine is back; refreshMachines does the rest
// when the machines are free.
func (s *Server) ensureStaticMachines(ctx context.Context, start bool) error {
	machines, err := s.store.Machines(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	byTemplate := map[string]store.Machine{}
	for _, r := range machines {
		if p := s.config().Providers[r.Provider]; (p != nil && p.Kind == config.ProviderStatic) || r.State == protocol.MachineRetired {
			byTemplate[r.Provider+"/"+r.Template] = r
		}
	}
	for _, m := range s.config().Templates() {
		if m.Kind != config.ProviderStatic {
			continue
		}
		r, ok := byTemplate[m.Key()]
		delete(byTemplate, m.Key())
		if !ok {
			if _, err := s.store.InsertMachine(ctx, store.Machine{Name: m.Key(), Provider: m.Provider, Template: m.Name,
				State: protocol.MachineProvisioning, OS: m.OS}, s.config().ServerID, roomNames(m), now); err != nil {
				return err
			}
			continue
		}
		if !start && r.State != protocol.MachineRetired {
			continue
		}
		if err := s.store.EnsureRooms(ctx, r.ID, roomNames(m)); err != nil {
			return err
		}
		probeAgain := []string{protocol.MachineRunning, protocol.MachineRetired}
		if r.State == protocol.MachineBroken && r.BrokenReason == reasonUnreachable {
			probeAgain = append(probeAgain, protocol.MachineBroken)
		}
		if _, err := s.store.SetMachineState(ctx, r.ID, probeAgain, protocol.MachineProvisioning, "", now); err != nil {
			return err
		}
	}
	for _, r := range byTemplate {
		if start && r.State != protocol.MachineRetired {
			if _, err := s.store.SetMachineState(ctx, r.ID, []string{r.State}, protocol.MachineRetired, "machine removed from the configuration", now); err != nil {
				return err
			}
		}
	}
	return nil
}

const reasonUnreachable = "unreachable"

// reasonHostKeyChanged: the machine answered with another SSH host key; an
// admin repairs it and accepts the new key.
const reasonHostKeyChanged = "host key changed"

// connect opens an SSH connection to a machine and pins its host key at the first
// connection.
func (s *Server) connect(ctx context.Context, r store.Machine) (*machineops.Conn, error) {
	return s.connectAs(ctx, r, "")
}

// connectAs connects to a machine as user (a room's own user), or as the
// provider's user if user is empty.
func (s *Server) connectAs(ctx context.Context, r store.Machine, user string) (*machineops.Conn, error) {
	p := s.providerMap()[r.Provider]
	if p == nil {
		return nil, fmt.Errorf("provider %q is not in the configuration", r.Provider)
	}
	access, err := p.Access(ctx, provider.MachineRef{Name: r.Name, Template: r.Template, ID: r.ProviderRef})
	if err != nil {
		return nil, fmt.Errorf("machine access: %w", err)
	}
	if user != "" {
		access.User = user
	}
	conn, err := machineops.Connect(ctx, access, r.SSHHostKey)
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

// doProvision probes a machine and uploads what it needs; then it cleans dirty
// rooms, installs the mise tools, runs the install and check scripts of the
// defined tools that are not up to date (on-demand machines: software tools
// only), and the check scripts of the installed tools.
func (s *Server) doProvision(ctx context.Context, r store.Machine, m *config.Template) error {
	// One configuration for the whole step: the scripts that run and the
	// hashes that the machine records are of one version (the next provisioning
	// follows a newer configuration).
	lc := s.live.Load()
	conn, err := s.connect(ctx, r)
	if err != nil {
		return err
	}
	defer conn.Close()
	machine, err := machineops.Probe(ctx, conn)
	if err != nil {
		return err
	}
	// The helper and mise follow the probe. Tool scripts follow the
	// machine's os: an external machine can declare another OS (the E2E tests
	// run .macos.sh scripts in Linux containers this way).
	if m.Kind == config.ProviderStatic && machine.OS != m.OS {
		return fmt.Errorf("the machine runs %s, but the configuration says os = %q", machine.OS, m.OS)
	}
	if err := s.store.SetMachineProbe(ctx, r.ID, machine.OS, machine.Arch, machine.Home, machine.Shell); err != nil {
		return err
	}
	upload, err := s.machineUpload(lc, machine, m)
	if err != nil {
		return err
	}
	helperSum, err := s.syncMachine(ctx, r.ID, machine, upload)
	if err != nil {
		return err
	}
	if err := s.store.SetHelperSHA256(ctx, r.ID, helperSum); err != nil {
		return err
	}
	// Room users first: the cleanup of a dirty room runs as its user, with its
	// recorded home and its current helper.
	if err := s.provisionRoomUsers(ctx, r, m); err != nil {
		return err
	}
	if m.LeaseUsers {
		if err := checkLeaseUserRoot(ctx, machine.Conn); err != nil {
			return err
		}
		if err := s.deleteLeaseUsers(ctx, r.ID, ""); err != nil {
			return err
		}
	}
	if err := s.cleanDirtyRooms(ctx, machine, r.ID); err != nil {
		return err
	}
	// Machine-level mise tools, before the tool scripts, so the scripts can use
	// them. A failure is a machine provisioning failure. It also runs without
	// tools and without agent-device, because it removes an agent-device
	// command that an earlier configuration installed.
	var resp protocol.MiseInstallResponse
	req := protocol.MiseInstallRequest{TimeoutSeconds: int(miseInstallTimeout.Seconds()), AgentDevice: s.config().Mise.AgentDeviceEnabled()}
	if err := machine.Helper(ctx, []string{"mise-install"}, req, &resp); err != nil {
		return err
	}
	if resp.TimedOut || resp.ExitCode != 0 {
		s.store.AddEvent(ctx, time.Now(), "", r.ID, "mise", fmt.Sprintf("mise install exited with code %d\n%s", resp.ExitCode, resp.OutputTail))
		return fmt.Errorf("machine-level tools: mise install failed (exit code %d); see the events", resp.ExitCode)
	}
	// The inventory has each tool of the machine: the mise tools with the
	// versions that mise installed; tools that the machine no longer lists
	// leave it.
	var keep []string
	for _, mt := range m.ToolList {
		keep = append(keep, mt.Name)
		if mt.Mise {
			if err := s.store.SetMachineTool(ctx, store.MachineTool{MachineID: r.ID, Tool: mt.Name, State: store.ToolReady,
				Option: resp.Versions[mt.Name], CheckedAt: time.Now()}); err != nil {
				return err
			}
		}
	}
	if err := s.store.RemoveMachineTools(ctx, r.ID, keep); err != nil {
		return err
	}
	ready, err := s.installedTools(ctx, r.ID)
	if err != nil {
		return err
	}
	for _, t := range lc.cfg.DefinedTools(m) {
		sum := installHash(lc, m, t.Name)
		if ready[t.Name] == sum {
			continue
		}
		// On-demand machines install a screen tool when a lease for it first
		// uses the machine (design, section 7).
		if m.OnDemand() && t.Screen {
			continue
		}
		if err := s.installTool(ctx, machine, r.ID, m, t, sum); err != nil {
			return err
		}
	}
	// The provisioning reached the machine: a "last seen" for the presence.
	if err := s.store.SetLastSeen(ctx, r.ID, time.Now()); err != nil {
		s.log.Warn("save the last seen time", "machine", r.Name, "err", err)
	}
	// The hardware is for the status view only: an error does not stop the
	// provisioning.
	var hw protocol.Hardware
	if err := machine.Helper(ctx, []string{"hardware"}, nil, &hw); err != nil {
		s.log.Warn("read machine hardware", "machine", r.Name, "err", err)
	} else if data, err := json.Marshal(hw); err == nil {
		if err := s.store.SetHardware(ctx, r.ID, string(data)); err != nil {
			s.log.Warn("save machine hardware", "machine", r.Name, "err", err)
		}
	}
	return s.runChecks(ctx, machine, r.ID)
}

// provisionRoomUsers prepares the rooms that have their own OS user: each has
// its own hostel directory, helper, and machine-level tools (design, section
// 9). It records each user's home directory.
func (s *Server) provisionRoomUsers(ctx context.Context, r store.Machine, m *config.Template) error {
	for _, sl := range m.Rooms {
		if sl.User == "" {
			continue
		}
		if err := s.provisionRoomUser(ctx, r, m, sl); err != nil {
			return fmt.Errorf("room %s (user %s): %w", sl.Name, sl.User, err)
		}
	}
	return nil
}

func (s *Server) provisionRoomUser(ctx context.Context, r store.Machine, m *config.Template, sl config.Room) error {
	conn, err := s.connectAs(ctx, r, sl.User)
	if err != nil {
		return err
	}
	defer conn.Close()
	machine, err := machineops.Probe(ctx, conn)
	if err != nil {
		return err
	}
	if machine.OS != r.OS {
		return fmt.Errorf("the user runs %s, the machine user %s", machine.OS, r.OS)
	}
	if err := s.store.SetRoomHome(ctx, r.ID, sl.Name, machine.Home); err != nil {
		return err
	}
	upload, err := s.machineUpload(s.live.Load(), machine, m)
	if err != nil {
		return err
	}
	if _, err := s.syncMachine(ctx, r.ID, machine, upload); err != nil {
		return err
	}
	var resp protocol.MiseInstallResponse
	req := protocol.MiseInstallRequest{TimeoutSeconds: int(miseInstallTimeout.Seconds()), AgentDevice: s.config().Mise.AgentDeviceEnabled()}
	if err := machine.Helper(ctx, []string{"mise-install"}, req, &resp); err != nil {
		return err
	}
	if resp.TimedOut || resp.ExitCode != 0 {
		s.store.AddEvent(ctx, time.Now(), "", r.ID, "mise", fmt.Sprintf("room %s: mise install exited with code %d\n%s", sl.Name, resp.ExitCode, resp.OutputTail))
		return fmt.Errorf("machine-level tools: mise install failed (exit code %d); see the events", resp.ExitCode)
	}
	return nil
}

// roomUser returns the room's own OS user, or "" (the machine user).
func roomUser(m *config.Template, room string) string {
	if m == nil {
		return ""
	}
	for _, sl := range m.Rooms {
		if sl.Name == room {
			return sl.User
		}
	}
	return ""
}

// roomUserOf returns the OS user of a room: its configured user, the user of
// its lease (M11), or "" (the machine user).
func (s *Server) roomUserOf(ctx context.Context, r store.Machine, room string) (string, error) {
	if u := roomUser(s.config().Template(r.Provider+"/"+r.Template), room); u != "" {
		return u, nil
	}
	return s.store.RoomLeaseUser(ctx, r.ID, room)
}

// roomHome returns the home directory of a room's user: its own user's, or
// the machine user's.
func (s *Server) roomHome(ctx context.Context, r store.Machine, room string) (string, error) {
	if u, err := s.roomUserOf(ctx, r, room); err != nil || u == "" {
		return r.Home, err
	}
	home, err := s.store.RoomHome(ctx, r.ID, room)
	if err == nil && home == "" {
		err = fmt.Errorf("room %s: the home directory of its user is not known; the machine provisioning records it", room)
	}
	return home, err
}

// installHash identifies the install of a tool on a machine: the hash of the
// tool's files for the machine's OS, the machine's env (install scripts read it,
// for example HOSTEL_ANDROID_API) and, for a screen tool, the options that the
// machine lists (HOSTEL_TOOL_ALL_OPTIONS). A change of one makes the machine
// install the tool again.
func installHash(lc *liveConfig, m *config.Template, name string) string {
	sum := lc.uploads[name][m.OS].Hash
	if sum == "" {
		return sum
	}
	options := ""
	if t := lc.cfg.Tools[name]; t != nil && t.Screen {
		options = strings.Join(configuredHaves(lc.cfg, m)[name].Options, ",")
	}
	env, _ := json.Marshal(m.Env)
	h := sha256.Sum256([]byte(sum + "\n" + options + "\n" + string(env)))
	return hex.EncodeToString(h[:])
}

// checkTools returns the defined tools of a machine that have a check script and
// that the machine has installed with the current files, each with the install
// hash that its inventory row keeps. An on-demand machine installs a screen tool
// only when the first lease for it uses the machine (design, section 7), and a
// check script can need the result of install (for example the ios-simulator
// golden simulators).
func (s *Server) checkTools(ctx context.Context, machineID int64) (map[string]string, *config.Template, error) {
	r, err := s.store.Machine(ctx, machineID)
	if err != nil {
		return nil, nil, err
	}
	m, err := s.templateOf(r)
	if err != nil {
		return nil, nil, err
	}
	installed, err := s.installedTools(ctx, machineID)
	if err != nil {
		return nil, nil, err
	}
	lc := s.live.Load()
	out := map[string]string{}
	for _, t := range lc.cfg.DefinedTools(m) {
		sum := installHash(lc, m, t.Name)
		if sum != "" && t.Script(m.OS, config.StepCheck) != "" && installed[t.Name] == sum {
			out[t.Name] = sum
		}
	}
	return out, m, nil
}

// runChecks runs the check scripts of checkTools and records the option that
// each one reports.
func (s *Server) runChecks(ctx context.Context, machine *machineops.Machine, machineID int64) error {
	sums, m, err := s.checkTools(ctx, machineID)
	if err != nil {
		return err
	}
	for _, name := range sortedKeys(sums) {
		t := s.config().Tools[name]
		if t == nil {
			continue
		}
		if err := s.runCheck(ctx, machine, machineID, m, t, sums[name]); err != nil {
			return err
		}
	}
	return nil
}

// runCheck runs the check script of a tool and writes its inventory row with
// the option that the last line of the output reports, and sum, the install
// hash of what the machine has. A tool without a check script has no option.
func (s *Server) runCheck(ctx context.Context, machine *machineops.Machine, machineID int64, m *config.Template, t *config.Tool, sum string) error {
	osName := m.OS
	row := store.MachineTool{MachineID: machineID, Tool: t.Name, ScriptsSHA256: sum, State: store.ToolReady,
		Env: envNames(t), CheckedAt: time.Now()}
	if t.Script(osName, config.StepCheck) != "" {
		timeout := t.ScriptTimeout(config.StepCheck)
		resp, err := machine.RunScript(ctx, t.Name, config.StepCheck, "", protocol.RunScriptRequest{TimeoutSeconds: int(timeout / time.Second)})
		if err != nil {
			return err
		}
		if resp.TimedOut || resp.ExitCode != 0 {
			s.store.AddEvent(ctx, time.Now(), "", machineID, "script",
				fmt.Sprintf("%s check exited with code %d\n%s", t.Name, resp.ExitCode, resp.OutputTail))
			row.State = store.ToolFailed
			if err := s.store.SetMachineTool(ctx, row); err != nil {
				return err
			}
			return fmt.Errorf("tool %s: check script failed (exit code %d)", t.Name, resp.ExitCode)
		}
		row.Option = resp.LastLine
		if msg := noTemplateOption(s.config(), m, t, row.Option); msg != "" {
			s.store.AddEvent(ctx, time.Now(), "", machineID, "script", msg)
			row.State, row.Option = store.ToolFailed, ""
			if err := s.store.SetMachineTool(ctx, row); err != nil {
				return err
			}
			return fmt.Errorf("tool %s: check: %s", t.Name, msg)
		}
	}
	return s.store.SetMachineTool(ctx, row)
}

// noTemplateOption explains a check of a screen tool that reports none of the
// options that the machine lists (also when it reports nothing): no lease can
// start the tool on the machine. It returns "" for any other check.
func noTemplateOption(cfg *config.Server, m *config.Template, t *config.Tool, reported string) string {
	listed := configuredHaves(cfg, m)[t.Name].Options
	got := splitOptions(reported)
	if !t.Screen || len(listed) == 0 || slices.ContainsFunc(got, func(o string) bool { return slices.Contains(listed, o) }) {
		return ""
	}
	what := "no option"
	if len(got) > 0 {
		what = strings.Join(got, ", ")
	}
	return fmt.Sprintf("%s check reported %s, none of the options of the machine (%s); repair the machine to run install again",
		t.Name, what, strings.Join(listed, ", "))
}

// installTool runs the install script of a tool and then its check script,
// which records the tool in the machine's inventory with sum, the install hash.
func (s *Server) installTool(ctx context.Context, machine *machineops.Machine, machineID int64, m *config.Template, t *config.Tool, sum string) error {
	osName := m.OS
	if t.Script(osName, config.StepInstall) != "" {
		if err := s.store.SetMachineTool(ctx, store.MachineTool{MachineID: machineID, Tool: t.Name, ScriptsSHA256: sum,
			State: store.ToolInstalling, Env: envNames(t), CheckedAt: time.Now()}); err != nil {
			return err
		}
		timeout := t.ScriptTimeout(config.StepInstall)
		sctx, cancel := context.WithTimeout(ctx, timeout+time.Minute)
		defer cancel()
		resp, err := machine.RunScript(sctx, t.Name, config.StepInstall, "", protocol.RunScriptRequest{TimeoutSeconds: int(timeout / time.Second)})
		if err != nil {
			return err
		}
		s.store.AddEvent(ctx, time.Now(), "", machineID, "script",
			fmt.Sprintf("%s install exited with code %d\n%s", t.Name, resp.ExitCode, resp.OutputTail))
		if resp.TimedOut || resp.ExitCode != 0 {
			if err := s.store.SetMachineTool(ctx, store.MachineTool{MachineID: machineID, Tool: t.Name, ScriptsSHA256: sum,
				State: store.ToolFailed, Env: envNames(t), CheckedAt: time.Now()}); err != nil {
				return err
			}
			if resp.TimedOut {
				return fmt.Errorf("tool %s: install script timed out after %s", t.Name, timeout)
			}
			return fmt.Errorf("tool %s: install script exited with code %d", t.Name, resp.ExitCode)
		}
	}
	return s.runCheck(ctx, machine, machineID, m, t, sum)
}

// installedTools returns the scripts hash of each tool that the machine has
// installed and checked (state ready).
func (s *Server) installedTools(ctx context.Context, machineID int64) (map[string]string, error) {
	rts, err := s.store.MachineTools(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, rt := range rts {
		if rt.MachineID == machineID && rt.State == store.ToolReady {
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

// machineUpload computes what the machine must have: the helper and mise for its OS
// and architecture, the machine-level mise tools, the machine environment, and the
// directories of the machine's defined tools.
func (s *Server) machineUpload(lc *liveConfig, machine *machineops.Machine, m *config.Template) (machineops.Upload, error) {
	goos, ext := machine.OS, ""
	switch goos {
	case config.OSMacOS:
		goos = "darwin"
	case config.OSWindows:
		ext = ".exe"
	}
	u := machineops.Upload{
		HelperFile: filepath.Join(lc.cfg.HelperDir, fmt.Sprintf("hostel-%s-%s%s", goos, machine.Arch, ext)),
		MiseFile:   filepath.Join(lc.cfg.HelperDir, fmt.Sprintf("mise-%s-%s%s", goos, machine.Arch, ext)),
		Tools:      map[string]machineops.Tool{},
	}
	var err error
	if u.MiseConfig, err = toml.Marshal(map[string]any{"tools": miseConfigTools(m)}); err != nil {
		return u, err
	}
	env := map[string]string{}
	for k, v := range m.Env {
		env[k] = v
	}
	for k, v := range s.miseEnv(machine.OS, machine.Home) {
		env[k] = v
	}
	if machine.SharedHome != "" {
		env["MISE_SHARED_INSTALL_DIRS"] = machineops.Path(machine.OS, machine.SharedHome, "cache", "mise", "installs")
	}
	re := protocol.MachineEnv{Env: env, Rooms: map[string]map[string]string{}, OS: m.OS, ToolOptions: map[string][]string{}}
	for _, sl := range m.Rooms {
		re.Rooms[sl.Name] = sl.Env
	}
	for name, h := range configuredHaves(lc.cfg, m) {
		if h.Screen {
			re.ToolOptions[name] = h.Options
		}
	}
	if u.MachineEnv, err = json.Marshal(re); err != nil {
		return u, err
	}
	for _, t := range lc.cfg.DefinedTools(m) {
		u.Tools[t.Name] = lc.uploads[t.Name][m.OS]
	}
	return u, nil
}

// miseConfigTools returns the [tools] of hostel/mise/config.toml: each mise
// tool of the machine with its version, or its versions.
func miseConfigTools(m *config.Template) map[string]any {
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

// miseEnv returns the MISE_* variables of all machine-side processes (design,
// section 9, "mise on machines").
func (s *Server) miseEnv(osName, home string) map[string]string {
	env := map[string]string{
		"MISE_DATA_DIR":             machineops.Path(osName, home, "cache", "mise"),
		"MISE_CACHE_DIR":            machineops.Path(osName, home, "cache", "mise-cache"),
		"MISE_CONFIG_DIR":           machineops.Path(osName, home, "mise"),
		"MISE_STATE_DIR":            machineops.Path(osName, home, "mise-state"),
		"MISE_TRUSTED_CONFIG_PATHS": machineops.Path(osName, home, "rooms"),
		"MISE_YES":                  "1",
		"MISE_IDIOMATIC_VERSION_FILE_ENABLE_TOOLS": strings.Join(s.config().Mise.IdiomaticTools, ","),
	}
	if s.config().Mise.GithubToken != "" {
		env["MISE_GITHUB_TOKEN"] = s.config().Mise.GithubToken
	}
	return env
}
