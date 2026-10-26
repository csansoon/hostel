package config

// agent-device (github.com/callstack/agent-device) drives simulators,
// emulators, and desktops for agents. Hostel installs it on each room (mise
// agent_device) with its own Node, so the repository's Node does not matter.
// T3 Code pins the same version.
const (
	AgentDeviceVersion = "0.21.25"
	AgentDeviceNode    = "22.22.0"
)

// AgentDeviceEnabled reports whether rooms get agent-device (default true).
func (m *Mise) AgentDeviceEnabled() bool { return m.AgentDevice == nil || *m.AgentDevice }
