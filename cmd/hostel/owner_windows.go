package main

import "os"

// fileOwner is not used on Windows (secureServiceDir checks the owner with
// PowerShell).
func fileOwner(os.FileInfo) (uint32, bool) { return 0, false }
