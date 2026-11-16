//go:build !darwin

package main

import "os"

func isSystemDir(string, os.FileInfo) bool { return false }
