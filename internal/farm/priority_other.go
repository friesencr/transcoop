//go:build !unix && !windows

package farm

import "os/exec"

func startWithPriority(cmd *exec.Cmd, priority string) error { return cmd.Start() }
