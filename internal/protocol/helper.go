package protocol

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
	Platforms  map[string]string `json:"platforms"`
}

// RoomEnv is hostel/room-env.json: the provider and machine env, the MISE_*
// variables, and the env of each slot. The helper adds it to the environment of
// scripts, jobs, and shells.
type RoomEnv struct {
	Env   map[string]string            `json:"env"`
	Slots map[string]map[string]string `json:"slots"`
}

// RunScriptRequest is the input of "hostel room run-script".
type RunScriptRequest struct {
	Args           []string          `json:"args"`
	Env            map[string]string `json:"env"`
	TimeoutSeconds int               `json:"timeout_seconds"`
}

// RunScriptResponse is the output of "hostel room run-script".
type RunScriptResponse struct {
	ExitCode   int    `json:"exit_code"`
	OutputTail string `json:"output_tail"`
	// Missing is true when the platform has no script for this step and OS.
	Missing  bool `json:"missing,omitempty"`
	TimedOut bool `json:"timed_out,omitempty"`
}

// HelperError is the output of a helper command that fails.
type HelperError struct {
	Error string `json:"error"`
}
