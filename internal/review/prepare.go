package review

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func IsVideoFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mkv", ".mp4", ".m4v", ".avi", ".mov", ".webm", ".wmv", ".mpg", ".mpeg", ".m2ts", ".mts", ".ts", ".vob", ".ogv", ".flv":
		return true
	}
	return false
}

// PresetNames reads HandBrake's installed built-in and GUI presets. Descriptions
// are indented more deeply than the four-space preset names in --preset-list.
func PresetNames(ctx context.Context, executable string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, executable, "--preset-import-gui", "--preset-list").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("cannot list presets; install HandBrakeCLI on the coordinator or set coordinator.handbrake: %w", err)
	}
	names := parsePresetNames(string(out))
	if len(names) == 0 {
		return nil, fmt.Errorf("HandBrake returned no presets")
	}
	return names, nil
}

func parsePresetNames(list string) []string {
	seen := map[string]bool{}
	var names []string
	for _, line := range strings.Split(list, "\n") {
		if !strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "     ") {
			continue
		}
		name := strings.TrimSpace(line)
		if name == "" || strings.HasSuffix(name, "/") || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// PrepareMedia shares the TUI's preset snapshot, scan, track-selection and
// collision checks. It never creates output files or submits jobs.
func PrepareMedia(ctx context.Context, opts Options, limit int) ([]json.RawMessage, error) {
	plan, err := planFolder(ctx, opts)
	if err != nil {
		return nil, err
	}
	if len(plan.files) > limit {
		return nil, fmt.Errorf("selected %d files; choose a smaller batch (maximum %d)", len(plan.files), limit)
	}
	queue := make([]json.RawMessage, 0, len(plan.files))
	for _, file := range plan.files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry, err := prepareFolderFile(ctx, opts.HandBrake, file, plan)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file.source, err)
		}
		raw, err := entry.Export()
		if err != nil {
			return nil, err
		}
		queue = append(queue, raw)
	}
	return queue, nil
}
