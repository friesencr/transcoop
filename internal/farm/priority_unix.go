//go:build unix && !linux

package farm

import (
	"os/exec"
	"syscall"
)

// startWithPriority starts cmd, then lowers its process priority. On BSD and
// macOS the nice value applies to the whole process.
func startWithPriority(cmd *exec.Cmd, priority string) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	if nice := niceness(priority); nice != 0 {
		_ = syscall.Setpriority(syscall.PRIO_PROCESS, cmd.Process.Pid, nice) // best effort
	}
	return nil
}
