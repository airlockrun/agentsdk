//go:build !windows

package main

import (
	"errors"
	"os/exec"
	"syscall"
)

func configureManifestProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateManifestDescendants(command *exec.Cmd) {
	if command.Process == nil {
		return
	}
	if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return
	}
}
