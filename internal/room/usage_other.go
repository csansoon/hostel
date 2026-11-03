//go:build !windows

package room

import "github.com/csansoon/hostel/internal/protocol"

// windowsUsage: only the Windows build reads it.
func windowsUsage(*protocol.Usage) {}
