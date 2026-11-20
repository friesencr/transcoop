package farm

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// HandBrakeVersion runs `executable --version` and returns only its version
// line, such as "HandBrake 1.11.2". HandBrakeCLI also prints libhb startup
// diagnostics, which are discarded.
func HandBrakeVersion(ctx context.Context, executable string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, executable, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("HandBrake version check: %w", err)
	}
	v := ParseHandBrakeVersion(string(out))
	if v == "" {
		return "", fmt.Errorf("HandBrake version check: no version in output")
	}
	return v, nil
}

// ParseHandBrakeVersion extracts the "HandBrake <version>" line from --version output.
func ParseHandBrakeVersion(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "HandBrake" && fields[1] != "has" {
			return line
		}
	}
	return ""
}

// VersionWarnings describes version differences among reporting workers and
// the coordinator. Offline workers are ignored.
func VersionWarnings(workers []WorkerStatus, coordinator string) []string {
	handbrake := map[string][]string{}
	var warnings []string
	for _, w := range workers {
		if w.Presence() == "offline" {
			continue
		}
		if w.HandBrake != "" {
			handbrake[w.HandBrake] = append(handbrake[w.HandBrake], w.Name)
		}
		if coordinator != "" && w.Version != "" && w.Version != coordinator {
			warnings = append(warnings, fmt.Sprintf("Worker %s runs Transcoop %s but the coordinator runs %s. Upgrade all machines together.", w.Name, w.Version, coordinator))
		}
	}
	if len(handbrake) > 1 {
		parts := []string{}
		for v, names := range handbrake {
			parts = append(parts, fmt.Sprintf("%s (%s)", v, strings.Join(names, ", ")))
		}
		sort.Strings(parts)
		warnings = append(warnings, "Workers run different HandBrake versions: "+strings.Join(parts, "; ")+". Use one version across the farm so presets encode consistently.")
	}
	sort.Strings(warnings)
	return warnings
}
