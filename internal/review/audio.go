package review

import (
	"encoding/json"
	"fmt"
	"strings"
)

// codec translates decoder names into the codec names used by passthrough.
// HandBrake calls both DTS and DTS-HD "dca"; its numeric Codec distinguishes
// them (HB_ACODEC_DCA / HB_ACODEC_DCA_HD in libhb/handbrake/common.h).
func (a AudioInfo) codec() string {
	switch {
	case a.Codec&0x00040000 != 0:
		return "dtshd"
	case a.Codec&0x00002000 != 0:
		return "dts"
	}
	codec := strings.TrimPrefix(strings.ToLower(a.CodecName), "av_")
	switch {
	case strings.Contains(codec, "dts-hd") || strings.Contains(codec, "dtshd"):
		return "dtshd"
	case codec == "dca":
		// Older/minimal queue metadata may omit Codec. The scan description
		// still distinguishes HD; a user-supplied track Name does not.
		if strings.Contains(strings.ToLower(a.Description), "dts-hd") {
			return "dtshd"
		}
		return "dts"
	case strings.Contains(codec, "truehd"):
		return "truehd"
	case strings.Contains(codec, "e-ac3") || strings.Contains(codec, "e-ac-3"):
		return "eac3"
	case strings.Contains(codec, "ac-3"):
		return "ac3"
	case strings.HasPrefix(codec, "aac"):
		return "aac"
	case strings.HasPrefix(codec, "pcm"):
		return "pcm"
	}
	return codec
}

// Audio templates refer to language, commentary status and the source's ordinal
// within that group, never its absolute track number. Repeated output encoders
// have their own ordinal so two encodings of one source remain distinct.
type audioMatch struct {
	language   string
	commentary bool
	source     int
	encoder    string
	output     int
}

func audioMatches(e *Entry) ([]audioMatch, error) {
	if !e.CompleteMetadata() {
		return nil, fmt.Errorf("missing track/language metadata; rescan first")
	}
	sources := make([]audioMatch, len(e.Metadata))
	counts := map[audioMatch]int{}
	for i, info := range e.Metadata {
		if info.LanguageCode == "" {
			return nil, fmt.Errorf("missing track/language metadata; rescan first")
		}
		group := audioMatch{language: strings.ToLower(info.LanguageCode), commentary: info.Commentary()}
		match := group
		match.source = counts[group]
		counts[group]++
		sources[i] = match
	}
	counts = map[audioMatch]int{}
	matches := make([]audioMatch, len(e.Outputs))
	for i, out := range e.Outputs {
		match := sources[out.Source]
		match.encoder = out.Encoder
		if out.Encoder == "copy" || strings.HasPrefix(out.Encoder, "copy:") {
			match.encoder = "passthrough"
		}
		matches[i] = match
		matches[i].output = counts[match]
		counts[match]++
	}
	return matches, nil
}

// Only encoding controls are copied. Track indices, names, encoder resolution
// (notably passthrough) and any file-specific fields belong to the destination.
var audioSettings = []string{
	"Mixdown", "Samplerate", "Bitrate", "Quality", "CompressionLevel",
	"DRC", "Gain", "DitherMethod", "NormalizeMixLevel",
}

func copyAudioSettings(from, to Output) (Output, error) {
	var template, target map[string]json.RawMessage
	if err := json.Unmarshal(from.Raw, &template); err != nil {
		return Output{}, err
	}
	if err := json.Unmarshal(to.Raw, &target); err != nil {
		return Output{}, err
	}
	for _, key := range audioSettings {
		delete(target, key)
		if value, ok := template[key]; ok {
			target[key] = value
		}
	}
	var err error
	to.Raw, err = json.Marshal(target)
	return to, err
}

// ApplyAudio copies the reference's retained audio layout and encoding controls
// onto matching prepared outputs. A file is changed atomically: if any retained
// output has no counterpart, its existing selections/settings are kept.
func ApplyAudio(reference *Entry, entries []*Entry) (applied, skipped int, err error) {
	var matches []audioMatch
	if reference.Count() > 0 {
		matches, err = audioMatches(reference)
		if err != nil {
			return 0, 0, err
		}
	}
	for _, e := range entries {
		if e == reference {
			continue
		}
		outputs := append([]Output(nil), e.Outputs...)
		keep := make([]bool, len(outputs))
		var targetMatches []audioMatch
		var failure error
		if reference.Count() > 0 {
			targetMatches, failure = audioMatches(e)
		}
		lookup := map[audioMatch]int{}
		for i, match := range targetMatches {
			lookup[match] = i
		}
		for i, retained := range reference.Keep {
			if !retained || failure != nil {
				continue
			}
			match := matches[i]
			j, ok := lookup[match]
			if !ok {
				role := "main"
				if match.commentary {
					role = "commentary"
				}
				failure = fmt.Errorf("no matching %s %s source %d / %s output %d", match.language, role, match.source+1, match.encoder, match.output+1)
				break
			}
			outputs[j], failure = copyAudioSettings(reference.Outputs[i], outputs[j])
			keep[j] = true
		}
		if failure != nil {
			e.AudioWarning = "Batch audio not applied: " + failure.Error()
			skipped++
			continue
		}
		e.Outputs, e.Keep = outputs, keep
		e.outputsChanged = true
		e.AudioWarning = ""
		applied++
	}
	return applied, skipped, nil
}
