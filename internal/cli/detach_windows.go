package cli

import (
	"os/exec"
	"syscall"
)

// detachedProcess is DETACHED_PROCESS, which package syscall does not export.
const detachedProcess = 0x00000008

// detach gives the child its own process group and no console, so it survives
// the restarting shell closing and a Ctrl+C aimed at that shell.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess,
	}
}
