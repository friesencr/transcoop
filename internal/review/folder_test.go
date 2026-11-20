package review

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/friesencr/transcoop/internal/farm"
)

const folderPreset = `{"PresetList":[{"PresetName":"transcoop-snapshot","FileFormat":"av_mkv","VideoEncoder":"x265_10bit","VideoPreset":"medium","VideoFramerate":"30","PictureDeinterlaceFilter":"decomb","AudioTrackSelectionBehavior":"all","AudioLanguageList":["eng","jpn"],"AudioCopyMask":["copy:ac3","copy:aac"],"AudioEncoderFallback":"none","AudioSecondaryEncoderMode":false,"AudioList":[{"AudioEncoder":"flac16","AudioMixdown":"stereo","AudioSamplerate":"auto","AudioTrackQualityEnable":false,"AudioBitrate":160},{"AudioEncoder":"copy","AudioMixdown":"7point1","AudioSamplerate":"auto","AudioBitrate":224}],"SubtitleAddForeignAudioSearch":true,"SubtitleLanguageList":["eng"]}],"VersionMajor":70,"VersionMinor":0,"VersionMicro":0}`
const folderTitle = `JSON Title Set: {"TitleList":[{"Index":1,"Geometry":{"Width":1280,"Height":720},"FrameRate":{"Num":30,"Den":1},"AudioList":[{"LanguageCode":"eng","CodecName":"ac3","ChannelCount":6,"Name":"Main"},{"LanguageCode":"eng","CodecName":"ac3","ChannelCount":2,"Name":"Commentary"},{"LanguageCode":"jpn","CodecName":"aac","ChannelCount":2,"Name":"Original"}]}]}`

func folderOptions(t *testing.T) Options {
	t.Helper()
	t.Setenv("TRANSCOOP_REVIEW_TEST_FOLDER", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Options{Queue: t.TempDir(), OutputDir: filepath.Join(t.TempDir(), "output"), HandBrake: exe, Preset: "Mkv"}
}
func put(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestFolderDiscoveryAndOutputPaths(t *testing.T) {
	opts := folderOptions(t)
	put(t, filepath.Join(opts.Queue, "B.MP4"))
	put(t, filepath.Join(opts.Queue, "a.mkv"))
	put(t, filepath.Join(opts.Queue, "notes.txt"))
	put(t, filepath.Join(opts.Queue, "nested", "c.avi"))
	plan, err := planFolder(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.files) != 2 || filepath.Base(plan.files[0].source) != "B.MP4" || filepath.Base(plan.files[0].destination) != "B.mkv" {
		t.Fatal(plan.files)
	}
	opts.Recursive = true
	opts.OutputDir = filepath.Join(opts.Queue, "output")
	put(t, filepath.Join(opts.OutputDir, "old.mkv"))
	plan, err = planFolder(context.Background(), opts)
	if err != nil || len(plan.files) != 3 {
		t.Fatalf("%v %v", plan, err)
	}
	if plan.files[2].destination != filepath.Join(opts.OutputDir, "nested", "c.mkv") {
		t.Fatal(plan.files[2])
	}
	if err := os.Symlink(filepath.Join(opts.Queue, "a.mkv"), filepath.Join(opts.Queue, "linked.mkv")); err == nil {
		plan, err = planFolder(context.Background(), opts)
		if err != nil || len(plan.files) != 3 {
			t.Fatalf("symlink followed: %v %v", plan, err)
		}
	}
}
func TestFolderRejectsCollisionsAndExistingOutputs(t *testing.T) {
	opts := folderOptions(t)
	ctx := context.Background()
	put(t, filepath.Join(opts.Queue, "a.mkv"))
	put(t, filepath.Join(opts.Queue, "a.mp4"))
	if _, err := planFolder(ctx, opts); err == nil || !strings.Contains(err.Error(), "multiple inputs") {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(opts.Queue, "a.mp4")); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(opts.OutputDir, "a.mkv"))
	if _, err := planFolder(ctx, opts); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatal(err)
	}
	opts.OutputDir = opts.Queue
	if _, err := planFolder(ctx, opts); err == nil {
		t.Fatal("in-place output allowed")
	}
	opts.OutputDir = ""
	if _, err := planFolder(ctx, opts); err == nil {
		t.Fatal("missing output allowed")
	}
}
func TestFolderPreparationAndAudioReview(t *testing.T) {
	opts := folderOptions(t)
	put(t, filepath.Join(opts.Queue, "a.mkv"))
	plan, err := planFolder(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	e, err := prepareFolderFile(context.Background(), opts.HandBrake, plan.files[0], plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Outputs) != 6 || e.PresetName != "Mkv" || e.Outputs[1].Encoder != "copy:ac3" || e.Outputs[5].Encoder != "copy:aac" {
		t.Fatal(e.Outputs)
	}
	opts.KeepLanguages = "eng,jpn"
	opts.FirstAudio = true
	m := newModel(context.Background(), opts, []*Entry{e})
	if e.Count() != 4 {
		t.Fatal("expected two sources with two outputs each", e.Keep)
	}
	raw, err := e.Export()
	if err != nil {
		t.Fatal(err)
	}
	snapshot, preset, err := farm.ReadPresetSnapshot(raw)
	if err != nil || snapshot.Name != "Mkv" || preset["SubtitleAddForeignAudioSearch"] != true {
		t.Fatalf("snapshot changed: %v %v", snapshot, err)
	}
	dir := t.TempDir()
	args, err := farm.PresetArguments(raw, "local-source", "local-output", dir)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--audio 1,1,3,3") || !strings.Contains(joined, "--aencoder flac16,copy:ac3,flac16,copy:aac") {
		t.Fatal(args)
	}
	e.Keep[4] = false
	e.Keep[5] = false
	if !strings.Contains(m.warning(e), "jpn") {
		t.Fatal("missing Japanese not flagged")
	}
	for i := range e.Keep {
		e.Keep[i] = false
	}
	raw, _ = e.Export()
	args, err = farm.PresetArguments(raw, "source", "out", dir)
	if err != nil || !strings.Contains(strings.Join(args, " "), "--audio none") {
		t.Fatal(args, err)
	}
	saved := filepath.Join(t.TempDir(), "review.json")
	queue, _ := Export([]*Entry{e})
	if err = Save(saved, queue); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(saved)
	if err != nil || reloaded[0].PresetName != "Mkv" {
		t.Fatal(err)
	}
}
func TestFolderProgressErrorsAndCancellation(t *testing.T) {
	opts := folderOptions(t)
	put(t, filepath.Join(opts.Queue, "bad.mkv"))
	put(t, filepath.Join(opts.Queue, "good.mkv"))
	plan, err := planFolder(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(context.Background(), opts, nil)
	m.folder = plan
	cmd := m.Init()
	if !m.busy {
		t.Fatal("scan not busy")
	}
	msg := cmd()
	_, cmd = m.Update(msg)
	if cmd == nil {
		t.Fatal("failed scan stopped rest of batch")
	}
	m.Update(cmd())
	if m.busy || len(m.entries) != 1 || len(m.folderErrors) != 1 || !strings.Contains(strings.Join(m.previewLines(), "\n"), "SKIPPED") {
		t.Fatal(m.status)
	}
	if _, err = os.Stat(opts.OutputDir); !os.IsNotExist(err) {
		t.Fatal("review created encoded outputs")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = prepareFolderFile(ctx, opts.HandBrake, plan.files[1], plan); err == nil {
		t.Fatal("canceled scan succeeded")
	}
}
func TestPresetSelectionModesAndMapping(t *testing.T) {
	opts := folderOptions(t)
	put(t, filepath.Join(opts.Queue, "a.mkv"))
	source := opts.Queue
	coordinator := filepath.Join(t.TempDir(), "coordinator")
	opts.Queue = coordinator
	opts.Mappings = []farm.Mapping{{From: coordinator, To: source}}
	opts.PresetFile = filepath.Join(t.TempDir(), "presets.json")
	put(t, opts.PresetFile)
	plan, err := planFolder(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	// Local paths are resolved (macOS: /var is /private/var).
	physicalSource, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	if plan.files[0].local != filepath.Join(physicalSource, "a.mkv") || plan.files[0].source != filepath.Join(coordinator, "a.mkv") {
		t.Fatal(plan.files[0])
	}
	opts.Preset = ""
	if _, err = planFolder(context.Background(), opts); err == nil {
		t.Fatal("ambiguous preset file accepted")
	}
}
func TestSecondaryAudioAndMissingLanguage(t *testing.T) {
	var export struct{ PresetList []map[string]any }
	_ = json.Unmarshal([]byte(folderPreset), &export)
	p := export.PresetList[0]
	metadata := []AudioInfo{{LanguageCode: "eng", CodecName: "ac3"}, {LanguageCode: "jpn", CodecName: "aac"}}
	p["AudioSecondaryEncoderMode"] = true
	out := presetAudio(p, metadata)
	if len(out) != 3 {
		t.Fatal(out)
	}
	p["AudioLanguageList"] = []any{"spa"}
	out = presetAudio(p, metadata)
	if len(out) != 2 || out[0]["Track"] != 0 {
		t.Fatal("preset fallback", out)
	}
	p["AudioTrackSelectionBehavior"] = "none"
	if len(presetAudio(p, metadata)) != 0 {
		t.Fatal("none ignored")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	if _, err := commandJSON(ctx, "does-not-exist"); err == nil {
		t.Fatal("expected error")
	}
}

func TestPrepareMediaFileAndBatch(t *testing.T) {
	opts := folderOptions(t)
	source := filepath.Join(opts.Queue, "single.mkv")
	put(t, source)
	opts.Queue = source
	queue, err := PrepareMedia(context.Background(), opts, 10)
	if err != nil || len(queue) != 1 {
		t.Fatal(len(queue), err)
	}
	entries, err := Parse(mustJSON(t, queue))
	if err != nil || entries[0].Source != source || entries[0].Destination != filepath.Join(opts.OutputDir, "single.mkv") {
		t.Fatal(entries, err)
	}
	if _, err := os.Stat(opts.OutputDir); !os.IsNotExist(err) {
		t.Fatal("preparation created outputs", err)
	}
	opts.Queue = filepath.Dir(source)
	put(t, filepath.Join(opts.Queue, "other.mp4"))
	if _, err := PrepareMedia(context.Background(), opts, 1); err == nil {
		t.Fatal("ignored batch limit")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PrepareMedia(ctx, opts, 10); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPresetNameParsing(t *testing.T) {
	got := parsePresetNames("[log] ready\nGeneral/\n    Fast 1080p30\n        Description of the preset\n    HQ 720p30\nCustom/\n    My preset\n    Fast 1080p30\n")
	if strings.Join(got, ",") != "Fast 1080p30,HQ 720p30,My preset" {
		t.Fatal(got)
	}
}

func TestFolderKeepsDiscTreesWhole(t *testing.T) {
	opts := folderOptions(t)
	opts.Recursive = true
	dvd := filepath.Join(opts.Queue, "movie.dvd")
	bluray := filepath.Join(opts.Queue, "movie.bluray")
	put(t, filepath.Join(dvd, "VIDEO_TS", "VIDEO_TS.IFO"))
	put(t, filepath.Join(dvd, "VIDEO_TS", "VTS_01_1.VOB"))
	put(t, filepath.Join(bluray, "BDMV", "STREAM", "00001.m2ts"))
	plan, err := planFolder(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.files) != 2 {
		t.Fatalf("split disc source: %#v", plan.files)
	}
	for _, f := range plan.files {
		if f.source != dvd && f.source != bluray {
			t.Fatal(f.source)
		}
	}
	opts.Queue = dvd
	plan, err = planFolder(context.Background(), opts)
	if err != nil || len(plan.files) != 1 || plan.files[0].source != dvd {
		t.Fatal(plan, err)
	}
}
