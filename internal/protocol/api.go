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
