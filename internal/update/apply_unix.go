//go:build !windows

package update

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

func apply(ctx context.Context, work, target, stage, installed, next string) (keep bool, err error) {
	backup := filepath.Join(work, "previous")
	// Retain the original inode and permissions until the replacement passes.
	if err := os.Link(target, backup); err != nil {
		// Some removable filesystems do not support hard links.
		old, err := os.Open(target)
		if err != nil {
			return false, err
		}
		defer old.Close()
		info, err := old.Stat()
		if err != nil {
			return false, err
		}
		if err := writeBinary(backup, old); err != nil {
			return false, err
		}
		if err := os.Chmod(backup, info.Mode().Perm()); err != nil {
			return false, err
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := os.Rename(stage, target); err != nil {
		return false, err
	}
	if err := probe(ctx, target, next); err != nil {
		if restore := os.Rename(backup, target); restore != nil {
			return true, fmt.Errorf("new binary failed (%v); restore %s manually: %w", err, backup, restore)
		}
		return false, fmt.Errorf("restored %s after failed version check: %w", installed, err)
	}
	fmt.Println("Installed", next, "— restart any Transcoop services to use it.")
	return false, nil
}
