package main

import "os/exec"

// asUser is not used on Windows (the lease user service deletes files as SYSTEM).
func asUser(*exec.Cmd, int, int) {}
