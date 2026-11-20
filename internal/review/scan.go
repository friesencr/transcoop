package review

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"time"
)

// Scan reads HandBrake's labelled JSON output. No output path is supplied and
// --scan prevents encoding. The title is the one selected in the imported job.
func Scan(ctx context.Context, executable, source string, title int) ([]AudioInfo, error) {
	info, err := scanTracks(ctx, executable, source, title)
	return info.AudioList, err
}

type scannedTracks struct {
	AudioList    []AudioInfo
	SubtitleList []SubtitleInfo
}

func scanTracks(ctx context.Context, executable, source string, title int) (scannedTracks, error) {
	if title < 1 {
		title = 1
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "--json", "--scan", "--input", source, "--title", strconv.Itoa(title), "--min-duration", "0")
	// HandBrake logs on stderr while streaming JSON on stdout. Combining the
	// streams can splice a log line into the JSON object.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		out = stderr.Bytes()
		if len(out) > 1200 {
			out = out[len(out)-1200:]
		}
		return scannedTracks{}, fmt.Errorf("scan: %w: %s", err, out)
	}
	item, err := parseTitle(out, title)
	if err != nil {
		return scannedTracks{}, err
	}
	raw, err := json.Marshal(item)
	if err != nil {
		return scannedTracks{}, err
	}
	var info scannedTracks
	err = json.Unmarshal(raw, &info)
	return info, err
}

func parseTitle(data []byte, title int) (map[string]any, error) {
	marker := []byte("JSON Title Set:")
	i := bytes.Index(data, marker)
	if i < 0 {
		return nil, fmt.Errorf("HandBrake returned no JSON title set")
	}
	var result struct{ TitleList []map[string]any }
	if err := json.NewDecoder(bytes.NewReader(data[i+len(marker):])).Decode(&result); err != nil {
		return nil, err
	}
	for _, item := range result.TitleList {
		if num(item["Index"]) == float64(title) {
			return item, nil
		}
	}
	return nil, fmt.Errorf("HandBrake did not find title %d", title)
}
func ParseScan(data []byte, title int) ([]AudioInfo, error) {
	item, err := parseTitle(data, title)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(item["AudioList"])
	if err != nil {
		return nil, err
	}
	var audio []AudioInfo
	err = json.Unmarshal(raw, &audio)
	return audio, err
}
