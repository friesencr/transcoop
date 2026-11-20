//go:build windows

package farm

import (
	"os/exec"
	"syscall"
)

// isolateHelper keeps console Ctrl+C away from the FUSE helper so the worker
// can stop HandBrake, return jobs and then unmount, in that order. A new
// process group does not receive Ctrl+C from the console it shares.
func isolateHelper(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
