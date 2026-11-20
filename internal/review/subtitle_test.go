package review

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/friesencr/transcoop/internal/farm"
)

func subtitleFixture(t *testing.T, folder bool) *Entry {
	t.Helper()
	var root map[string]json.RawMessage
	if err := json.Unmarshal(fixture(t)[0].Raw, &root); err != nil {
		t.Fatal(err)
	}
	var title map[string]any
	_ = json.Unmarshal(root["Title"], &title)
	title["SubtitleList"] = []any{
		map[string]any{"LanguageCode": "eng", "SourceName": "PGS", "Source": 6, "Name": "Full"},
		map[string]any{"LanguageCode": "eng", "SourceName": "PGS", "Source": 6, "Name": "Signs"},
		map[string]any{"LanguageCode": "spa", "SourceName": "UTF-8", "Source": 3},
	}
	root["Title"], _ = json.Marshal(title)
	if folder {
		var export struct{ PresetList []map[string]any }
		_ = json.Unmarshal([]byte(folderPreset), &export)
		export.PresetList[0]["SubtitleTrackSelectionBehavior"] = "all"
		export.PresetList[0]["SubtitleBurnBehavior"] = "none"
		export.PresetList[0]["SubtitleTrackNamePassthru"] = true
		raw, _ := json.Marshal(export)
		root["TranscoopPreset"], _ = json.Marshal(farm.PresetSnapshot{Version: 1, Name: "Mkv", Export: raw})
	}
	raw, _ := json.Marshal([]any{root})
	entries, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return entries[0]
}

func TestSubtitleDefaultsAndOverrideRoundTrip(t *testing.T) {
	e := subtitleFixture(t, true)
	if len(e.Subtitles) != 4 || !e.Subtitles[0].Keep || !e.Subtitles[0].Forced || !e.Subtitles[1].Keep || !e.Subtitles[2].Keep || e.Subtitles[3].Keep {
		t.Fatal(e.Subtitles)
	}
	// Leaving the defaults alone must preserve the frozen preset and job exactly.
	untouched, err := e.Export()
	if err != nil || string(untouched) != string(e.Raw) {
		t.Fatalf("untouched export changed: %v", err)
	}
	m := newModel(context.Background(), Options{}, []*Entry{e})
	m.Update(key("t"))
	m.Update(key(" ")) // disable foreign audio search
	m.track = 1
	m.Update(key(" ")) // remove first English
	m.track = 2
	m.Update(key(" ")) // remove second English
	m.track = 3
	m.Update(key("d")) // add previously unselected Spanish and default it
	if !e.Subtitles[3].Keep || !e.Subtitles[3].Default || e.subtitleCount() != 1 {
		t.Fatal(e.Subtitles)
	}
	raw, err := e.Export()
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := farm.ReadPresetSnapshot(raw)
	if err != nil || snapshot.Version != 2 {
		t.Fatalf("missing version guard: %v %v", snapshot, err)
	}
	args, err := farm.PresetArguments(raw, "source", "out", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--subtitle 3 --subtitle-default=1 --subtitle-burned=none --subtitle-forced=none") {
		t.Fatal(joined)
	}
	again, err := Parse(append(append([]byte{'['}, raw...), ']'))
	if err != nil || again[0].subtitleCount() != 1 || again[0].Subtitles[1].Source != 2 || !again[0].Subtitles[1].Default {
		t.Fatalf("reload: %v %v", again, err)
	}
	if !strings.Contains(m.View().Content, "SUBTITLES") || !strings.Contains(strings.Join(m.previewLines(), "\n"), "spa | UTF-8") {
		t.Fatal("missing subtitle review")
	}
	m.Update(key("u"))
	if e.Subtitles[3].Keep {
		t.Fatal("undo failed")
	}
	m.Update(key("r"))
	if e.subtitlesChanged || e.subtitleCount() != 3 {
		t.Fatal("reset did not restore preset", e.Subtitles)
	}
	m.Update(key("u"))
	if !e.subtitlesChanged || e.subtitleCount() != 0 {
		t.Fatal("undo reset failed")
	}
}

func TestSubtitleQueuePreservesOtherSettings(t *testing.T) {
	e := subtitleFixture(t, false)
	if len(e.Subtitles) != 4 || !e.Subtitles[1].Default || e.Subtitles[2].Keep {
		t.Fatal(e.Subtitles)
	}
	if err := e.editSubtitle(2, "keep"); err != nil {
		t.Fatal(err)
	}
	if err := e.editSubtitle(2, "default"); err != nil {
		t.Fatal(err)
	}
	if e.Subtitles[1].Default {
		t.Fatal("multiple defaults")
	}
	raw, err := e.Export()
	if err != nil {
		t.Fatal(err)
	}
	var before, after map[string]json.RawMessage
	_ = json.Unmarshal(e.Raw, &before)
	_ = json.Unmarshal(raw, &after)
	if string(before["extra"]) != string(after["extra"]) {
		t.Fatal("lost unknown data")
	}
	var bj, aj map[string]json.RawMessage
	_ = json.Unmarshal(before["Job"], &bj)
	_ = json.Unmarshal(after["Job"], &aj)
	for _, k := range []string{"Audio", "Video", "Source", "Destination"} {
		if string(bj[k]) != string(aj[k]) {
			t.Fatalf("changed %s", k)
		}
	}
	if err := e.editSubtitle(2, "burn"); err != nil {
		t.Fatal(err)
	}
	if !e.Subtitles[2].Burn || e.Subtitles[2].Default {
		t.Fatal("burn/default conflict")
	}
	if err := e.editSubtitle(3, "forced"); err == nil {
		t.Fatal("allowed forced-only on text subtitles")
	}
}

func TestSubtitleNoneWithNoAudio(t *testing.T) {
	e := subtitleFixture(t, true)
	for i := range e.Subtitles {
		if e.Subtitles[i].Keep {
			_ = e.editSubtitle(i, "keep")
		}
	}
	for i := range e.Keep {
		e.Keep[i] = false
	}
	raw, err := e.Export()
	if err != nil {
		t.Fatal(err)
	}
	args, err := farm.PresetArguments(raw, "source", "out", t.TempDir())
	joined := strings.Join(args, " ")
	if err != nil || !strings.Contains(joined, "--subtitle none") || !strings.Contains(joined, "--audio none") {
		t.Fatal(args, err)
	}
}

func TestBatchSubtitlesUsesLanguageAndSkipsMissing(t *testing.T) {
	ref, target := subtitleFixture(t, true), subtitleFixture(t, true)
	ref.Subtitles[0].Keep = false
	ref.Subtitles[1].Keep = false
	ref.Subtitles[2].Default = true
	target.SubtitleMetadata[0], target.SubtitleMetadata[2] = target.SubtitleMetadata[2], target.SubtitleMetadata[0]
	if err := applySubtitles(ref, target); err != nil {
		t.Fatal(err)
	}
	if target.subtitleCount() != 1 || !target.Subtitles[3].Keep || !target.Subtitles[3].Default {
		t.Fatal(target.Subtitles)
	}
	before := append([]SubtitleOutput(nil), target.Subtitles...)
	target.SubtitleMetadata = nil
	if err := applySubtitles(ref, target); err == nil || !reflect.DeepEqual(before, target.Subtitles) {
		t.Fatal("missing match changed target")
	}
}

func TestSubtitleSearchArgumentsIncludeOutputOffset(t *testing.T) {
	e := subtitleFixture(t, true)
	e.subtitlesChanged = true
	e.Subtitles[0].Default = false
	e.Subtitles[2].Default = true
	e.Subtitles[2].Forced = true
	raw, err := e.Export()
	if err != nil {
		t.Fatal(err)
	}
	args, err := farm.PresetArguments(raw, "source", "out", t.TempDir())
	joined := strings.Join(args, " ")
	if err != nil || !strings.Contains(joined, "--subtitle scan,1,2 --subtitle-default=3 --subtitle-burned=none --subtitle-forced=1,3") {
		t.Fatal(args, err)
	}
}

func TestSubtitleRescanRetainsEditsAndMetadataOnSave(t *testing.T) {
	e := subtitleFixture(t, false)
	e.SubtitleMetadata = nil
	e.Subtitles = e.Subtitles[:2]
	m := newModel(context.Background(), Options{}, []*Entry{e})
	m.Update(key("t"))
	m.track = 1
	m.Update(key(" "))
	metadata := subtitleFixture(t, false).SubtitleMetadata
	e.updateScanMetadata(e.Metadata, metadata)
	if e.Subtitles[1].Keep || len(e.Subtitles) != 4 {
		t.Fatal("rescan changed selections or lost available sources")
	}
	m.Update(key("u"))
	if !e.Subtitles[1].Keep || len(e.Subtitles) != 4 {
		t.Fatal("undo lost newly scanned sources")
	}
	_ = e.editSubtitle(3, "keep")
	raw, err := e.Export()
	if err != nil {
		t.Fatal(err)
	}
	again, err := Parse(append(append([]byte{'['}, raw...), ']'))
	if err != nil || len(again[0].SubtitleMetadata) != 3 {
		t.Fatalf("rescan metadata lost on save: %v", err)
	}
}
