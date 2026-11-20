package farm

import (
	"os/exec"
	"syscall"
)

const (
	belowNormalPriorityClass = 0x00004000
	idlePriorityClass        = 0x00000040
)

// startWithPriority starts cmd in a lower Windows priority class.
func startWithPriority(cmd *exec.Cmd, priority string) error {
	var class uint32
	switch priority {
	case PriorityLow:
		class = belowNormalPriorityClass
	case PriorityIdle:
		class = idlePriorityClass
	}
	if class != 0 {
		if cmd.SysProcAttr == nil {
			cmd.SysProcAttr = &syscall.SysProcAttr{}
		}
		cmd.SysProcAttr.CreationFlags |= class
	}
	return cmd.Start()
}
