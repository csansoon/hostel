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

// Room states.
const (
	RoomCreating     = "creating"
	RoomProvisioning = "provisioning"
	RoomRunning      = "running"
	RoomStopping     = "stopping"
	RoomStopped      = "stopped"
	RoomResuming     = "resuming"
	RoomDestroying   = "destroying"
	RoomDestroyed    = "destroyed"
	RoomBroken       = "broken"
	RoomRetired      = "retired"
)

// Slot states.
const (
	SlotFree      = "free"
	SlotAssigned  = "assigned"
	SlotPreparing = "preparing"
	SlotLeased    = "leased"
	SlotCleaning  = "cleaning"
	SlotBroken    = "broken"
	SlotDirty     = "dirty"
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
