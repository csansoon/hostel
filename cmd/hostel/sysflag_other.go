//go:build !darwin

package main

import "os"

func isSystemDir(os.FileInfo) bool { return false }
