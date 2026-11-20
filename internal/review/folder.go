package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/friesencr/transcoop/internal/farm"
)

type folderFile struct{ source, local, destination string }
type folderPlan struct {
	files    []folderFile
	snapshot farm.PresetSnapshot
	preset   map[string]any
}
type folderResult struct {
	entry *Entry
	err   error
}

func commandJSON(ctx context.Context, executable string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		tail := stderr.Bytes()
		if len(tail) > 1200 {
			tail = tail[len(tail)-1200:]
		}
		return nil, fmt.Errorf("HandBrake: %w: %s", err, tail)
	}
	return out, nil
}

func loadPreset(ctx context.Context, opts Options) (farm.PresetSnapshot, map[string]any, error) {
	args := []string{}
	if opts.PresetFile != "" {
		args = append(args, "--preset-import-file", opts.PresetFile)
	} else {
		args = append(args, "--preset-import-gui")
	}
	if opts.Preset != "" {
		args = append(args, "--preset", opts.Preset)
	}
	args = append(args, "--preset-export", "transcoop-snapshot")
	out, err := commandJSON(ctx, opts.HandBrake, args...)
	if err != nil {
		return farm.PresetSnapshot{}, nil, err
	}
	name := opts.Preset
	if name == "" {
		name = "HandBrake default"
	}
	snapshot := farm.PresetSnapshot{Version: 1, Name: name, Export: json.RawMessage(bytes.TrimSpace(out))}
	wrapper, err := json.Marshal(map[string]any{"TranscoopPreset": snapshot})
	if err != nil {
		return snapshot, nil, err
	}
	_, preset, err := farm.ReadPresetSnapshot(wrapper)
	if err != nil {
		return snapshot, nil, err
	}
	// Explicit audio lists bypass HandBrake's template expansion. Reject fields
	// we cannot faithfully express instead of silently losing preset behavior.
	templates, _ := preset["AudioList"].([]any)
	allowed := map[string]bool{}
	for _, k := range strings.Fields("AudioEncoder AudioBitrate AudioCompressionLevel AudioDitherMethod AudioMixdown AudioNormalizeMixLevel AudioSamplerate AudioTrackDRCSlider AudioTrackGainSlider AudioTrackQuality AudioTrackQualityEnable") {
		allowed[k] = true
	}
	for _, v := range templates {
		for k, value := range obj(v) {
			if !allowed[k] {
				if k == "AudioFilterList" {
					if list, ok := value.([]any); ok && len(list) == 0 {
						continue
					}
				}
				return snapshot, nil, fmt.Errorf("folder input cannot preserve audio template field %s; use a GUI queue export for this preset", k)
			}
		}
	}
	return snapshot, preset, nil
}
func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }
func textValue(v any) string   { s, _ := v.(string); return s }
func num(v any) float64        { n, _ := v.(float64); return n }
func enabled(v any) bool       { b, _ := v.(bool); return b }
func outputExtension(p map[string]any) (string, error) {
	switch textValue(p["FileFormat"]) {
	case "av_mkv", "mkv":
		return ".mkv", nil
	case "av_mp4", "mp4":
		return ".mp4", nil
	case "av_webm", "webm":
		return ".webm", nil
	case "av_mov", "mov":
		return ".mov", nil
	}
	return "", fmt.Errorf("unsupported preset container %q", p["FileFormat"])
}
func beneath(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Resolve existing parents too, so a new output directory through a symlink
// cannot point back into the source tree without being recognized.
func physical(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	resolved, err = physical(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(path)), nil
}
func planFolder(ctx context.Context, opts Options) (*folderPlan, error) {
	if opts.OutputDir == "" {
		return nil, errors.New("folder input requires -output-dir")
	}
	if opts.PresetFile != "" && opts.Preset == "" {
		return nil, errors.New("-preset-file requires -preset to select a named preset")
	}
	snapshot, preset, err := loadPreset(ctx, opts)
	if err != nil {
		return nil, err
	}
	ext, err := outputExtension(preset)
	if err != nil {
		return nil, err
	}
	root, err := filepath.Abs(opts.Queue)
	if err != nil {
		return nil, err
	}
	output, err := filepath.Abs(opts.OutputDir)
	if err != nil {
		return nil, err
	}
	localRoot, err := physical(farm.MapPath(root, opts.Mappings))
	if err != nil {
		return nil, err
	}
	localOutput, err := physical(farm.MapPath(output, opts.Mappings))
	if err != nil {
		return nil, err
	}
	if localRoot == localOutput {
		return nil, errors.New("output folder must differ from input folder")
	}
	if info, err := os.Stat(localOutput); err == nil && !info.IsDir() {
		return nil, errors.New("output path is not a folder")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	plan := &folderPlan{snapshot: snapshot, preset: preset}
	destinations := map[string]bool{}
	info, err := os.Stat(localRoot)
	if err != nil {
		return nil, err
	}
	singleFile := !info.IsDir()
	if singleFile && !info.Mode().IsRegular() {
		return nil, errors.New("input must be a regular video file or folder")
	}
	err = filepath.WalkDir(localRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() && path != localRoot && beneath(localRoot, localOutput) && beneath(localOutput, path) {
			return filepath.SkipDir
		}
		disc := d.IsDir() && isDiscDirectory(path)
		if d.IsDir() && !disc {
			if path != localRoot && (!opts.Recursive || (beneath(localRoot, localOutput) && beneath(localOutput, path))) {
				return filepath.SkipDir
			}
			return nil
		}
		if !disc && (!d.Type().IsRegular() || !IsVideoFile(path)) {
			return nil
		}
		rel, err := filepath.Rel(localRoot, path)
		if err != nil {
			return err
		}
		if singleFile || path == localRoot {
			rel = filepath.Base(root)
		}
		outputName := rel
		if !disc {
			outputName = strings.TrimSuffix(rel, filepath.Ext(rel))
		}
		destination := filepath.Join(output, outputName+ext)
		localDest := farm.MapPath(destination, opts.Mappings)
		// Case-folding also catches destinations that collide on Windows/SMB.
		key := strings.ToLower(filepath.Clean(localDest))
		if destinations[key] {
			return fmt.Errorf("multiple inputs target %s; rename an input or use separate batches", destination)
		}
		destinations[key] = true
		if _, err := os.Lstat(localDest); err == nil {
			return fmt.Errorf("output already exists: %s", destination)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		source := filepath.Join(root, rel)
		if singleFile || path == localRoot {
			source = root
		}
		plan.files = append(plan.files, folderFile{source, path, destination})
		if disc {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(plan.files) == 0 {
		return nil, errors.New("no supported video files found in folder")
	}
	sort.Slice(plan.files, func(i, j int) bool { return plan.files[i].source < plan.files[j].source })
	return plan, nil
}

func prepareFolderFile(ctx context.Context, executable string, file folderFile, plan *folderPlan) (*Entry, error) {
	out, err := commandJSON(ctx, executable, "--json", "--scan", "--input", file.local, "--title", "1", "--min-duration", "0")
	if err != nil {
		return nil, err
	}
	title, err := parseTitle(out, 1)
	if err != nil {
		return nil, err
	}
	title["Path"] = file.source
	metadata, _ := json.Marshal(title)
	var info struct{ AudioList []AudioInfo }
	if err = json.Unmarshal(metadata, &info); err != nil {
		return nil, err
	}
	p := plan.preset
	audios := presetAudio(p, info.AudioList)
	job := map[string]any{"Source": map[string]any{"Path": file.source, "Title": 1}, "Destination": map[string]any{"File": file.destination}, "Video": map[string]any{"Encoder": p["VideoEncoder"], "Preset": p["VideoPreset"]}, "Audio": map[string]any{"AudioList": audios}, "Subtitle": map[string]any{}}
	root := map[string]any{"Job": job, "Title": title, "TranscoopPreset": plan.snapshot}
	raw, err := json.Marshal([]any{root})
	if err != nil {
		return nil, err
	}
	entries, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	entries[0].PresetName = plan.snapshot.Name
	return entries[0], nil
}

// Preset language rules select source tracks in language preference order. The
// first fallback is HandBrake's behavior when no preferred language is present.
func presetAudio(p map[string]any, metadata []AudioInfo) []map[string]any {
	result := []map[string]any{}
	behavior := textValue(p["AudioTrackSelectionBehavior"])
	if behavior == "none" {
		return result
	}
	languages, _ := p["AudioLanguageList"].([]any)
	if len(languages) == 0 {
		languages = []any{"any"}
	}
	templates, _ := p["AudioList"].([]any)
	seen := map[int]bool{}
	sources := []int{}
	for _, v := range languages {
		lang := textValue(v)
		for i, a := range metadata {
			if lang != "any" && a.LanguageCode != lang {
				continue
			}
			if !seen[i] {
				sources = append(sources, i)
				seen[i] = true
			}
			if behavior != "all" {
				break
			}
		}
	}
	if len(sources) == 0 && len(metadata) > 0 {
		sources = append(sources, 0)
	}
	for _, track := range sources {
		firstTrack := len(result) == 0
		for index, v := range templates {
			if enabled(p["AudioSecondaryEncoderMode"]) && !firstTrack && index > 0 {
				break
			}
			template := obj(v)
			encoder := textValue(template["AudioEncoder"])
			encoder = resolveAudioEncoder(encoder, metadata[track], p)
			if encoder == "none" || encoder == "" {
				continue
			}
			a := map[string]any{"Track": track, "Encoder": encoder}
			for from, to := range map[string]string{"AudioMixdown": "Mixdown", "AudioSamplerate": "Samplerate", "AudioCompressionLevel": "CompressionLevel", "AudioTrackDRCSlider": "DRC", "AudioTrackGainSlider": "Gain", "AudioDitherMethod": "DitherMethod", "AudioNormalizeMixLevel": "NormalizeMixLevel"} {
				if v, ok := template[from]; ok {
					a[to] = v
				}
			}
			if enabled(template["AudioTrackQualityEnable"]) {
				a["Quality"] = template["AudioTrackQuality"]
			} else {
				a["Bitrate"] = template["AudioBitrate"]
			}
			result = append(result, a)
		}
	}
	return result
}
func resolveAudioEncoder(encoder string, info AudioInfo, p map[string]any) string {
	if !strings.HasPrefix(encoder, "copy") {
		return encoder
	}
	copyCodec := "copy:" + info.codec()
	if encoder == copyCodec {
		return encoder
	}
	if encoder == "copy" {
		mask, _ := p["AudioCopyMask"].([]any)
		for _, v := range mask {
			if textValue(v) == copyCodec {
				return copyCodec
			}
		}
	}
	return textValue(p["AudioEncoderFallback"])
}

func isDiscDirectory(p string) bool {
	for _, name := range []string{"VIDEO_TS", "BDMV"} {
		if info, err := os.Stat(filepath.Join(p, name)); err == nil && info.IsDir() {
			return true
		}
	}
	base := strings.ToUpper(filepath.Base(p))
	if base == "VIDEO_TS" {
		_, err := os.Stat(filepath.Join(p, "VIDEO_TS.IFO"))
		return err == nil
	}
	if base == "BDMV" {
		_, err := os.Stat(filepath.Join(p, "index.bdmv"))
		return err == nil
	}
	return false
}
