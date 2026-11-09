//go:build !windows

package helper

import "github.com/csansoon/hostel/internal/protocol"

// windowsUsage: only the Windows build reads it.
func windowsUsage(*protocol.Usage) {}
