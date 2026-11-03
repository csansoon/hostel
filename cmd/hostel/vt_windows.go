package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableVT turns on VT sequences in the Windows console (Windows 10 and
// later); without them the view is not possible.
func enableVT(f *os.File) error {
	h := windows.Handle(f.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return err
	}
	return windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
}
