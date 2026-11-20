package review

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/friesencr/transcoop/internal/farm"
)

func TestMain(m *testing.M) {
	if os.Getenv("TRANSCOOP_REVIEW_TEST_FOLDER") == "1" {
		args := strings.Join(os.Args[1:], " ")
		if strings.Contains(args, "--preset-export") {
			fmt.Print(folderPreset)
			os.Exit(0)
		}
		if !strings.Contains(args, "--scan") {
			fmt.Fprintln(os.Stderr, "refusing encode during folder review")
			os.Exit(3)
		}
		if strings.Contains(args, "bad.mkv") {
			fmt.Fprintln(os.Stderr, "bad source")
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "scan log")
		fmt.Print(folderTitle)
		os.Exit(0)
	}

	if os.Getenv("TRANSCOOP_REVIEW_TEST_SCANNER") == "1" {
		fmt.Fprint(os.Stdout, `JSON Title Set: {`)
		fmt.Fprint(os.Stderr, "HandBrake scan log\n")
		fmt.Fprint(os.Stdout, `"TitleList":[{"Index":1,"AudioList":[{"LanguageCode":"eng"}]}]}`)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestScanKeepsLogsSeparateFromJSON(t *testing.T) {
	t.Setenv("TRANSCOOP_REVIEW_TEST_SCANNER", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	audio, err := Scan(context.Background(), exe, "fixture.mkv", 1)
	if err != nil || len(audio) != 1 || audio[0].LanguageCode != "eng" {
		t.Fatalf("scan: %v %v", audio, err)
	}
}

func fixture(t *testing.T) []*Entry {
	t.Helper()
	data := `[{"Job":{"Source":{"Path":"/media/a.mkv","Title":1},"Destination":{"File":"/output/a.mkv"},"Video":{"Encoder":"x265_10bit","Quality":20},"Audio":{"FallbackEncoder":"ac3","AudioList":[{"Track":0,"Encoder":"flac16","Mixdown":"stereo","Quality":-3},{"Track":0,"Encoder":"copy:ac3"},{"Track":1,"Encoder":"copy"},{"Track":2,"Encoder":"opus"}]},"Subtitle":{"Search":{"Enable":true},"SubtitleList":[{"Track":0,"Default":true}]}},"Title":{"AudioList":[{"LanguageCode":"eng","CodecName":"ac3","ChannelCount":6,"Name":"Main"},{"LanguageCode":"eng","CodecName":"aac","ChannelCount":2,"Name":"Director commentary","Attributes":{"Commentary":true}},{"LanguageCode":"jpn","CodecName":"aac","ChannelCount":2,"Name":"Original"}]},"extra":{"keep":1234567890123456789}}]`
	entries, err := Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestExclusionsPreserveEncodingAndSubtitleSettings(t *testing.T) {
	e := fixture(t)[0]
	e.Keep[2] = false
	raw, err := e.Export()
	if err != nil {
		t.Fatal(err)
	}
	var original, changed map[string]json.RawMessage
	json.Unmarshal(e.Raw, &original)
	json.Unmarshal(raw, &changed)
	var before, after map[string]json.RawMessage
	json.Unmarshal(original["Job"], &before)
	json.Unmarshal(changed["Job"], &after)
	for _, key := range []string{"Video", "Subtitle", "Source", "Destination"} {
		if string(before[key]) != string(after[key]) {
			t.Fatalf("changed %s", key)
		}
	}
	if string(original["extra"]) != string(changed["extra"]) {
		t.Fatal("lost unknown fields or integer precision")
	}
	reloaded, err := Parse([]byte("[" + string(raw) + "]"))
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded[0].Outputs) != 3 {
		t.Fatal("wrong output count")
	}
	for newIndex, oldIndex := range []int{0, 1, 3} {
		if string(reloaded[0].Outputs[newIndex].Raw) != string(e.Outputs[oldIndex].Raw) {
			t.Fatal("modified retained encoding settings")
		}
	}
	for i := range e.Keep {
		e.Keep[i] = false
	}
	raw, err = e.Export()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"AudioList":[]`) {
		t.Fatal("no-audio must be an explicit empty array")
	}
}

func TestRulesUseMetadataAndPreserveMultipleOutputs(t *testing.T) {
	for _, test := range []struct {
		rule Rule
		want []bool
	}{
		{Rule{Kind: "first"}, []bool{true, true, false, true}},
		{Rule{Kind: "commentary"}, []bool{true, true, false, true}},
		{Rule{Kind: "keep-languages", Value: "jpn"}, []bool{false, false, false, true}},
		{Rule{Kind: "codec", Value: "ac3"}, []bool{false, false, true, true}},
		{Rule{Kind: "channels", Value: "2"}, []bool{true, true, false, false}},
		{Rule{Kind: "name", Value: "director"}, []bool{true, true, false, true}},
	} {
		t.Run(test.rule.Kind, func(t *testing.T) {
			entries := fixture(t)
			Apply(entries, test.rule)
			if !reflect.DeepEqual(entries[0].Keep, test.want) {
				t.Fatal(entries[0].Keep)
			}
		})
	}
	entries := fixture(t)
	entries[0].Metadata = nil
	removed, skipped := Apply(entries, Rule{Kind: "keep-languages", Value: "eng"})
	if removed != 0 || skipped != 1 || entries[0].Count() != 4 {
		t.Fatal("unknown metadata must not cause silent exclusions")
	}
}

func key(s string) tea.KeyPressMsg { return tea.KeyPressMsg{Code: []rune(s)[0], Text: s} }
func TestSelectionFilterUndoAndPreview(t *testing.T) {
	entries := append(fixture(t), fixture(t)...)
	entries[1].Source = "/different/b.mkv"
	entries[1].Destination = "/different/b-out.mkv"
	m := newModel(context.Background(), Options{}, entries)
	m.filter = "/media/"
	m.Update(key("c"))
	if entries[0].Keep[2] || !entries[1].Keep[2] {
		t.Fatal("batch rule changed a hidden file")
	}
	m.Update(key("u"))
	if !entries[0].Keep[2] {
		t.Fatal("undo did not restore")
	}
	m.Update(key("s"))
	if !m.preview || m.confirm != "" || m.busy {
		t.Fatal("first submit must open review without importing")
	}
	m.Update(key("s"))
	if m.confirm != "submit" {
		t.Fatal("submission must be confirmed")
	}
	m.Update(key("n"))
	if m.confirm != "" || m.busy {
		t.Fatal("cancel submitted jobs")
	}
	entries[0].Selected = false
	if len(m.targets()) != 0 {
		t.Fatal("deselected files included")
	}
}

func TestSubmitReviewedPayloadToCoordinator(t *testing.T) {
	entries := fixture(t)
	dir := t.TempDir()
	// Adapt the fixture paths to the current OS, including Windows test runs.
	raw := strings.ReplaceAll(string(entries[0].Raw), "/media/a.mkv", filepath.ToSlash(filepath.Join(dir, "source.mkv")))
	raw = strings.ReplaceAll(raw, "/output/a.mkv", filepath.ToSlash(filepath.Join(dir, "out.mkv")))
	entries, err := Parse([]byte("[" + raw + "]"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := farm.OpenStore(filepath.Join(dir, "farm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InstallAdmin(context.Background(), "0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer((&farm.Server{Store: store}).Handler())
	defer server.Close()
	m := newModel(context.Background(), Options{Client: farm.NewClient(server.URL, "0123456789abcdef0123456789abcdef")}, entries)
	m.apply(Rule{Kind: "commentary"})
	cmd := m.submit()
	if cmd == nil {
		t.Fatal("no submit command")
	}
	msg := cmd().(operationMsg)
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	m.Update(msg)
	j, err := store.Claim(context.Background(), "worker", []string{"x265_10bit"}, farm.LeaseDuration)
	if err != nil || j == nil {
		t.Fatalf("claim: %v %v", j, err)
	}
	queue, err := farm.RewriteQueue(*j, filepath.Join(dir, "worker-source.mkv"), filepath.Join(dir, "worker-stage.mkv"), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Parse(queue)
	if err != nil {
		t.Fatal(err)
	}
	if len(result[0].Outputs) != 3 || result[0].Outputs[2].Source != 2 {
		t.Fatal("worker did not receive the reviewed tracks")
	}
}

func TestSaveIsNonDestructiveAndReloadable(t *testing.T) {
	entries := fixture(t)
	Apply(entries, Rule{Kind: "commentary"})
	queue, err := Export(entries)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "review.json")
	if err = Save(path, queue); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if err = Save(path, []json.RawMessage{}); err == nil {
		t.Fatal("save overwrote existing file")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("existing queue changed")
	}
	reloaded, err := Load(path)
	if err != nil || reloaded[0].Count() != 3 {
		t.Fatalf("reload: %v %v", reloaded, err)
	}
}

func TestScanJSONAndTerminalSanitizing(t *testing.T) {
	data := []byte("logs\nJSON Title Set: {\"TitleList\":[{\"Index\":2,\"AudioList\":[{\"LanguageCode\":\"eng\"}]}]}\nHandBrake has exited.\n")
	audio, err := ParseScan(data, 2)
	if err != nil || len(audio) != 1 {
		t.Fatalf("scan: %v %v", audio, err)
	}
	if _, err = ParseScan(data, 1); err == nil {
		t.Fatal("wrong title accepted")
	}
	if _, err = ParseScan([]byte("not JSON"), 1); err == nil {
		t.Fatal("invalid scan accepted")
	}
	entries := fixture(t)
	entries[0].Metadata[0].Name = "bad\x1b[2J\nname"
	m := newModel(context.Background(), Options{}, entries)
	if strings.Contains(m.View().Content, "\x1b") {
		t.Fatal("untrusted terminal escape rendered")
	}
	if _, err = ParseRule("channels=oops"); err == nil {
		t.Fatal("invalid rule accepted")
	}
}
