// Package protocol has the types that the server, the guest, and the helper share.
package protocol

// Lease states (design, section 4).
const (
	LeaseQueued       = "queued"
	LeaseAssigned     = "assigned"
	LeaseProvisioning = "provisioning"
	LeasePreparing    = "preparing"
	LeaseReady        = "ready"
	LeaseCleaning     = "cleaning"
	LeaseReleased     = "released"
	LeaseExpired      = "expired"
	LeaseCancelled    = "cancelled"
	LeaseFailed       = "failed"
)

// Machine states.
const (
	MachineCreating     = "creating"
	MachineProvisioning = "provisioning"
	MachineRunning      = "running"
	MachineStopping     = "stopping"
	MachineStopped      = "stopped"
	MachineResuming     = "resuming"
	MachineDestroying   = "destroying"
	MachineDestroyed    = "destroyed"
	MachineBroken       = "broken"
	MachineRetired      = "retired"
)

// Room states.
const (
	RoomFree      = "free"
	RoomAssigned  = "assigned"
	RoomPreparing = "preparing"
	RoomLeased    = "leased"
	RoomCleaning  = "cleaning"
	RoomBroken    = "broken"
	RoomDirty     = "dirty"
)

// Priorities. The effective priority of a queued lease grows with its wait time
// up to PriorityHigh.
const (
	PriorityLow    = 0
	PriorityNormal = 1
	PriorityHigh   = 2
)

// PriorityNames maps the CLI and API names to priorities.
var PriorityNames = map[string]int{"low": PriorityLow, "normal": PriorityNormal, "high": PriorityHigh}
