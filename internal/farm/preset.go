package farm

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PresetSnapshot is private to transcoop queue files. It freezes a resolved
// HandBrake preset. Audio outputs are explicit; version 2 also supplies reviewed
// Job.Subtitle selections. Version 1 leaves subtitles to the saved preset.
type PresetSnapshot struct {
	Version int             `json:"Version"`
	Name    string          `json:"Name"`
	Export  json.RawMessage `json:"Export"`
}

func ReadPresetSnapshot(raw json.RawMessage) (*PresetSnapshot, map[string]any, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, nil, err
	}
	data, ok := root["TranscoopPreset"]
	if !ok {
		data, ok = root["HBBatchPreset"] // Queues saved before the preset key was renamed.
	}
	if !ok {
		return nil, nil, nil
	}
	var snapshot PresetSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, nil, err
	}
	if snapshot.Name == "" {
		return nil, nil, errors.New("preset snapshot label is missing")
	}
	if snapshot.Version != 1 && snapshot.Version != 2 {
		return nil, nil, errors.New("unsupported transcoop preset job version")
	}
	var export struct{ PresetList []map[string]any }
	if err := json.Unmarshal(snapshot.Export, &export); err != nil {
		return nil, nil, err
	}
	if len(export.PresetList) != 1 {
		return nil, nil, errors.New("preset job requires exactly one resolved preset")
	}
	p := export.PresetList[0]
	if name, _ := p["PresetName"].(string); name == "" {
		return nil, nil, errors.New("preset name is missing")
	}
	if enc, _ := p["VideoEncoder"].(string); enc == "" {
		return nil, nil, errors.New("preset encoder is missing")
	}
	return &snapshot, p, nil
}

// PresetArguments writes the frozen preset and supplies every reviewed audio
// output explicitly. It never reads a worker's GUI preset configuration.
func PresetArguments(raw json.RawMessage, source, destination, dir string) ([]string, error) {
	snapshot, p, err := ReadPresetSnapshot(raw)
	if err != nil || snapshot == nil {
		return nil, err
	}
	var q struct {
		Job struct {
			Source struct{ Title int }
			Video  struct{ Encoder string }
			Audio  struct{ AudioList []map[string]any }
		}
	}
	if err = json.Unmarshal(raw, &q); err != nil {
		return nil, err
	}
	if q.Job.Video.Encoder != p["VideoEncoder"] {
		return nil, errors.New("preset job encoder does not match snapshot")
	}
	subtitleArgs := []string{}
	if snapshot.Version == 2 {
		subtitleArgs, err = reviewedSubtitleArguments(raw)
		if err != nil {
			return nil, err
		}
	}
	file := filepath.Join(dir, "preset.json")
	if err = os.WriteFile(file, snapshot.Export, 0600); err != nil {
		return nil, err
	}
	args := []string{"--preset-import-file", file, "--preset", p["PresetName"].(string), "--input", source, "--output", destination, "--title", strconv.Itoa(max(1, q.Job.Source.Title)), "--min-duration", "0", "--json"}
	args = append(args, subtitleArgs...)
	outputs := q.Job.Audio.AudioList
	if len(outputs) == 0 {
		return append(args, "--audio", "none"), nil
	}
	tracks := []string{}
	// Empty entries are intentional: HandBrake preserves per-output defaults for
	// unspecified bitrate/quality values. All template values are retained.
	fields := []struct{ key, flag, def string }{
		{"Encoder", "--aencoder", ""}, {"Mixdown", "--mixdown", "none"}, {"Samplerate", "--arate", "auto"},
		{"Bitrate", "--ab", ""}, {"Quality", "--aq", ""}, {"CompressionLevel", "--ac", ""},
		{"DRC", "--drc", "0"}, {"Gain", "--gain", "0"}, {"DitherMethod", "--adither", "auto"}, {"NormalizeMixLevel", "--normalize-mix", "0"},
	}
	for _, a := range outputs {
		track, ok := a["Track"].(float64)
		if !ok || track < 0 || track != float64(int(track)) {
			return nil, errors.New("invalid preset audio track")
		}
		tracks = append(tracks, strconv.Itoa(int(track)+1))
	}
	args = append(args, "--audio", strings.Join(tracks, ","))
	for _, f := range fields {
		values := make([]string, len(outputs))
		present := false
		for i, a := range outputs {
			value, ok := a[f.key]
			values[i] = f.def
			if ok {
				present = true
				switch v := value.(type) {
				case bool:
					if v {
						values[i] = "1"
					} else {
						values[i] = "0"
					}
				case string:
					values[i] = v
				case float64:
					values[i] = strconv.FormatFloat(v, 'f', -1, 64)
				default:
					return nil, fmt.Errorf("invalid audio %s", f.key)
				}
			}
			if strings.ContainsAny(values[i], ",\x00\r\n") {
				return nil, fmt.Errorf("invalid audio %s", f.key)
			}
		}
		if present {
			args = append(args, f.flag, strings.Join(values, ","))
		}
	}
	return args, nil
}

// Explicit subtitle switches override the preset's automatic selection. The
// indices for forced/default/burn refer to output positions, including scan.
func reviewedSubtitleArguments(raw json.RawMessage) ([]string, error) {
	var q struct {
		Job struct {
			Subtitle *struct {
				Search       struct{ Enable, Default, Forced, Burn bool }
				SubtitleList []struct {
					Track                 *int
					Import                json.RawMessage
					Default, Forced, Burn bool
				}
			}
		}
	}
	if err := json.Unmarshal(raw, &q); err != nil {
		return nil, err
	}
	sub := q.Job.Subtitle
	if sub == nil {
		return nil, errors.New("missing reviewed subtitles")
	}
	tracks, forced := []string{}, []string{}
	def, burn := "none", "none"
	add := func(track string, d, f, b bool) error {
		tracks = append(tracks, track)
		index := strconv.Itoa(len(tracks))
		if d && b {
			return errors.New("a burned subtitle cannot be the default subtitle")
		}
		if d {
			if def != "none" {
				return errors.New("multiple default subtitles")
			}
			def = index
		}
		if b {
			if burn != "none" {
				return errors.New("multiple burned subtitles")
			}
			burn = index
		}
		if f {
			forced = append(forced, index)
		}
		return nil
	}
	if sub.Search.Enable {
		if err := add("scan", sub.Search.Default, sub.Search.Forced, sub.Search.Burn); err != nil {
			return nil, err
		}
	}
	for _, s := range sub.SubtitleList {
		if len(s.Import) > 0 && string(s.Import) != "null" {
			return nil, errors.New("preset subtitle overrides require embedded tracks")
		}
		if s.Track == nil || *s.Track < 0 {
			return nil, errors.New("invalid reviewed subtitle track")
		}
		if err := add(strconv.Itoa(*s.Track+1), s.Default, s.Forced, s.Burn); err != nil {
			return nil, err
		}
	}
	if len(tracks) == 0 {
		tracks = []string{"none"}
	}
	if len(forced) == 0 {
		forced = []string{"none"}
	}
	return []string{"--subtitle", strings.Join(tracks, ","), "--subtitle-default=" + def, "--subtitle-burned=" + burn, "--subtitle-forced=" + strings.Join(forced, ",")}, nil
}
