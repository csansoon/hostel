//go:build !windows

package main

import "os"

// enableVT: terminals outside Windows take VT sequences.
func enableVT(*os.File) error { return nil }
