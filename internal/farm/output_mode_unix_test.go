//go:build unix

package farm

import (
	"os"
	"syscall"
	"testing"
)

func TestOutputFilesHonourUmask(t *testing.T) {
	for _, tc := range []struct {
		umask int
		want  os.FileMode
	}{{0o022, 0o644}, {0o077, 0o600}} {
		previous := syscall.Umask(tc.umask)
		f, err := createOutputTemp(t.TempDir(), ".transcoop-upload-")
		syscall.Umask(previous)
		if err != nil {
			t.Fatal(err)
		}
		info, err := f.Stat()
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != tc.want {
			t.Errorf("umask %03o: mode %03o, want %03o", tc.umask, got, tc.want)
		}
	}
}
