package webui

import (
	"testing"

	"github.com/friesencr/transcoop/internal/farm"
)

func TestFormatting(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{baseName("/media/in/clip 1.mkv"), "clip 1.mkv"},
		{dirName("/media/in/clip 1.mkv"), "/media/in"},
		{baseName(`C:\media\clip.mkv`), "clip.mkv"},
		{dirName("clip.mkv"), ""},
		{duration(13), "13s"},
		{duration(252), "4m 12s"},
		{duration(3725), "1h 02m"},
		{took(100, 113), "13s"},
		{took(0, 113), ""},
		{byteSize(-1), "—"},
		{byteSize(1536), "2 KiB"},
		{byteSize(410 << 20), "410 MiB"},
		{byteSize(3 << 29), "1.50 GiB"},
		{sizeChange(1000, 340), "−66%"},
		{sizeChange(1000, 1200), "+20%"},
		{sizeChange(-1, 10), ""},
		{navSection("Job 12"), "/jobs"},
		{navSection("Preview jobs"), "/add"},
		{firstLine("boom\nmore"), "boom"},
	} {
		if c.got != c.want {
			t.Errorf("got %q want %q", c.got, c.want)
		}
	}
	hw, sw := splitEncoders([]string{"x264", "vce_h265", "qsv_av1", "svt_av1", "nvenc_h264", "vt_h265"})
	if len(hw) != 4 || len(sw) != 2 {
		t.Fatal(hw, sw)
	}
	s := slots(farm.WorkerStatus{MaxJobs: 3, Active: []int64{7}})
	if len(s) != 3 || s[0] != 7 || s[2] != 0 {
		t.Fatal(s)
	}
	for _, p := range []string{"/jobs", "/jobs/1?x=y"} {
		if !localPath(p) {
			t.Error("rejected", p)
		}
	}
	for _, p := range []string{"", "jobs", "//x", "/\\x", "https://x", "/a\nb"} {
		if localPath(p) {
			t.Error("accepted", p)
		}
	}
}
