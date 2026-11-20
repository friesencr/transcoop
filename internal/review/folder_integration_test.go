package review

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/friesencr/transcoop/internal/farm"
)

// Opt-in real CLI check: generates its own multilingual media, never uses user
// presets/media, and runs the entire coordinator/worker path in both modes.
func TestRealFolderPresetBothStorageModes(t *testing.T) {
	if os.Getenv("TRANSCOOP_REVIEW_INTEGRATION") != "1" {
		t.Skip("set TRANSCOOP_REVIEW_INTEGRATION=1 with HandBrakeCLI, ffmpeg and ffprobe installed")
	}
	for _, exe := range []string{"HandBrakeCLI", "ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(exe); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dir := t.TempDir()
	input := filepath.Join(dir, "source")
	if err := os.Mkdir(input, 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(input, "episode.mkv")
	english, spanish := filepath.Join(dir, "english.srt"), filepath.Join(dir, "spanish.srt")
	for path, content := range map[string]string{english: "1\n00:00:00,000 --> 00:00:01,800\nEnglish subtitle\n", spanish: "1\n00:00:00,000 --> 00:00:01,800\nSpanish subtitle\n"} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{"-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=24", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-f", "lavfi", "-i", "sine=frequency=550:sample_rate=48000", "-f", "lavfi", "-i", "sine=frequency=660:sample_rate=48000", "-i", english, "-i", spanish, "-map", "4:s", "-map", "5:s", "-map", "0:v", "-map", "1:a", "-map", "2:a", "-map", "3:a", "-t", "2", "-c:v", "libx264", "-preset", "ultrafast", "-threads", "2", "-c:a", "ac3", "-c:a:2", "dca", "-strict", "-2", "-ac:a:2", "2", "-metadata:s:a:0", "language=eng", "-metadata:s:a:0", "title=Main", "-metadata:s:a:1", "language=eng", "-metadata:s:a:1", "title=Commentary", "-metadata:s:a:2", "language=jpn", "-c:s", "srt", "-metadata:s:s:0", "language=eng", "-metadata:s:s:1", "language=spa", source}
	if out, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	raw, err := commandJSON(ctx, "HandBrakeCLI", "--preset", "Fast 1080p30", "--preset-export", "FolderTest")
	if err != nil {
		t.Fatal(err)
	}
	var export map[string]any
	if err = json.Unmarshal(raw, &export); err != nil {
		t.Fatal(err)
	}
	p := obj(export["PresetList"].([]any)[0])
	p["FileFormat"] = "av_mkv"
	p["VideoEncoder"] = "x264"
	p["VideoPreset"] = "ultrafast"
	p["AudioTrackSelectionBehavior"] = "all"
	p["AudioLanguageList"] = []string{"eng", "jpn"}
	p["AudioSecondaryEncoderMode"] = false
	p["AudioCopyMask"] = []string{"copy:ac3", "copy:dts"}
	p["AudioEncoderFallback"] = "none"
	p["SubtitleTrackSelectionBehavior"] = "all"
	p["SubtitleLanguageList"] = []string{"eng"}
	p["SubtitleAddForeignAudioSearch"] = false
	p["SubtitleAddForeignAudioSubtitle"] = false
	p["SubtitleAddCC"] = false
	p["SubtitleBurnBehavior"] = "none"
	p["AudioList"] = []any{map[string]any{"AudioEncoder": "flac16", "AudioMixdown": "stereo", "AudioSamplerate": "auto", "AudioBitrate": 160, "AudioCompressionLevel": 5, "AudioTrackDRCSlider": 0, "AudioTrackGainSlider": 0, "AudioTrackQualityEnable": false}, map[string]any{"AudioEncoder": "copy", "AudioMixdown": "none", "AudioSamplerate": "auto", "AudioBitrate": 160, "AudioTrackQualityEnable": false}}
	presetFile := filepath.Join(dir, "presets.json")
	raw, _ = json.Marshal(export)
	if err = os.WriteFile(presetFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"shared", "transfer"} {
		t.Run(mode, func(t *testing.T) {
			opts := Options{Queue: input, OutputDir: filepath.Join(dir, mode), HandBrake: "HandBrakeCLI", Preset: "FolderTest", PresetFile: presetFile, KeepLanguages: "eng,jpn", FirstAudio: true}
			plan, err := planFolder(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			e, err := prepareFolderFile(ctx, opts.HandBrake, plan.files[0], plan)
			if err != nil {
				t.Fatal(err)
			}
			// Choose audio on a reference file, then apply it to the encode job.
			reference, err := prepareFolderFile(ctx, opts.HandBrake, plan.files[0], plan)
			if err != nil {
				t.Fatal(err)
			}
			m := newModel(ctx, opts, []*Entry{reference})
			if reference.Count() != 4 {
				t.Fatalf("expected two sources/two encodings each: %+v %+v", e.Metadata, e.Outputs)
			}
			if m.warning(reference) != "" {
				t.Fatal(m.warning(reference))
			}
			// Both encodings in shared mode; individual template exclusion in transfer.
			if mode == "transfer" {
				for i, a := range reference.Outputs {
					if a.Encoder == "flac16" {
						reference.Keep[i] = false
					}
				}
			}
			if applied, skipped, err := ApplyAudio(reference, []*Entry{e}); err != nil || applied != 1 || skipped != 0 {
				t.Fatalf("batch audio: applied=%d skipped=%d err=%v", applied, skipped, err)
			}
			// Override the preset: Spanish only in shared mode, no subtitles
			// in transfer mode. Spanish was available but not preset-selected.
			if len(e.Subtitles) != 3 || !e.Subtitles[1].Keep || e.Subtitles[2].Keep {
				t.Fatal(e.Subtitles)
			}
			if err := e.editSubtitle(1, "keep"); err != nil {
				t.Fatal(err)
			}
			if mode == "shared" {
				if err := e.editSubtitle(2, "default"); err != nil {
					t.Fatal(err)
				}
			}
			queue, err := Export([]*Entry{e})
			if err != nil {
				t.Fatal(err)
			}
			store, err := farm.OpenStore(filepath.Join(t.TempDir(), "farm.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := store.InstallAdmin(context.Background(), "0123456789abcdef0123456789abcdef"); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer((&farm.Server{Store: store}).Handler())
			defer server.Close()
			if _, err = store.Import(ctx, queue); err != nil {
				t.Fatal(err)
			}
			// Old worker claims must not receive the new payload format.
			if job, err := store.Claim(ctx, "old-worker", []string{"x264"}, time.Minute); err != nil || job != nil {
				t.Fatalf("old worker claimed preset job: %v %v", job, err)
			}
			workerCtx, stop := context.WithCancel(ctx)
			defer stop()
			worker := farm.Worker{Client: farm.NewClient(server.URL, "0123456789abcdef0123456789abcdef"), Name: "folder-test", Mode: mode, HandBrake: "HandBrakeCLI", WorkDir: filepath.Join(t.TempDir(), "work"), MaxJobs: 1, Encoders: []string{"x264"}}
			done := make(chan error, 1)
			go func() { done <- worker.Run(workerCtx) }()
			defer func() {
				stop()
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			for {
				jobs, err := store.List(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if jobs[0].Status == "succeeded" {
					break
				}
				if jobs[0].Status == "failed" {
					logs, _ := filepath.Glob(filepath.Join(worker.WorkDir, "*", "handbrake.log"))
					for _, log := range logs {
						data, _ := os.ReadFile(log)
						t.Log(string(data))
					}
					t.Fatalf("encode: %s", jobs[0].Error)
				}
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(50 * time.Millisecond):
				}
			}
			data, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "a", "-show_entries", "stream=codec_name:stream_tags=language", "-of", "json", e.Destination).Output()
			if err != nil {
				t.Fatal(err)
			}
			var probe struct {
				Streams []struct {
					CodecName string `json:"codec_name"`
					Tags      map[string]string
				}
			}
			if err = json.Unmarshal(data, &probe); err != nil {
				t.Fatal(err)
			}
			want := []string{"eng:flac", "eng:ac3", "jpn:flac", "jpn:dts"}
			if mode == "transfer" {
				want = []string{"eng:ac3", "jpn:dts"}
			}
			got := []string{}
			for _, s := range probe.Streams {
				got = append(got, s.Tags["language"]+":"+s.CodecName)
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("actual output audio %v != reviewed %v", got, want)
			}
			subData, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "s", "-show_entries", "stream=codec_name:stream_tags=language:stream_disposition=default,forced", "-of", "json", e.Destination).Output()
			if err != nil {
				t.Fatal(err)
			}
			var subs struct {
				Streams []struct {
					CodecName   string `json:"codec_name"`
					Tags        map[string]string
					Disposition struct{ Default, Forced int }
				}
			}
			if err := json.Unmarshal(subData, &subs); err != nil {
				t.Fatal(err)
			}
			// HandBrake also sets the MKV forced disposition for default tracks;
			// it is distinct from the job's forced-only filtering setting.
			if mode == "shared" {
				if len(subs.Streams) != 1 || subs.Streams[0].Tags["language"] != "spa" || subs.Streams[0].Disposition.Default != 1 {
					logs, _ := filepath.Glob(filepath.Join(worker.WorkDir, "*", "handbrake.log"))
					if len(logs) > 0 {
						data, _ := os.ReadFile(logs[0])
						t.Log(string(data))
					}
					t.Fatalf("subtitle override lost: %s", subData)
				}
			} else if len(subs.Streams) != 0 {
				t.Fatalf("preset restored unwanted subtitles: %s", subData)
			}
			logs, _ := filepath.Glob(filepath.Join(worker.WorkDir, "*", "handbrake.log"))
			data, _ = os.ReadFile(logs[0])
			if !strings.Contains(string(data), "Progress:") {
				t.Fatal("missing JSON progress")
			}
		})
	}
}
