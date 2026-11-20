package review

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/friesencr/transcoop/internal/farm"
)

func (m *model) changeSubtitle(action string) {
	e := m.current()
	if e == nil || m.track < 0 || m.track >= len(e.Subtitles) {
		return
	}
	m.snapshot()
	if err := e.editSubtitle(m.track, action); err != nil {
		m.status = err.Error()
		return
	}
	m.status = "Changed this subtitle. b: apply to selected files | u: undo"
}

func (m *model) resetSubtitles() {
	m.snapshot()
	for _, e := range m.targets() {
		original, err := Parse(append(append([]byte{'['}, e.Raw...), ']'))
		if err != nil {
			m.status = err.Error()
			return
		}
		e.Subtitles = original[0].Subtitles
		e.addSubtitleSources()
		e.subtitlesChanged = false
	}
	m.status = "Restored original subtitle selections for selected visible files. u: undo"
}

type subtitleMatch struct {
	language, codec    string
	forced, commentary bool
	ordinal            int
}

func subtitleMatches(e *Entry) map[int]subtitleMatch {
	result := map[int]subtitleMatch{}
	counts := map[subtitleMatch]int{}
	for i, s := range e.SubtitleMetadata {
		group := subtitleMatch{language: s.LanguageCode, codec: s.SourceName, forced: s.Attributes.Forced, commentary: s.Attributes.Commentary}
		match := group
		match.ordinal = counts[group]
		counts[group]++
		result[i] = match
	}
	return result
}

func applySubtitles(reference, target *Entry) error {
	from, to := subtitleMatches(reference), subtitleMatches(target)
	outputs := append([]SubtitleOutput(nil), target.Subtitles...)
	for i := range outputs {
		outputs[i].Keep = false
		outputs[i].Default = false
		outputs[i].Burn = false
	}
	for _, s := range reference.Subtitles {
		if !s.Keep {
			continue
		}
		match, ok := from[s.Source]
		if s.Source >= 0 && (!ok || match.language == "") {
			return fmt.Errorf("missing reference subtitle metadata; rescan first")
		}
		if s.Source == -2 {
			return fmt.Errorf("external subtitle imports must be reviewed per file")
		}
		index := -1
		for j, d := range outputs {
			if s.Source == -1 && d.Source == -1 {
				index = j
				break
			}
			if d.Source >= 0 {
				other, ok := to[d.Source]
				if ok && s.Source >= 0 && other == match {
					index = j
					break
				}
			}
		}
		if index < 0 {
			return fmt.Errorf("no matching %s %s subtitle %d", match.language, match.codec, match.ordinal+1)
		}
		outputs[index].Keep = true
		outputs[index].Default, outputs[index].Forced, outputs[index].Burn = s.Default, s.Forced, s.Burn
	}
	target.Subtitles = outputs
	target.subtitlesChanged = true
	return nil
}

func (m *model) batchSubtitles() {
	reference := m.current()
	if reference == nil {
		return
	}
	targets := m.targets()
	if len(targets) == 0 {
		m.status = "Select at least one visible file."
		return
	}
	m.snapshot()
	applied, skipped := 0, 0
	reason := ""
	for _, e := range targets {
		if e == reference {
			continue
		}
		if err := applySubtitles(reference, e); err != nil {
			skipped++
			reason = filepath.Base(e.Source) + ": " + err.Error()
		} else {
			applied++
		}
	}
	m.status = fmt.Sprintf("Applied subtitles to %d files; %d skipped. u: undo", applied, skipped)
	if reason != "" {
		m.status += " | " + reason
	}
}

// Keep metadata in resaved queues after a rescan so newly added subtitle tracks
// can still be identified when that queue is reopened.
func (e *Entry) updateScanMetadata(audio []AudioInfo, subtitles []SubtitleInfo) {
	e.Metadata, e.SubtitleMetadata = audio, subtitles
	e.addSubtitleSources()
	var root map[string]json.RawMessage
	if json.Unmarshal(e.Raw, &root) != nil {
		return
	}
	title := map[string]json.RawMessage{}
	if raw := root["Title"]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &title)
	}
	if title == nil {
		title = map[string]json.RawMessage{}
	}
	title["AudioList"], _ = json.Marshal(audio)
	title["SubtitleList"], _ = json.Marshal(subtitles)
	root["Title"], _ = json.Marshal(title)
	e.Raw, _ = json.Marshal(root)
	if !e.subtitlesChanged {
		snapshot, preset, err := farm.ReadPresetSnapshot(e.Raw)
		if err == nil && snapshot != nil && snapshot.Version == 1 {
			_ = e.initSubtitles(presetSubtitles(preset, e.SubtitleMetadata, e.Metadata, e.Outputs))
		}
	}
}
