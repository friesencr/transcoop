// Package review prepares batch audio selections and settings before import.
package review

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/friesencr/transcoop/internal/farm"
	"os"
	"strconv"
	"strings"
)

type AudioInfo struct {
	LanguageCode string
	CodecName    string
	Codec        int
	ChannelCount int
	Name         string
	Description  string
	Attributes   struct{ Commentary, AltCommentary bool }
}

func (a AudioInfo) Commentary() bool {
	return a.Attributes.Commentary || a.Attributes.AltCommentary || strings.Contains(strings.ToLower(a.Name+" "+a.Description), "commentary")
}

type Output struct {
	Source  int
	Encoder string
	Raw     json.RawMessage
}
type Entry struct {
	PresetName          string
	Raw                 json.RawMessage
	Source, Destination string
	Title               int
	Metadata            []AudioInfo
	SubtitleMetadata    []SubtitleInfo
	Subtitles           []SubtitleOutput
	subtitlesChanged    bool
	Outputs             []Output
	Keep                []bool
	Selected            bool
	AudioWarning        string
	outputsChanged      bool
}

func Load(path string) ([]*Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}
func Parse(data []byte) ([]*Entry, error) {
	var queue []json.RawMessage
	if err := json.Unmarshal(data, &queue); err != nil {
		return nil, err
	}
	if len(queue) == 0 {
		return nil, errors.New("queue is empty")
	}
	entries := make([]*Entry, 0, len(queue))
	for i, raw := range queue {
		var q struct {
			Job struct {
				Source struct {
					Path  string
					Title int
				}
				Destination struct{ File string }
				Audio       struct{ AudioList []json.RawMessage }
				Video       struct{ Encoder string }
				Subtitle    json.RawMessage
			}
			Title struct {
				AudioList    []AudioInfo
				SubtitleList []SubtitleInfo
			}
		}
		if err := json.Unmarshal(raw, &q); err != nil {
			return nil, fmt.Errorf("job %d: %w", i+1, err)
		}
		if q.Job.Source.Path == "" || q.Job.Destination.File == "" || q.Job.Video.Encoder == "" {
			return nil, fmt.Errorf("job %d: missing source, destination or video encoder", i+1)
		}
		e := &Entry{Raw: raw, Source: q.Job.Source.Path, Destination: q.Job.Destination.File, Title: q.Job.Source.Title, Metadata: q.Title.AudioList, Selected: true}
		for _, audio := range q.Job.Audio.AudioList {
			var track struct {
				Track   *int
				Encoder string
			}
			if err := json.Unmarshal(audio, &track); err != nil {
				return nil, fmt.Errorf("job %d: %w", i+1, err)
			}
			if track.Track == nil || *track.Track < 0 {
				return nil, fmt.Errorf("job %d: invalid audio track index", i+1)
			}
			e.Outputs = append(e.Outputs, Output{Source: *track.Track, Encoder: track.Encoder, Raw: audio})
			e.Keep = append(e.Keep, true)
		}
		e.SubtitleMetadata = q.Title.SubtitleList
		snapshot, preset, err := farm.ReadPresetSnapshot(raw)
		if err != nil {
			return nil, err
		}
		if snapshot != nil {
			e.PresetName = snapshot.Name
			if snapshot.Version == 1 {
				q.Job.Subtitle = presetSubtitles(preset, e.SubtitleMetadata, e.Metadata, e.Outputs)
			}
		}
		if err := e.initSubtitles(q.Job.Subtitle); err != nil {
			return nil, fmt.Errorf("job %d: %w", i+1, err)
		}
		entries = append(entries, e)
	}
	return entries, nil
}
func (e *Entry) Info(output int) (AudioInfo, bool) {
	i := e.Outputs[output].Source
	if i >= len(e.Metadata) {
		return AudioInfo{}, false
	}
	return e.Metadata[i], true
}
func (e *Entry) Count() int {
	n := 0
	for _, keep := range e.Keep {
		if keep {
			n++
		}
	}
	return n
}
func (e *Entry) CompleteMetadata() bool {
	for i := range e.Outputs {
		info, ok := e.Info(i)
		if !ok || info.LanguageCode == "" {
			return false
		}
	}
	return true
}

// Export preserves untouched settings and changes only reviewed audio/subtitles.
func (e *Entry) Export() (json.RawMessage, error) {
	if !e.outputsChanged && e.Count() == len(e.Outputs) && !e.subtitlesChanged {
		return bytes.Clone(e.Raw), nil
	}
	var root, job, audio map[string]json.RawMessage
	if err := json.Unmarshal(e.Raw, &root); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(root["Job"], &job); err != nil {
		return nil, err
	}
	if e.outputsChanged || e.Count() != len(e.Outputs) {
		if err := json.Unmarshal(job["Audio"], &audio); err != nil {
			return nil, err
		}
		if audio == nil {
			return nil, errors.New("missing Audio object")
		}
		list := []json.RawMessage{}
		for i, output := range e.Outputs {
			if e.Keep[i] {
				list = append(list, output.Raw)
			}
		}
		audio["AudioList"], _ = json.Marshal(list)
		job["Audio"], _ = json.Marshal(audio)
	}
	if e.subtitlesChanged {
		var err error
		job["Subtitle"], err = e.exportSubtitles(job["Subtitle"])
		if err != nil {
			return nil, err
		}
		snapshot, _, err := farm.ReadPresetSnapshot(e.Raw)
		if err != nil {
			return nil, err
		}
		if snapshot != nil {
			delete(root, "HBBatchPreset")
			// Older workers reject this version instead of ignoring overrides.
			snapshot.Version = 2
			root["TranscoopPreset"], _ = json.Marshal(snapshot)
		}
	}
	root["Job"], _ = json.Marshal(job)
	return json.Marshal(root)
}

type Rule struct{ Kind, Value string }

func ParseRule(text string) (Rule, error) {
	kind, value, ok := strings.Cut(strings.TrimSpace(text), "=")
	kind = strings.ToLower(strings.TrimSpace(kind))
	value = strings.ToLower(strings.TrimSpace(value))
	if !ok || value == "" {
		return Rule{}, errors.New("use lang=spa, codec=ac3, channels=2 or name=commentary")
	}
	switch kind {
	case "lang", "codec", "name":
	case "channels":
		if n, err := strconv.Atoi(value); err != nil || n < 1 {
			return Rule{}, errors.New("channels must be a positive integer")
		}
	default:
		return Rule{}, errors.New("supported matches: lang, codec, channels, name")
	}
	return Rule{kind, value}, nil
}

// Apply only removes outputs, allowing rules to compose. Reset/undo restores them.
// Unknown metadata skips an entire file for rules that could otherwise silently discard it.
func Apply(entries []*Entry, rule Rule) (removed, skipped int) {
	languages := map[string]bool{}
	for _, lang := range strings.Split(rule.Value, ",") {
		languages[strings.TrimSpace(strings.ToLower(lang))] = true
	}
	for _, e := range entries {
		if !e.CompleteMetadata() {
			skipped++
			continue
		}
		first := map[string]int{}
		for i, out := range e.Outputs {
			if !e.Keep[i] {
				continue
			}
			a, _ := e.Info(i)
			lang := strings.ToLower(a.LanguageCode)
			drop := false
			switch rule.Kind {
			case "keep-languages":
				drop = !languages[lang]
			case "commentary":
				drop = a.Commentary()
			case "first":
				if source, ok := first[lang]; ok {
					drop = source != out.Source
				} else {
					first[lang] = out.Source
				}
			case "lang":
				drop = lang == rule.Value
			case "codec":
				drop = strings.EqualFold(a.CodecName, rule.Value) || a.codec() == rule.Value
			case "channels":
				drop = strconv.Itoa(a.ChannelCount) == rule.Value
			case "name":
				drop = strings.Contains(strings.ToLower(a.Name), rule.Value)
			}
			if drop {
				e.Keep[i] = false
				removed++
			}
		}
	}
	return
}

func Export(entries []*Entry) ([]json.RawMessage, error) {
	if len(entries) == 0 {
		return nil, errors.New("select at least one visible file")
	}
	queue := make([]json.RawMessage, 0, len(entries))
	for _, e := range entries {
		raw, err := e.Export()
		if err != nil {
			return nil, err
		}
		queue = append(queue, raw)
	}
	return queue, nil
}

// Save never replaces the input queue or any existing file.
func Save(path string, queue []json.RawMessage) error {
	data, err := json.MarshalIndent(queue, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}
