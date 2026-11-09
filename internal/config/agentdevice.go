package config

// agent-device (github.com/callstack/agent-device) drives simulators,
// emulators, and desktops for agents. Hostel installs it on each machine (mise
// agent_device) with its own Node, so the repository's Node does not matter.
// T3 Code pins the same version.
const (
	AgentDeviceVersion = "0.21.25"
	AgentDeviceNode    = "22.22.0"
)

// AgentDeviceEnabled reports whether machines get agent-device (default true).
func (m *Mise) AgentDeviceEnabled() bool { return m.AgentDevice == nil || *m.AgentDevice }

// winapp (github.com/microsoft/winappCli) drives Windows apps with UI
// Automation ("winapp ui"). Windows machines get this pinned build with
// agent_device; m7-implementation.md.
const (
	WinAppVersion = "0.7.1"
	WinAppURL     = "https://github.com/microsoft/winappCli/releases/download/v0.7.1/winappcli-x64.zip"
	WinAppSHA256  = "d925d1e32cdc320b6d271f653fd2cd3c05b5d7558deb20d1b548bead77900fcf"
)
