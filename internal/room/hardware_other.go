//go:build !windows

package room

import "github.com/csansoon/hostel/internal/protocol"

// windowsHardware: only the Windows build reads it.
func windowsHardware(*protocol.Hardware) {}
