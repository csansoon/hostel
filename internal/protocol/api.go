package protocol

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
	Listen string `json:"listen"`
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
