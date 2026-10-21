package protocol

import "time"

// Token roles (design, section 18).
const (
	RoleAdmin = "admin"
	RoleGuest = "guest"
	RoleRoom  = "room"
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
}

// Status is the response of GET /v1/status.
type Status struct {
	Rooms []RoomStatus `json:"rooms"`
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
