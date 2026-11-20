package farm

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/friesencr/transcoop/internal/fsproto"
)

// This gate is deliberately opt-in: a skipped test is not native validation.
// Run with the native transcoop-fs binary on PATH and actual installed drivers.
func TestFuseNativeHandBrake(t *testing.T) {
	if os.Getenv("TRANSCOOP_FUSE_NATIVE") != "1" {
		t.Skip("native driver/HandBrake gate: set TRANSCOOP_FUSE_NATIVE=1 on Linux, macOS, and Windows")
	}
	hb, err := exec.LookPath("HandBrakeCLI")
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err = ProbeFuse(ctx, work); err != nil {
		t.Fatal(err)
	}
	s := newStore(t)
	enrollment := enroll(t, s, "worker")
	server := httptest.NewServer((&Server{Store: s}).Handler())
	defer server.Close()
	client := NewClient(server.URL, enrollment.Token)
	helper, err := startFuse(ctx, fsproto.Settings{}.Defaults(work), client)
	if err != nil {
		t.Fatal(err)
	}
	defer helper.close()
	source, err := filepath.Abs(filepath.Join("..", "..", "cmd", "transcoop", "testdata", "encoder-probe.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	var jobs []Job
	for i := 0; i < 4; i++ {
		ext := ".mkv"
		if i%2 == 1 {
			ext = ".mp4"
		}
		dest := filepath.Join(work, fmtID(int64(i))+ext)
		raw, _ := json.Marshal(map[string]any{"Job": map[string]any{"Source": map[string]any{"Path": source}, "Destination": map[string]any{"File": dest}, "Video": map[string]any{"Encoder": "x264"}}})
		if _, err = s.Import(ctx, []json.RawMessage{raw}); err != nil {
			t.Fatal(err)
		}
		j, e := s.claimMatching(ctx, "native", []string{"x264"}, 5*time.Minute, enrollment.Device.ID)
		if e != nil {
			t.Fatal(e)
		}
		jobs = append(jobs, *j)
	}
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j Job) {
			defer wg.Done()
			ns := "native-" + fmtID(j.ID)
			reply, e := helper.call(fsproto.Control{Kind: "add", Namespace: ns, JobID: j.ID, Attempt: j.Token, Deadline: j.LeaseUntil})
			if e != nil {
				t.Error(e)
				return
			}
			defer helper.call(fsproto.Control{Kind: "remove", Namespace: ns})
			root := filepath.Join(helper.settings.MountDir, ns)
			input := filepath.Join(root, "inputs", reply.Manifest.Source)
			output := filepath.Join(root, "output", reply.Manifest.Output)
			cmd := exec.CommandContext(ctx, hb, "--input", input, "--output", output, "--encoder", "x264", "--encoder-preset", "ultrafast", "--audio", "none", "--min-duration", "0")
			if log, e := cmd.CombinedOutput(); e != nil {
				t.Errorf("HandBrake: %v: %s", e, log)
				return
			}
			if _, e = os.Stat(j.Destination); !os.IsNotExist(e) {
				t.Error("final output visible before completion")
				return
			}
			if _, e = helper.call(fsproto.Control{Kind: "seal", Namespace: ns}); e != nil {
				t.Error(e)
				return
			}
			if e = s.Finish(ctx, j.ID, j.Token, "", publish); e != nil {
				t.Error(e)
				return
			}
			if log, e := exec.CommandContext(ctx, hb, "--scan", "--input", j.Destination, "--min-duration", "0").CombinedOutput(); e != nil {
				t.Errorf("completed output scan: %v: %s", e, log)
			}
		}(j)
	}
	wg.Wait()
}
