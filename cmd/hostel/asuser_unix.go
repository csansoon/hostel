//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// asUser makes cmd run as another user (root drops its rights).
func asUser(cmd *exec.Cmd, uid, gid int) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
}
