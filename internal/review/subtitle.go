package review

import (
	"encoding/json"
	"fmt"
	"strings"
)

type SubtitleInfo struct {
	LanguageCode, Language, SourceName, Format, Name string
	Source                                           int
	Attributes                                       struct{ Default, Forced, Commentary, ClosedCaption bool }
}

type SubtitleOutput struct {
	Source                      int // -1: foreign audio search; -2: imported external subtitle
	Keep, Default, Forced, Burn bool
	Raw                         json.RawMessage
}

func (e *Entry) initSubtitles(raw json.RawMessage) error {
	var sub struct {
		Search       json.RawMessage
		SubtitleList []json.RawMessage
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &sub); err != nil {
			return err
		}
	}
	search := SubtitleOutput{Source: -1, Raw: sub.Search}
	if len(sub.Search) > 0 {
		var s struct{ Enable, Default, Forced, Burn bool }
		if err := json.Unmarshal(sub.Search, &s); err != nil {
			return err
		}
		search.Keep, search.Default, search.Forced, search.Burn = s.Enable, s.Default, s.Forced, s.Burn
	} else {
		search.Forced = true
	}
	e.Subtitles = []SubtitleOutput{search}
	for _, raw := range sub.SubtitleList {
		var s struct {
			Track                 *int
			Import                json.RawMessage
			Default, Forced, Burn bool
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		track := -2
		if len(s.Import) == 0 || string(s.Import) == "null" {
			if s.Track == nil || *s.Track < 0 {
				return fmt.Errorf("invalid subtitle track index")
			}
			track = *s.Track
		}
		e.Subtitles = append(e.Subtitles, SubtitleOutput{track, true, s.Default, s.Forced, s.Burn, raw})
	}
	e.addSubtitleSources()
	return nil
}

func (e *Entry) addSubtitleSources() {
	for track := range e.SubtitleMetadata {
		found := false
		for _, s := range e.Subtitles {
			found = found || s.Source == track
		}
		if !found {
			e.Subtitles = append(e.Subtitles, SubtitleOutput{Source: track})
		}
	}
}

func (e *Entry) subtitleCount() int {
	n := 0
	for _, s := range e.Subtitles {
		if s.Keep {
			n++
		}
	}
	return n
}

func (e *Entry) subtitleLabel(i int) string {
	s := e.Subtitles[i]
	label := "Foreign audio search (selected during encode)"
	if s.Source == -2 {
		label = "Imported external subtitle"
		var v struct{ Import struct{ Filename string } }
		_ = json.Unmarshal(s.Raw, &v)
		label += " | " + v.Import.Filename
	} else if s.Source >= 0 {
		label = fmt.Sprintf("source #%d | metadata unknown", s.Source+1)
		if s.Source < len(e.SubtitleMetadata) {
			info := e.SubtitleMetadata[s.Source]
			label = fmt.Sprintf("source #%d | %s | %s | %s", s.Source+1, info.LanguageCode, info.SourceName, info.Name)
		}
	}
	if s.Default {
		label += " [default]"
	}
	if s.Forced {
		label += " [forced only]"
	}
	if s.Burn {
		label += " [burn in]"
	}
	return label
}

func (e *Entry) editSubtitle(i int, action string) error {
	if i < 0 || i >= len(e.Subtitles) {
		return nil
	}
	s := &e.Subtitles[i]
	if action == "forced" && s.Source >= 0 && s.Source < len(e.SubtitleMetadata) {
		source := e.SubtitleMetadata[s.Source].Source
		if source != 0 && source != 6 {
			return fmt.Errorf("forced-only filtering is supported for DVD and PGS subtitles")
		}
	}
	switch action {
	case "keep":
		s.Keep = !s.Keep
	case "default":
		s.Default = !s.Default
		s.Keep = true
		if s.Default {
			s.Burn = false
		}
	case "forced":
		s.Forced = !s.Forced
		s.Keep = true
	case "burn":
		s.Burn = !s.Burn
		s.Keep = true
		if s.Burn {
			s.Default = false
		}
	}
	if s.Keep {
		for j := range e.Subtitles {
			if i == j {
				continue
			}
			if s.Default {
				e.Subtitles[j].Default = false
			}
			if s.Burn {
				e.Subtitles[j].Burn = false
			}
		}
	}
	e.subtitlesChanged = true
	return nil
}

func (e *Entry) exportSubtitles(original json.RawMessage) (json.RawMessage, error) {
	sub := map[string]json.RawMessage{}
	if len(original) > 0 {
		if err := json.Unmarshal(original, &sub); err != nil {
			return nil, err
		}
	}
	if sub == nil {
		sub = map[string]json.RawMessage{}
	}
	list := []json.RawMessage{}
	for _, s := range e.Subtitles {
		if s.Source != -1 && !s.Keep {
			continue
		}
		value := map[string]any{}
		if len(s.Raw) > 0 {
			// Preserve unknown settings, including external subtitle import details.
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(s.Raw, &fields); err != nil {
				return nil, err
			}
			for k, v := range fields {
				value[k] = v
			}
		}
		value["Default"], value["Forced"], value["Burn"] = s.Default, s.Forced, s.Burn
		if s.Source == -1 {
			value["Enable"] = s.Keep
		} else if s.Source >= 0 {
			value["Track"] = s.Source
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		if s.Source == -1 {
			sub["Search"] = raw
		} else {
			list = append(list, raw)
		}
	}
	sub["SubtitleList"], _ = json.Marshal(list)
	return json.Marshal(sub)
}

// Mirror libhb/preset.c's subtitle selection rules for the review defaults.
// Unedited folder jobs still let HandBrake apply the saved preset at execution.
func presetSubtitles(p map[string]any, metadata []SubtitleInfo, audio []AudioInfo, outputs []Output) json.RawMessage {
	result := []map[string]any{}
	search := map[string]any{"Enable": false}
	if len(metadata) == 0 {
		raw, _ := json.Marshal(map[string]any{"Search": search, "SubtitleList": result})
		return raw
	}
	firstAudio := ""
	if len(outputs) > 0 && outputs[0].Source < len(audio) {
		firstAudio = audio[outputs[0].Source].LanguageCode
	}
	langs, _ := p["SubtitleLanguageList"].([]any)
	preferred := "any"
	if len(langs) > 0 {
		preferred = textValue(langs[0])
	}
	foreign, scan := enabled(p["SubtitleAddForeignAudioSubtitle"]), enabled(p["SubtitleAddForeignAudioSearch"])
	if preferred == "any" {
		if firstAudio != "" {
			preferred = firstAudio
			foreign = false
		} else {
			foreign, scan = false, false
		}
	}
	burnMode := textValue(p["SubtitleBurnBehavior"])
	burnForeign := burnMode == "foreign" || burnMode == "foreign_first"
	burnFirst := burnMode == "first" || burnMode == "foreign_first"
	burned := false
	used := map[int]bool{}
	add := func(lang string, first, foreignDefault, foreignBurn bool) {
		for i, s := range metadata {
			if lang != "any" && s.LanguageCode != lang {
				continue
			}
			if used[i] {
				if first {
					break
				}
				continue
			}
			pass := subtitleCanPass(s.Source, textValue(p["FileFormat"]))
			burn := !burned && (burnFirst || foreignBurn || !pass || (s.Source == 0 && enabled(p["SubtitleBurnDVDSub"])) || (s.Source == 6 && enabled(p["SubtitleBurnBDSub"])))
			def := !burn && foreignDefault
			hasDefault := false
			for _, o := range result {
				hasDefault = hasDefault || enabled(o["Default"])
			}
			def = def || (!hasDefault && s.Attributes.Default)
			if !burned || pass {
				row := map[string]any{"Track": i, "Default": def, "Forced": false, "Burn": burn}
				if enabled(p["SubtitleTrackNamePassthru"]) && s.Name != "" {
					row["Name"] = s.Name
				}
				result = append(result, row)
			}
			burned = burned || burn
			burnFirst = burnFirst && !burn
			used[i] = true
			if first {
				break
			}
		}
	}
	if firstAudio != "" && foreign && firstAudio != preferred {
		scan = false
		add(preferred, true, true, burnForeign)
	}
	if firstAudio != "" && scan && firstAudio == preferred {
		burn := burnForeign || burnFirst
		search = map[string]any{"Enable": true, "Default": !burn, "Forced": true, "Burn": burn}
	}
	behavior := textValue(p["SubtitleTrackSelectionBehavior"])
	if behavior == "first" || behavior == "all" {
		if len(langs) == 0 {
			langs = []any{"any"}
		}
		for _, lang := range langs {
			add(textValue(lang), behavior == "first", false, false)
		}
	}
	if enabled(p["SubtitleAddCC"]) {
		for i, s := range metadata {
			if used[i] || (s.Source != 1 && s.Source != 2) {
				continue
			}
			burn := !burned && (burnFirst || !subtitleCanPass(s.Source, textValue(p["FileFormat"])))
			row := map[string]any{"Track": i, "Default": false, "Forced": false, "Burn": burn}
			if s.Name != "" {
				row["Name"] = s.Name
			}
			result = append(result, row)
			break
		}
	}
	raw, _ := json.Marshal(map[string]any{"Search": search, "SubtitleList": result})
	return raw
}

func subtitleCanPass(source int, container string) bool {
	switch strings.TrimPrefix(container, "av_") {
	case "mkv":
		return source >= 0 && source <= 9
	case "mp4":
		return source == 0 || source == 1 || source == 2 || source == 3 || source == 4 || source == 5 || source == 7 || source == 8
	case "mov":
		return source == 1 || source == 2 || source == 3 || source == 4 || source == 5 || source == 7 || source == 8
	}
	return false
}
