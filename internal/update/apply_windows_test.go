package update

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/friesencr/transcoop/internal/filelock"
)

func TestWindowsHelperRestoresFailedReplacement(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "transcoop.exe")
	work, err := os.MkdirTemp(dir, "staged-")
	if err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(work, "transcoop.exe")
	original := []byte("original executable bytes")
	if err := os.WriteFile(target, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stage, []byte("cannot execute this replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	unlock, err := filelock.Lock(target + ".update.lock")
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	// A nonexistent PID represents a launcher that has already exited.
	cmd, err := replacementCommand(target, stage, "v1.0.1", 2147483647)
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "Restored the previous executable") {
		t.Fatalf("helper returned %v: %s", err, out)
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("original bytes were not restored: %v", err)
	}
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Fatalf("expected helper to clean up staging: %v", err)
	}
}
