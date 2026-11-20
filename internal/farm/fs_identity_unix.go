//go:build !windows

package farm

import (
	"fmt"
	"os"
	"syscall"
)

func fsIdentity(f *os.File) (string, error) {
	i, e := f.Stat()
	if e != nil {
		return "", e
	}
	s := i.Sys().(*syscall.Stat_t)
	return fmt.Sprintf("%d:%d", s.Dev, s.Ino), nil
}
