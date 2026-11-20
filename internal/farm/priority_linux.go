package farm

import (
	"os/exec"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	ioprioClassBE   = 2
	ioprioClassIdle = 3
	ioprioWhoThread = 1 // IOPRIO_WHO_PROCESS with id 0: the calling thread
)

// startWithPriority starts cmd at a lower CPU and I/O priority. Linux applies
// both per thread and children inherit them, so a dedicated OS thread lowers
// its own priority and forks HandBrake. The goroutine never unlocks the thread,
// so the runtime discards it instead of reusing a lowered thread elsewhere.
func startWithPriority(cmd *exec.Cmd, priority string) error {
	nice := niceness(priority)
	if nice == 0 {
		return cmd.Start()
	}
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		// Fails only when the worker already runs at a lower priority than
		// requested; the child then simply inherits that.
		_ = unix.Setpriority(unix.PRIO_PROCESS, syscall.Gettid(), nice)
		io := ioprioClassBE<<13 | 7 // lowest best-effort level
		if priority == PriorityIdle {
			io = ioprioClassIdle << 13
		}
		_, _, _ = unix.Syscall(unix.SYS_IOPRIO_SET, ioprioWhoThread, 0, uintptr(io)) // best effort
		done <- cmd.Start()
	}()
	return <-done
}
