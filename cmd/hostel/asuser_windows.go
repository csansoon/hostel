package main

import "os/exec"

// asUser is not used on Windows (lease users are for macOS and Linux).
func asUser(*exec.Cmd, int, int) {}
