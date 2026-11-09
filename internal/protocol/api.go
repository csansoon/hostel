package protocol

import (
	"fmt"
	"strings"
	"time"
)

// Roles: users are admins or members (multiplayer.md, section 6); machine
// tokens have RoleMachine.
const (
	RoleAdmin   = "admin"
	RoleMember  = "member"
	RoleMachine = "machine"
)

// Info is the response of GET /v1/info.
type Info struct {
	Version         string `json:"version"`
	GatewayAddress  string `json:"gateway_address"`
	GatewayHostKey  string `json:"gateway_host_key"`
	ServerPublicKey string `json:"server_public_key"`
}

// Error is the body of each error response.
type Error struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// Addresses is data_dir/server.json, written after the listeners start.
type Addresses struct {
	Listen  string `json:"listen"`
	Gateway string `json:"gateway"`
	// URL is the API on the listener: https:// with the server's own TLS.
	URL string `json:"url"`
	// Executable is the server binary ("hostel server update" on the server
	// machine replaces it; M10d).
	Executable string `json:"executable,omitempty"`
}

// Status is the response of GET /v1/status.
type Status struct {
	// Revision changes when the status changes (M10: the long poll of
	// GET /v1/status?since=...).
	Revision string `json:"revision"`
	ServerID string `json:"server_id"`
	// User and Role are the caller's.
	User string `json:"user,omitempty"`
	Role string `json:"role,omitempty"`
	// Providers are in configuration order; a static provider is a group of
	// real hosts.
	Providers []ProviderStatus `json:"providers"`
	Templates []TemplateStatus `json:"templates"`
	Machines  []MachineStatus  `json:"machines"`
	Leases    []LeaseStatus    `json:"leases"`
	// Warning is a problem of the server that an admin must fix, for
	// example a stored configuration of the form before M10e.
	Warning string `json:"warning,omitempty"`
}

// ProviderStatus is a provider of the configuration.
type ProviderStatus struct {
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
	Kind  string `json:"kind"`
	// MaxMachines is the limit of the provider on its machines; 0 for none.
	MaxMachines int `json:"max_machines,omitempty"`
}

// UsageList is GET /v1/usage: the live use of the running machines, by name.
type UsageList struct {
	Machines map[string]Usage `json:"machines"`
}

// TemplateStatus is a machine of the configuration (M10).
type TemplateStatus struct {
	Key  string `json:"key"`
	Kind string `json:"kind"`
	OS   string `json:"os"`
	// Tools are the machine's tool entries, with the tools that needs add.
	Tools       []string `json:"tools"`
	MaxMachines int      `json:"max_machines"`
	Connect     bool     `json:"connect,omitempty"`
	// Hardware is what the machine's configuration states, else what its
	// newest machine measured; nil when neither says.
	Hardware *Hardware `json:"hardware,omitempty"`
	// ConnectVersion is the hostel version of the machine's connect service
	// while it is connected (M10d; "" before).
	ConnectVersion string `json:"connect_version,omitempty"`
}

// Hardware is what the machine helper found at the last provisioning of the
// machine (M10); DiskFreeBytes is the free disk of the home at that time.
type Hardware struct {
	CPU           string `json:"cpu,omitempty"`
	Cores         int    `json:"cores,omitempty"`
	MemoryBytes   int64  `json:"memory_bytes,omitempty"`
	DiskFreeBytes int64  `json:"disk_free_bytes,omitempty"`
	OSVersion     string `json:"os_version,omitempty"`
}

// Size is "8 cores, 16 GB", or "" when neither is known.
func (h *Hardware) Size() string {
	if h == nil {
		return ""
	}
	var parts []string
	if h.Cores > 0 {
		parts = append(parts, fmt.Sprintf("%d cores", h.Cores))
	}
	if h.MemoryBytes > 0 {
		parts = append(parts, fmt.Sprintf("%d GB", (h.MemoryBytes+1<<29)>>30))
	}
	return strings.Join(parts, ", ")
}

// Usage is the live use of a machine (M10b), read when its details open.
type Usage struct {
	CPUPercent       float64 `json:"cpu_percent"`
	MemoryUsedBytes  int64   `json:"memory_used_bytes,omitempty"`
	MemoryTotalBytes int64   `json:"memory_total_bytes,omitempty"`
	DiskFreeBytes    int64   `json:"disk_free_bytes,omitempty"`
}

// LeaseStatus is a lease that is not final, in GET /v1/status.
type LeaseStatus struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Position int    `json:"position,omitempty"`
	Reason   string `json:"reason,omitempty"`
	// Tools is the request; Started the screen tools that started.
	Tools     []string      `json:"tools"`
	Started   []StartedTool `json:"started"`
	Task      string        `json:"task"`
	Priority  string        `json:"priority"`
	User      string        `json:"user"`
	Guest     string        `json:"guest"`
	Repo      string        `json:"repo"`
	Branch    string        `json:"branch,omitempty"`
	Provider  string        `json:"provider,omitempty"`
	Template  string        `json:"template,omitempty"`
	MachineID int64         `json:"machine_id,omitempty"`
	Room      string        `json:"room,omitempty"`
	CreatedAt time.Time     `json:"created_at"`
	ReadyAt   time.Time     `json:"ready_at,omitzero"`
	// EstimatedWaitSeconds: for a queued lease.
	EstimatedWaitSeconds int       `json:"estimated_wait_seconds,omitempty"`
	ExpiresAt            time.Time `json:"expires_at,omitzero"`
}

// Tools is the response of GET /v1/tools.
type Tools struct {
	Tools []Tool `json:"tools"`
	// Note is about the tools of the repository's mise files, which work on
	// every machine.
	Note string `json:"note"`
}

// Tool is one tool of GET /v1/tools: a defined tool, or a mise tool that a
// machine lists.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Screen      bool   `json:"screen,omitempty"`
	// Mise: a tool without a definition that mise installs.
	Mise  bool     `json:"mise,omitempty"`
	Needs []string `json:"needs"`
	// Options are the configured options of a screen tool, or the versions
	// in the inventories (configured versions before the first install).
	Options      []string `json:"options"`
	MaxOptions   int      `json:"max_options,omitempty"`
	WholeMachine bool     `json:"whole_machine,omitempty"`
	// Env are the names of the tool's environment (PATH when it adds to it).
	Env       []string       `json:"env"`
	Guide     string         `json:"guide,omitempty"`
	Templates []ToolTemplate `json:"templates"`
	// FreeRooms counts free rooms in running machines (for a whole-machine tool:
	// machines with all rooms free). NewMachine is true when an
	// on-demand machine can create a machine now.
	FreeRooms            int  `json:"free_rooms"`
	NewMachine           bool `json:"new_machine"`
	Waiting              int  `json:"waiting"`
	EstimatedWaitSeconds int  `json:"estimated_wait_seconds"`
	// TypicalLeaseSeconds is the median of the last leases that asked for
	// the tool (0: none yet).
	TypicalLeaseSeconds int `json:"typical_lease_seconds,omitempty"`
}

// ToolTemplate is a machine that has a tool.
type ToolTemplate struct {
	Key      string   `json:"key"`
	OS       string   `json:"os"`
	Options  []string `json:"options"`
	OnDemand bool     `json:"on_demand,omitempty"`
	Online   bool     `json:"online"`
	// Hardware: see MachineStatus.
	Hardware *Hardware `json:"hardware,omitempty"`
}

// InventoryTool is a tool of a machine's inventory.
type InventoryTool struct {
	Name string `json:"name"`
	// Options are the versions or the options that the check reported.
	Options []string `json:"options"`
	State   string   `json:"state"`
	Env     []string `json:"env"`
	Screen  bool     `json:"screen,omitempty"`
}

type MachineStatus struct {
	ID       int64        `json:"id"`
	Name     string       `json:"name"`
	Provider string       `json:"provider"`
	Template string       `json:"template"`
	OS       string       `json:"os"`
	Arch     string       `json:"arch"`
	State    string       `json:"state"`
	Reason   string       `json:"reason,omitempty"`
	Drained  bool         `json:"drained,omitempty"`
	Rooms    []RoomStatus `json:"rooms"`
	// Tools is the inventory: the tools of the machine with their options
	// and check state.
	Tools []InventoryTool `json:"tools"`
	// Deadline is the provider-side deadline of an on-demand machine.
	Deadline time.Time `json:"deadline,omitzero"`
	Hardware *Hardware `json:"hardware,omitempty"`
	// LastSeen is the last presence check that the machine answered; only for
	// an offline machine (broken, reason "unreachable"; M10b).
	LastSeen time.Time `json:"last_seen,omitzero"`
}

// MachineDetails is GET /v1/machines/{name} (M10): the machine, its template, the jobs
// of its leases (from the machine), its queue, and its last events.
type MachineDetails struct {
	Machine  MachineStatus   `json:"machine"`
	Template *TemplateStatus `json:"template,omitempty"`
	Leases   []LeaseStatus   `json:"leases"`
	// Jobs are the jobs of each ready lease on the machine; JobsError says why
	// they are not known. A member gets the jobs and events of the member's
	// own leases only.
	Jobs      map[string][]JobInfo `json:"jobs"`
	JobsError string               `json:"jobs_error,omitempty"`
	Queue     []LeaseStatus        `json:"queue"`
	Events    []Event              `json:"events"`
	// Usage is the live use of the machine (M10b; nil when it is not known).
	Usage *Usage `json:"usage,omitempty"`
}

// Event is an entry of the event log.
type Event struct {
	Time    time.Time `json:"time"`
	LeaseID string    `json:"lease_id,omitempty"`
	Kind    string    `json:"kind"`
	Message string    `json:"message"`
}

type RoomStatus struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	LeaseID string `json:"lease_id,omitempty"`
}

// CreateTokenRequest is the body of POST /v1/tokens.
type CreateTokenRequest struct {
	Name     string `json:"name"`
	Role     string `json:"role"`
	Template string `json:"template,omitempty"` // "<provider>/<machine>", only for the machine role
}

// TokenResponse is the response of POST /v1/tokens. Token is shown only once.
type TokenResponse struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	Template string `json:"template,omitempty"`
	Token    string `json:"token"`
}

// TunnelRequest is a tunnel in a lease request. MachinePort 0 selects a port from
// the room's pool.
type TunnelRequest struct {
	Name        string `json:"name"`
	MachinePort int    `json:"machine_port"`
}

// CreateLeaseRequest is the body of POST /v1/leases.
type CreateLeaseRequest struct {
	// Tools are "name" or "name:option" entries; none: any machine.
	Tools     []string        `json:"tools"`
	Task      string          `json:"task"`
	Priority  string          `json:"priority"`
	Tunnels   []TunnelRequest `json:"tunnels"`
	Fresh     bool            `json:"fresh"`
	PublicKey string          `json:"public_key"`
	GuestHost string          `json:"guest_host"`
	Repo      string          `json:"repo"`
	Worktree  string          `json:"worktree"`
	// Branch is the git branch of the worktree (M10b; empty when unknown).
	Branch string `json:"branch,omitempty"`
}

// Error codes of POST /v1/leases (HTTP 422, R4).
const (
	CodeUnknownTool    = "unknown_tool"
	CodeUnknownOption  = "unknown_option"
	CodeTooManyOptions = "too_many_options"
	CodeNoTemplate     = "no_machine"
	// CodeLeaseEnded: POST /v1/leases/{id}/touch for a lease that ended (HTTP 409).
	CodeLeaseEnded = "lease_ended"
)

// Lease is a lease as the API returns it.
type Lease struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Revision int64  `json:"revision"`
	Position int    `json:"position,omitempty"`
	Reason   string `json:"reason,omitempty"`
	// WaitingBehind counts the leases before a queued lease in the waiting
	// line that wait for one of its machines.
	WaitingBehind int `json:"waiting_behind,omitempty"`
	// Tools is the request; Started the screen tools that started.
	Tools     []string      `json:"tools"`
	Started   []StartedTool `json:"started"`
	Task      string        `json:"task"`
	Priority  string        `json:"priority"`
	Provider  string        `json:"provider,omitempty"`
	Template  string        `json:"template,omitempty"`
	CreatedAt time.Time     `json:"created_at"`
	EndReason string        `json:"end_reason,omitempty"`
	// EstimatedWaitSeconds is set for a queued lease.
	EstimatedWaitSeconds int `json:"estimated_wait_seconds,omitempty"`
	// PollTimeoutMS is how long a queued lease lives without a poll of its
	// owner; IdleTimeoutMS how long a ready lease lives without a hostel
	// command (the server settings poll_timeout and idle_timeout).
	PollTimeoutMS int64 `json:"poll_timeout_ms,omitempty"`
	IdleTimeoutMS int64 `json:"idle_timeout_ms,omitempty"`
	// Access is set when the lease is ready.
	Access *Access `json:"access,omitempty"`
}

// Access is what a guest needs to use a ready lease.
type Access struct {
	GatewayAddress     string         `json:"gateway_address"`
	GatewayHostKey     string         `json:"gateway_host_key"`
	HelperPath         string         `json:"helper_path"`
	Room               string         `json:"room"`
	WorkDir            string         `json:"work_dir"`
	TunnelPorts        map[string]int `json:"tunnel_ports"`
	MachineOS          string         `json:"machine_os"`
	MachineArch        string         `json:"machine_arch"`
	TunnelCheck        string         `json:"tunnel_check"`
	PortProcessOffline string         `json:"port_process_offline"`
	ReadyAt            time.Time      `json:"ready_at"`
	ExpiresAt          time.Time      `json:"expires_at"`
	IdleExpiresAt      time.Time      `json:"idle_expires_at"`
	// The lease facts: the machine's hardware (with the OS version) and
	// its inventory: all tools of the machine, also the ones that the
	// lease did not ask for.
	Hardware *Hardware       `json:"hardware,omitempty"`
	Tools    []InventoryTool `json:"tools"`
	// LeaseTools are the names of the lease's tools and of their needs, in
	// the order of needs: the [tool.<name>] sections of hostel.toml that apply.
	LeaseTools []string `json:"lease_tools"`
}

// PriorityName returns the API name of a priority.
func PriorityName(p int) string {
	for name, v := range PriorityNames {
		if v == p {
			return name
		}
	}
	return "normal"
}

// ConnectRequest is the body of POST /v1/connect (no bearer token).
type ConnectRequest struct {
	Secret   []byte `json:"secret"`
	Name     string `json:"name"`
	Computer string `json:"computer"`
}

// ConnectResponse returns the new token once.
type ConnectResponse struct {
	ServerID string `json:"server_id"`
	User     string `json:"user"`
	Role     string `json:"role"`
	Token    string `json:"token"`
}

// Me is the response of GET /v1/me.
type Me struct {
	ServerID string `json:"server_id"`
	User     string `json:"user"`
	Role     string `json:"role"`
	Computer string `json:"computer"`
}

// User is one entry of GET /v1/users.
type User struct {
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	Computers []string  `json:"computers"`
	CreatedAt time.Time `json:"created_at"`
}

// InviteRequest is the body of POST /v1/invites: a new user with Role and an
// optional Name (admin), or with Self a code that adds a computer for the
// caller (any user).
type InviteRequest struct {
	Role string `json:"role,omitempty"`
	Name string `json:"name,omitempty"`
	Self bool   `json:"self,omitempty"`
}

// InviteResponse has the code once.
type InviteResponse struct {
	Code        string    `json:"code"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// SetUserRequest is the body of PATCH /v1/users/{name}.
type SetUserRequest struct {
	Role string `json:"role"`
}

// MoveRequest is the body of POST /v1/leases/{id}/move (admin): the lease goes
// first, or before another queued lease.
type MoveRequest struct {
	First  bool   `json:"first,omitempty"`
	Before string `json:"before,omitempty"`
}

// PriorityRequest is the body of POST /v1/leases/{id}/priority (admin).
type PriorityRequest struct {
	Priority string `json:"priority"`
}

// ConfigDocument is the server's configuration document (M9): providers,
// machines, and settings, in TOML. For members, secret values are hidden.
type ConfigDocument struct {
	Revision int    `json:"revision"`
	Document string `json:"document"`
	Hidden   bool   `json:"hidden,omitempty"`
}

// PutConfigRequest changes the document; base_revision must be the latest.
type PutConfigRequest struct {
	BaseRevision int    `json:"base_revision"`
	Document     string `json:"document"`
}

// ConfigRevision is an entry of the configuration history.
type ConfigRevision struct {
	Revision  int       `json:"revision"`
	User      string    `json:"user"`
	CreatedAt time.Time `json:"created_at"`
}

// CodeConflict: the configuration changed since the base revision.
const CodeConflict = "conflict"

// VersionHeader carries the hostel version: of the guest in each request, of
// the server in each answer (M10d).
const VersionHeader = "X-Hostel-Version"

// ServerUpdate is the state of a remote server update (M10d):
// GET/POST /v1/server/update.
type ServerUpdate struct {
	State string `json:"state"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
	Error string `json:"error,omitempty"`
}

// ServerUpdateRequest is the body of POST /v1/server/update ("" for the
// latest release).
type ServerUpdateRequest struct {
	Version string `json:"version,omitempty"`
}

const (
	UpdateIdle    = "idle"
	UpdateRunning = "running"
	UpdateFailed  = "failed"
)

// CodeUpdateRequired: the guest is older than the server's minimum guest
// version (HTTP 426).
const CodeUpdateRequired = "update_required"

// ConfigTool is a defined tool of the server's configuration (M10e).
type ConfigTool struct {
	Name        string `json:"name"`
	Screen      bool   `json:"screen,omitempty"`
	Description string `json:"description,omitempty"`
	// Source is default, changed (a default with own files), or own.
	Source    string    `json:"source"`
	Revision  int       `json:"revision,omitempty"`
	User      string    `json:"user,omitempty"`
	ChangedAt time.Time `json:"changed_at,omitzero"`
	Templates []string  `json:"templates"`
}

// ToolFiles are the files of a tool: file name → content.
type ToolFiles struct {
	Name  string            `json:"name"`
	Files map[string]string `json:"files"`
}

// MachineInviteRequest makes a machine invite (M9): "hostel machine join" on
// the machine adds it to the configuration.
type MachineInviteRequest struct {
	Name     string   `json:"name"`
	Provider string   `json:"provider,omitempty"`
	Tools    []string `json:"tools,omitempty"`
}

// MachineInviteResponse has the code and the commands to run on the machine.
type MachineInviteResponse struct {
	Template  string    `json:"template"`
	Code      string    `json:"code"`
	Unix      string    `json:"unix"`
	Windows   string    `json:"windows"`
	ExpiresAt time.Time `json:"expires_at"`
}

// MachineJoinRequest uses a machine invite (no bearer token).
type MachineJoinRequest struct {
	Secret   []byte `json:"secret"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Hostname string `json:"hostname"`
}

// MachineJoinResponse returns the machine token once.
type MachineJoinResponse struct {
	ServerID        string `json:"server_id"`
	Template        string `json:"template"`
	Token           string `json:"token"`
	ServerPublicKey string `json:"server_public_key"`
}

// Login is a provider CLI login on the server (M9): the address that the admin
// opens, and the state (waiting, done, failed).
type Login struct {
	CLI   string `json:"cli"`
	State string `json:"state"`
	URL   string `json:"url,omitempty"`
	Error string `json:"error,omitempty"`
	// Already: the CLI was logged in; no login ran.
	Already bool `json:"already,omitempty"`
}

// Login states.
const (
	LoginWaiting = "waiting"
	LoginDone    = "done"
	LoginFailed  = "failed"
)
