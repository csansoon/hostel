//go:build !windows

package helper

import "github.com/csansoon/hostel/internal/protocol"

// windowsHardware: only the Windows build reads it.
func windowsHardware(*protocol.Hardware) {}
