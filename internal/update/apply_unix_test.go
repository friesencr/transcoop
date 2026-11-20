//go:build !windows

package update

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRestoreAfterFailedVersionProbe(t *testing.T) {
	work := t.TempDir()
	target := filepath.Join(t.TempDir(), "transcoop")
	if err := os.WriteFile(target, []byte("original"), 0755); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(work, "new")
	if err := os.WriteFile(stage, []byte("broken executable"), 0755); err != nil {
		t.Fatal(err)
	}
	if keep, err := apply(t.Context(), work, target, stage, "v1.0.0", "v1.0.1"); err == nil || keep {
		t.Fatalf("keep=%v err=%v", keep, err)
	}
	b, err := os.ReadFile(target)
	if err != nil || string(b) != "original" {
		t.Fatalf("original was not restored: %q, %v", b, err)
	}
}
