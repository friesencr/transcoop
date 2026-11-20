package update

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/windows"
)

//go:embed replace_windows.ps1
var replaceScript string

func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func replacementCommand(target, stage, next string, parent int) (*exec.Cmd, error) {
	original, err := fileHash(target)
	if err != nil {
		return nil, err
	}
	replacement, err := fileHash(stage)
	if err != nil {
		return nil, err
	}
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return nil, err
	}
	ps := filepath.Join(system, "WindowsPowerShell", "v1.0", "powershell.exe")
	cmd := exec.Command(ps, "-NoProfile", "-NonInteractive", "-Command", replaceScript)
	cmd.Env = append(os.Environ(),
		"TRANSCOOP_TARGET="+target, "TRANSCOOP_STAGE="+stage, "TRANSCOOP_VERSION="+next,
		"TRANSCOOP_ORIGINAL_HASH="+original, "TRANSCOOP_STAGE_HASH="+replacement,
		"TRANSCOOP_PARENT_PID="+strconv.Itoa(parent))
	return cmd, nil
}

func apply(ctx context.Context, work, target, stage, installed, next string) (keep bool, err error) {
	cmd, err := replacementCommand(target, stage, next, os.Getpid())
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return false, err
	}
	fmt.Println("Update staged. Waiting for Transcoop to exit; the Windows helper will report the result.")
	// From this point the helper owns the staged files, including on failure.
	return true, cmd.Process.Release()
}
