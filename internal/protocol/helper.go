package protocol

import "time"

// HelperInfo is the output of "hostel room info".
type HelperInfo struct {
	OS      string       `json:"os"`
	Arch    string       `json:"arch"`
	Version string       `json:"version"`
	Hashes  HelperHashes `json:"hashes"`
}

// HelperHashes has the SHA-256 of each item that the server uploads. A missing
// item has an empty hash.
type HelperHashes struct {
	Helper     string            `json:"helper"`
	Mise       string            `json:"mise"`
	MiseConfig string            `json:"mise_config"`
	RoomEnv    string            `json:"room_env"`
	Tools      map[string]string `json:"tools"`
}

// RoomEnv is hostel/room-env.json: the provider and machine env, the MISE_*
// variables, and the env of each slot. The helper adds it to the environment of
// scripts, jobs, and shells.
type RoomEnv struct {
	Env   map[string]string            `json:"env"`
	Slots map[string]map[string]string `json:"slots"`
	// OS is the machine's configured OS. It selects the tool scripts; it
	// can differ from the probed OS (external machines).
	OS string `json:"os"`
}

// RunScriptRequest is the input of "hostel room run-script".
type RunScriptRequest struct {
	Args           []string          `json:"args"`
	Env            map[string]string `json:"env"`
	TimeoutSeconds int               `json:"timeout_seconds"`
}

// MiseInstallRequest is the input of "hostel room mise-install".
type MiseInstallRequest struct {
	TimeoutSeconds int  `json:"timeout_seconds"`
	AgentDevice    bool `json:"agent_device"`
}

// RunScriptResponse is the output of "hostel room run-script".
type RunScriptResponse struct {
	ExitCode   int    `json:"exit_code"`
	OutputTail string `json:"output_tail"`
	// LastLine is the last line of the output that is not empty: the
	// version or the options that a check script prints.
	LastLine string `json:"last_line,omitempty"`
	// Missing is true when the tool has no script for this step and OS.
	Missing  bool `json:"missing,omitempty"`
	TimedOut bool `json:"timed_out,omitempty"`
}

// HelperError is the output of a helper command that fails.
type HelperError struct {
	Error string `json:"error"`
}

// BeginRequest is the input of "hostel room begin".
type BeginRequest struct {
	LeaseID string `json:"lease_id"`
	// Tool is the screen tool that starts for the lease (empty: none), and
	// Options are its options.
	Tool    string   `json:"tool"`
	Options []string `json:"options"`
	// Exclusive: the lease holds the whole room (for example the desktop).
	Exclusive bool `json:"exclusive,omitempty"`
}

// CleanupRequest is the input of "hostel room cleanup". It comes from the
// database, so a repeated cleanup does not need lease.json.
type CleanupRequest struct {
	LeaseID string `json:"lease_id"`
	// Tool is the screen tool that started for the lease (empty: none), and
	// Options are its options.
	Tool    string   `json:"tool"`
	Options []string `json:"options"`
	Ports   []int    `json:"ports"`
	// Exclusive: the lease held the whole room (its scripts may reset the
	// desktop).
	Exclusive bool `json:"exclusive,omitempty"`
	// Check are the tools whose check script runs at the end: those that the
	// room has installed. An on-demand room has the files of a screen tool
	// before its install script ran.
	Check []string `json:"check"`
}

// CleanupResponse lists the result of each cleanup step.
type CleanupResponse struct {
	Steps []CleanupStep `json:"steps"`
}

type CleanupStep struct {
	Name  string `json:"name"`
	Error string `json:"error,omitempty"`
}

// Job trailers end the output of "job attach" (design, section 15).
const (
	TrailerExit = "\x00hostel-exit "
	TrailerLost = "\x00hostel-lost\n"
)

// JobRequest is the input of "job start". With Shell, Command has one string
// that runs with sh -c.
type JobRequest struct {
	Command []string `json:"command"`
	Shell   bool     `json:"shell"`
	// JobID is chosen by the guest, so a start that it repeats after a dropped
	// connection returns the same job instead of a second one.
	JobID string `json:"job_id,omitempty"`
}

// JobInfo is one entry of "job list".
type JobInfo struct {
	ID        string    `json:"id"`
	Command   []string  `json:"command"`
	State     string    `json:"state"` // running, exited, lost
	ExitCode  *int      `json:"exit_code,omitempty"`
	StartedAt time.Time `json:"started_at"`
}
