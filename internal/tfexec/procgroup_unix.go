// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

//go:build unix

package tfexec

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup makes cmd the leader of a new process group, so a shell
// wrapper and everything it spawns can be interrupted together.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// interrupt sends SIGINT to cmd — to its whole process group when
// ownProcessGroup gave it one.
func interrupt(cmd *exec.Cmd) error {
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Setpgid {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
	}
	return cmd.Process.Signal(syscall.SIGINT)
}
