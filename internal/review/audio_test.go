package review

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/friesencr/transcoop/internal/farm"
)

func TestDTSScanPresetOutputs(t *testing.T) {
	// Real HandBrake scan: two English AC3 sources, Japanese DTS-HD MA and
	// DTS sources. Both Japanese decoder names are "dca".
	data, err := os.ReadFile("testdata/dts-scan.json")
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := ParseScan(data, 1)
	if err != nil {
		t.Fatal(err)
	}
	var export struct{ PresetList []map[string]any }
	if err := json.Unmarshal([]byte(folderPreset), &export); err != nil {
		t.Fatal(err)
	}
	p := export.PresetList[0]
	p["AudioCopyMask"] = []any{"copy:ac3", "copy:dts", "copy:dtshd"}
	outputs := presetAudio(p, metadata)
	want := []string{"flac16", "copy:ac3", "flac16", "copy:ac3", "flac16", "copy:dtshd", "flac16", "copy:dts"}
	if len(outputs) != len(want) {
		t.Fatalf("got %d outputs, want %d: %v", len(outputs), len(want), outputs)
	}
	for i, a := range outputs {
		if a["Track"] != i/2 || a["Encoder"] != want[i] {
			t.Fatalf("output %d: %v", i, a)
		}
	}
	// Exercise queue parsing, the review display, first-per-language selection,
	// export, and worker arguments so the passthrough survives the whole path.
	presetExport, _ := json.Marshal(export)
	raw, _ := json.Marshal([]any{map[string]any{
		"Title": map[string]any{"AudioList": metadata},
		"Job": map[string]any{
			"Source":      map[string]any{"Path": "/source.mkv", "Title": 1},
			"Destination": map[string]any{"File": "/out.mkv"},
			"Video":       map[string]any{"Encoder": p["VideoEncoder"]},
			"Audio":       map[string]any{"AudioList": outputs},
		},
		"TranscoopPreset": farm.PresetSnapshot{Version: 1, Name: "Mkv", Export: presetExport},
	}})
	entries, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	e := entries[0]
	m := newModel(context.Background(), Options{FirstAudio: true}, entries)
	if e.Count() != 4 || !strings.Contains(m.audioLabel(e, 5), "dtshd 2ch") || !strings.Contains(m.audioLabel(e, 7), "dts 2ch") {
		t.Fatalf("review lost DTS metadata/outputs: %v, %s, %s", e.Keep, m.audioLabel(e, 5), m.audioLabel(e, 7))
	}
	job, err := e.Export()
	if err != nil {
		t.Fatal(err)
	}
	args, err := farm.PresetArguments(job, "source", "out", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--audio 1,1,3,3") || !strings.Contains(joined, "--aencoder flac16,copy:ac3,flac16,copy:dtshd") {
		t.Fatal(joined)
	}
	if removed, skipped := Apply(entries, Rule{Kind: "codec", Value: "dtshd"}); removed != 2 || skipped != 0 {
		t.Fatalf("codec filter: removed=%d skipped=%d", removed, skipped)
	}
}

func TestDTSPassthroughResolution(t *testing.T) {
	for _, tc := range []struct {
		name, encoder, mask, fallback, want string
		info                                AudioInfo
	}{
		{"DTS decoder alias", "copy", "copy:dts", "none", "copy:dts", AudioInfo{CodecName: "dca", Codec: 8192}},
		{"HD decoder alias", "copy", "copy:dtshd", "none", "copy:dtshd", AudioInfo{CodecName: "dca", Codec: 262144}},
		{"explicit HD", "copy:dtshd", "", "none", "copy:dtshd", AudioInfo{CodecName: "dca", Codec: 262144}},
		{"HD disabled", "copy", "copy:dts", "flac16", "flac16", AudioInfo{CodecName: "dca", Codec: 262144}},
		{"DTS disabled", "copy", "copy:dtshd", "none", "none", AudioInfo{CodecName: "dca", Codec: 8192}},
		{"HD cannot pass DTS source", "copy:dtshd", "", "none", "none", AudioInfo{CodecName: "dca", Codec: 8192}},
		{"HD without numeric metadata", "copy", "copy:dtshd", "none", "copy:dtshd", AudioInfo{CodecName: "dca", Description: "Japanese (DTS-HD MA, 2.0 ch)"}},
		{"DTS without numeric metadata", "copy", "copy:dts", "none", "copy:dts", AudioInfo{CodecName: "dca"}},
		{"track name cannot change codec", "copy", "copy:dts", "none", "copy:dts", AudioInfo{CodecName: "dca", Codec: 8192, Name: "DTS-HD MA"}},
		{"FLAC template unchanged", "flac16", "copy:dtshd", "none", "flac16", AudioInfo{CodecName: "dca", Codec: 262144}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := map[string]any{"AudioCopyMask": []any{tc.mask}, "AudioEncoderFallback": tc.fallback}
			if got := resolveAudioEncoder(tc.encoder, tc.info, p); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// The destination uses a different source order, codec and existing mixdown.
func reorderedAudio(t *testing.T) *Entry {
	t.Helper()
	e := fixture(t)[0]
	var root map[string]any
	if err := json.Unmarshal(e.Raw, &root); err != nil {
		t.Fatal(err)
	}
	metadata := root["Title"].(map[string]any)["AudioList"].([]any)
	metadata[0].(map[string]any)["CodecName"] = "aac"
	root["Title"].(map[string]any)["AudioList"] = []any{metadata[2], metadata[1], metadata[0]}
	job := root["Job"].(map[string]any)
	job["Source"].(map[string]any)["Path"] = "/media/b.mkv"
	outputs := job["Audio"].(map[string]any)["AudioList"].([]any)
	for _, v := range outputs {
		a := v.(map[string]any)
		a["Track"] = []int{2, 1, 0}[int(a["Track"].(float64))]
		a["Mixdown"] = "5point1"
		a["Bitrate"] = 256
		if a["Encoder"] == "copy:ac3" {
			a["Encoder"] = "copy:aac"
		}
		a["Name"] = "Destination track name"
	}
	// Also put the Japanese output before English outputs.
	job["Audio"].(map[string]any)["AudioList"] = []any{outputs[3], outputs[0], outputs[1], outputs[2]}
	raw, _ := json.Marshal([]any{root})
	entries, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return entries[0]
}

func TestBatchAudioMatchesLanguageRoleAndEncoder(t *testing.T) {
	ref, target := fixture(t)[0], reorderedAudio(t)
	ref.Keep[2] = false // No commentary, even though it is also English.
	before := string(target.Raw)
	applied, skipped, err := ApplyAudio(ref, []*Entry{ref, target})
	if err != nil || applied != 1 || skipped != 0 {
		t.Fatalf("%d %d %v", applied, skipped, err)
	}
	if !reflect.DeepEqual(target.Keep, []bool{true, true, true, false}) {
		t.Fatal(target.Keep)
	}
	if target.Outputs[2].Encoder != "copy:aac" || target.Outputs[2].Source != 2 {
		t.Fatal(target.Outputs[2])
	}
	var settings map[string]any
	json.Unmarshal(target.Outputs[1].Raw, &settings)
	if settings["Mixdown"] != "stereo" || settings["Quality"] != float64(-3) || settings["Bitrate"] != nil || settings["Name"] != "Destination track name" {
		t.Fatal(settings)
	}
	if string(target.Raw) != before {
		t.Fatal("original payload mutated")
	}
	raw, err := target.Export()
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := Parse(append(append([]byte{'['}, raw...), ']'))
	if err != nil || reloaded[0].Count() != 3 {
		t.Fatalf("%v %v", reloaded, err)
	}
	var oldRoot, newRoot map[string]json.RawMessage
	json.Unmarshal(target.Raw, &oldRoot)
	json.Unmarshal(raw, &newRoot)
	var oldJob, newJob map[string]json.RawMessage
	json.Unmarshal(oldRoot["Job"], &oldJob)
	json.Unmarshal(newRoot["Job"], &newJob)
	for _, k := range []string{"Video", "Subtitle", "Source", "Destination"} {
		if string(oldJob[k]) != string(newJob[k]) {
			t.Fatalf("changed %s", k)
		}
	}
}

func TestBatchAudioExportsSettingsWhenAllOutputsKept(t *testing.T) {
	ref, target := fixture(t)[0], reorderedAudio(t)
	_, _, err := ApplyAudio(ref, []*Entry{target})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := target.Export()
	if err != nil || string(raw) == string(target.Raw) || !strings.Contains(string(raw), `"Mixdown":"stereo"`) {
		t.Fatalf("settings lost on export: %s %v", raw, err)
	}
}

func TestBatchAudioSkipsMissingMatchesAtomically(t *testing.T) {
	for _, kind := range []string{"language", "metadata", "encoder", "commentary"} {
		t.Run(kind, func(t *testing.T) {
			ref, target := fixture(t)[0], reorderedAudio(t)
			switch kind {
			case "language":
				target.Metadata[0].LanguageCode = "fra"
			case "metadata":
				target.Metadata = nil
			case "encoder":
				target.Outputs[0].Encoder = "av_aac"
			case "commentary":
				target.Metadata[2].Attributes.Commentary = true
			}
			before := append([]Output(nil), target.Outputs...)
			applied, skipped, err := ApplyAudio(ref, []*Entry{target})
			if err != nil || applied != 0 || skipped != 1 || target.AudioWarning == "" {
				t.Fatalf("%d %d %v %s", applied, skipped, err, target.AudioWarning)
			}
			if !reflect.DeepEqual(before, target.Outputs) || target.Count() != 4 {
				t.Fatal("partially changed skipped file")
			}
		})
	}
	ref, target := fixture(t)[0], reorderedAudio(t)
	ref.Metadata = nil
	if _, _, err := ApplyAudio(ref, []*Entry{target}); err == nil {
		t.Fatal("accepted reference without metadata")
	}
}

func TestBatchAudioRepeatedEncoderAndNoAudio(t *testing.T) {
	ref, target := fixture(t)[0], fixture(t)[0]
	for _, e := range []*Entry{ref, target} {
		e.Outputs = append(e.Outputs, e.Outputs[0])
		e.Keep = append(e.Keep, true)
	}
	ref.Keep[0] = false
	_, _, err := ApplyAudio(ref, []*Entry{target})
	if err != nil || !reflect.DeepEqual(ref.Keep, target.Keep) {
		t.Fatal(target.Keep, err)
	}
	for i := range ref.Keep {
		ref.Keep[i] = false
	}
	target.Metadata = nil
	_, _, err = ApplyAudio(ref, []*Entry{target})
	if err != nil || target.Count() != 0 {
		t.Fatal(target.Keep, err)
	}
}

func TestBatchAudioUISelectionUndoResetAndWarnings(t *testing.T) {
	ref, target, hidden, unselected := fixture(t)[0], reorderedAudio(t), fixture(t)[0], fixture(t)[0]
	hidden.Source, hidden.Destination = "/hidden/file", "/hidden/out"
	unselected.Selected = false
	ref.Keep[2] = false
	m := newModel(context.Background(), Options{}, []*Entry{ref, target, hidden, unselected})
	m.filter = "/media/"
	before := append([]Output(nil), target.Outputs...)
	m.Update(key("b"))
	if target.Keep[3] || !hidden.Keep[2] || !unselected.Keep[2] {
		t.Fatal("wrong scope")
	}
	m.Update(key("u"))
	if !reflect.DeepEqual(target.Outputs, before) || target.Count() != 4 || target.outputsChanged {
		t.Fatal("undo failed")
	}
	m.Update(key("b"))
	m.Update(key("r"))
	if !reflect.DeepEqual(target.Outputs, before) || target.Count() != 4 || target.outputsChanged {
		t.Fatal("reset failed")
	}
	m.Update(key("u"))
	if target.Keep[3] || !target.outputsChanged {
		t.Fatal("undo reset failed")
	}
	target.Metadata = nil
	m.Update(key("b"))
	if !strings.Contains(strings.Join(m.previewLines(), "\n"), "Batch audio not applied") {
		t.Fatal("missing preview warning")
	}
	for i := range target.Keep {
		target.Keep[i] = false
	}
	if !strings.Contains(m.warning(target), "NO AUDIO") {
		t.Fatal("batch warning hid no-audio warning")
	}
}
