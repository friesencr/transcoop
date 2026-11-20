//go:build unix

package farm

import (
	"os/exec"
	"syscall"
)

// isolateHelper keeps terminal signals (Ctrl+C) away from the FUSE helper so
// the worker can stop HandBrake, return jobs and then unmount, in that order.
func isolateHelper(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
