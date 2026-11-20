package farm

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/friesencr/transcoop/internal/fsproto"
)

func TestParseMountInfo(t *testing.T) {
	line := `612 30 0:91 / /home/me/My\040Work/mount rw,nosuid,nodev,relatime shared:350 - fuse transcoop rw,user_id=1000,group_id=1000`
	mp, src, ok := parseMountInfo(line)
	if !ok || mp != "/home/me/My Work/mount" || src != "transcoop" {
		t.Fatal(mp, src, ok)
	}
	if _, _, ok := parseMountInfo("garbage"); ok {
		t.Fatal("parsed garbage")
	}
}

// A worker that is killed, or whose helper is, leaves a FUSE mount with no
// daemon. The next start must clear it rather than refuse to run.
func TestFuseNativeStaleMountRecovery(t *testing.T) {
	if os.Getenv("TRANSCOOP_FUSE_NATIVE") != "1" {
		t.Skip("native driver gate: set TRANSCOOP_FUSE_NATIVE=1")
	}
	work := t.TempDir()
	settings := fsproto.Settings{}.Defaults(work)
	ctx, cancel := context.WithCancel(context.Background())
	h, err := startFuse(ctx, settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Cancelling the worker's context must not kill the helper.
	cancel()
	time.Sleep(300 * time.Millisecond)
	select {
	case <-h.done:
		t.Fatal("helper died with the worker context; its mount would be stranded")
	default:
	}
	// Simulate a crash: kill the helper without unmounting.
	if err = h.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	<-h.done
	for i := len(h.unlock) - 1; i >= 0; i-- {
		h.unlock[i]()
	}
	if _, err = os.Stat(filepath.Join(settings.MountDir, ".")); err == nil {
		t.Log("mount directory still readable after kill; kernel may have cleaned up")
	}
	h2, err := startFuse(context.Background(), settings, nil)
	if err != nil {
		t.Fatal("restart after a stale mount:", err)
	}
	if err = h2.close(); err != nil {
		t.Fatal(err)
	}
	if cleared, _ := clearStaleMount(settings.MountDir); cleared {
		t.Fatal("close left a mount behind")
	}
}
