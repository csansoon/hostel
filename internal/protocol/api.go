package protocol

import "time"

// Roles: users are admins or members (multiplayer.md, section 6); room
// tokens have RoleRoom.
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
	RoleRoom   = "room"
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
}

// Status is the response of GET /v1/status.
type Status struct {
	// Revision changes when the status changes (M10: the long poll of
	// GET /v1/status?since=...).
	Revision string `json:"revision"`
	ServerID string `json:"server_id"`
	// User and Role are the caller's.
	User     string          `json:"user,omitempty"`
	Role     string          `json:"role,omitempty"`
	Machines []MachineStatus `json:"machines"`
	Rooms    []RoomStatus    `json:"rooms"`
	Leases   []LeaseStatus   `json:"leases"`
}

// MachineStatus is a machine of the configuration (M10).
type MachineStatus struct {
	Key       string   `json:"key"`
	Kind      string   `json:"kind"`
	OS        string   `json:"os"`
	Platforms []string `json:"platforms"`
	Labels    []string `json:"labels"`
	MaxRooms  int      `json:"max_rooms"`
	Connect   bool     `json:"connect,omitempty"`
}

// Hardware is what the room helper found at the last provisioning of the
// room (M10); DiskFreeBytes is the free disk of the home at that time.
type Hardware struct {
	CPU           string `json:"cpu,omitempty"`
	Cores         int    `json:"cores,omitempty"`
	MemoryBytes   int64  `json:"memory_bytes,omitempty"`
	DiskFreeBytes int64  `json:"disk_free_bytes,omitempty"`
	OSVersion     string `json:"os_version,omitempty"`
}

// Usage is the live use of a room (M10b), read when its details open.
type Usage struct {
	CPUPercent       float64 `json:"cpu_percent"`
	MemoryUsedBytes  int64   `json:"memory_used_bytes,omitempty"`
	MemoryTotalBytes int64   `json:"memory_total_bytes,omitempty"`
	DiskFreeBytes    int64   `json:"disk_free_bytes,omitempty"`
}

// LeaseStatus is a lease that is not final, in GET /v1/status.
type LeaseStatus struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	Position  int       `json:"position,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Platform  string    `json:"platform"`
	Devices   []string  `json:"devices"`
	Task      string    `json:"task"`
	Priority  string    `json:"priority"`
	User      string    `json:"user"`
	Guest     string    `json:"guest"`
	Repo      string    `json:"repo"`
	Branch    string    `json:"branch,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	Machine   string    `json:"machine,omitempty"`
	RoomID    int64     `json:"room_id,omitempty"`
	Slot      string    `json:"slot,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ReadyAt   time.Time `json:"ready_at,omitzero"`
	// EstimatedWaitSeconds: for a queued lease.
	EstimatedWaitSeconds int       `json:"estimated_wait_seconds,omitempty"`
	ExpiresAt            time.Time `json:"expires_at,omitzero"`
}

// Platform is one entry of GET /v1/platforms.
type Platform struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Description string   `json:"description"`
	Devices     []string `json:"devices"`
	MaxDevices  int      `json:"max_devices"`
	// FreeSlots counts free slots in running rooms. NewRoom is true when an
	// on-demand machine can create a room now.
	FreeSlots            int  `json:"free_slots"`
	NewRoom              bool `json:"new_room"`
	Queue                int  `json:"queue"`
	EstimatedWaitSeconds int  `json:"estimated_wait_seconds"`
	// Machines are the keys of the machines that can serve the platform
	// (M10b); TypicalLeaseSeconds is the median of its last leases (0: none
	// yet).
	Machines            []string `json:"machines"`
	TypicalLeaseSeconds int      `json:"typical_lease_seconds,omitempty"`
}

type RoomStatus struct {
	ID       int64        `json:"id"`
	Name     string       `json:"name"`
	Provider string       `json:"provider"`
	Machine  string       `json:"machine"`
	OS       string       `json:"os"`
	Arch     string       `json:"arch"`
	State    string       `json:"state"`
	Reason   string       `json:"reason,omitempty"`
	Drained  bool         `json:"drained,omitempty"`
	Slots    []SlotStatus `json:"slots"`
	// Platforms are the platforms that the room is ready for.
	Platforms []string `json:"platforms"`
	// Deadline is the provider-side deadline of an on-demand room.
	Deadline time.Time `json:"deadline,omitzero"`
	Hardware *Hardware `json:"hardware,omitempty"`
	// LastSeen is the last presence check that the room answered; only for
	// an offline room (broken, reason "unreachable"; M10b).
	LastSeen time.Time `json:"last_seen,omitzero"`
}

// RoomDetails is GET /v1/rooms/{name} (M10): the room, its machine, the jobs
// of its leases (from the room), its queue, and its last events.
type RoomDetails struct {
	Room    RoomStatus     `json:"room"`
	Machine *MachineStatus `json:"machine,omitempty"`
	Leases  []LeaseStatus  `json:"leases"`
	// Jobs are the jobs of each ready lease on the room; JobsError says why
	// they are not known. A member gets the jobs and events of the member's
	// own leases only.
	Jobs      map[string][]JobInfo `json:"jobs"`
	JobsError string               `json:"jobs_error,omitempty"`
	Queue     []LeaseStatus        `json:"queue"`
	Events    []Event              `json:"events"`
	// Usage is the live use of the room (M10b; nil when it is not known).
	Usage *Usage `json:"usage,omitempty"`
}

// Event is an entry of the event log.
type Event struct {
	Time    time.Time `json:"time"`
	LeaseID string    `json:"lease_id,omitempty"`
	Kind    string    `json:"kind"`
	Message string    `json:"message"`
}

type SlotStatus struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	LeaseID string `json:"lease_id,omitempty"`
}

// CreateTokenRequest is the body of POST /v1/tokens.
type CreateTokenRequest struct {
	Name    string `json:"name"`
	Role    string `json:"role"`
	Machine string `json:"machine,omitempty"` // "<provider>/<machine>", only for the room role
}

// TokenResponse is the response of POST /v1/tokens. Token is shown only once.
type TokenResponse struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Role    string `json:"role"`
	Machine string `json:"machine,omitempty"`
	Token   string `json:"token"`
}

// TunnelRequest is a tunnel in a lease request. RoomPort 0 selects a port from
// the slot's pool.
type TunnelRequest struct {
	Name     string `json:"name"`
	RoomPort int    `json:"room_port"`
}

// CreateLeaseRequest is the body of POST /v1/leases.
type CreateLeaseRequest struct {
	Platform       string          `json:"platform"`
	Devices        []string        `json:"devices"`
	Task           string          `json:"task"`
	Priority       string          `json:"priority"`
	RequiredLabels []string        `json:"required_labels"`
	Tunnels        []TunnelRequest `json:"tunnels"`
	Fresh          bool            `json:"fresh"`
	PublicKey      string          `json:"public_key"`
	GuestHost      string          `json:"guest_host"`
	Repo           string          `json:"repo"`
	Worktree       string          `json:"worktree"`
	// Branch is the git branch of the worktree (M10b; empty when unknown).
	Branch string `json:"branch,omitempty"`
}

// Error codes of POST /v1/leases (HTTP 422, R4).
const (
	CodeUnknownPlatform = "unknown_platform"
	CodeUnknownDevice   = "unknown_device"
	CodeTooManyDevices  = "too_many_devices"
	CodeNoMachine       = "no_machine"
	// CodeLeaseEnded: POST /v1/leases/{id}/touch for a lease that ended (HTTP 409).
	CodeLeaseEnded = "lease_ended"
)

// Lease is a lease as the API returns it.
type Lease struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	Revision  int64     `json:"revision"`
	Position  int       `json:"position,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Platform  string    `json:"platform"`
	Devices   []string  `json:"devices"`
	Task      string    `json:"task"`
	Priority  string    `json:"priority"`
	Provider  string    `json:"provider,omitempty"`
	Machine   string    `json:"machine,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	EndReason string    `json:"end_reason,omitempty"`
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
	Slot               string         `json:"slot"`
	WorkDir            string         `json:"work_dir"`
	TunnelPorts        map[string]int `json:"tunnel_ports"`
	RoomOS             string         `json:"room_os"`
	RoomArch           string         `json:"room_arch"`
	TunnelCheck        string         `json:"tunnel_check"`
	PortProcessOffline string         `json:"port_process_offline"`
	ReadyAt            time.Time      `json:"ready_at"`
	ExpiresAt          time.Time      `json:"expires_at"`
	IdleExpiresAt      time.Time      `json:"idle_expires_at"`
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
// first, or before another queued lease of its platform.
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

// CodeUpdateRequired: the guest is older than the server's minimum guest
// version (HTTP 426).
const CodeUpdateRequired = "update_required"

// ConfigPlatform is a platform of the server's configuration (M9).
type ConfigPlatform struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Description string `json:"description,omitempty"`
	// Source is default, changed (a default with own files), or own.
	Source    string    `json:"source"`
	Revision  int       `json:"revision,omitempty"`
	User      string    `json:"user,omitempty"`
	ChangedAt time.Time `json:"changed_at,omitzero"`
	Machines  []string  `json:"machines"`
}

// PlatformFiles are the files of a platform: file name → content.
type PlatformFiles struct {
	Name  string            `json:"name"`
	Files map[string]string `json:"files"`
}

// MachineInviteRequest makes a machine invite (M9): "hostel machine join" on
// the machine adds it to the configuration.
type MachineInviteRequest struct {
	Name      string   `json:"name"`
	Provider  string   `json:"provider,omitempty"`
	Platforms []string `json:"platforms,omitempty"`
	Labels    []string `json:"labels,omitempty"`
}

// MachineInviteResponse has the code and the commands to run on the machine.
type MachineInviteResponse struct {
	Machine   string    `json:"machine"`
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

// MachineJoinResponse returns the room token once.
type MachineJoinResponse struct {
	ServerID        string `json:"server_id"`
	Machine         string `json:"machine"`
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
