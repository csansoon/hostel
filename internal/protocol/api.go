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
	Rooms  []RoomStatus  `json:"rooms"`
	Leases []LeaseStatus `json:"leases"`
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
	Provider  string    `json:"provider,omitempty"`
	Machine   string    `json:"machine,omitempty"`
	RoomID    int64     `json:"room_id,omitempty"`
	Slot      string    `json:"slot,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
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
