// Package presets reads HandBrake preset exports uploaded to the coordinator.
package presets

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Preset is a named preset inside an uploaded HandBrake export file.
type Preset struct {
	Name, File, ID string
}

// FileName matches stored uploads: a content hash and .json.
var FileName = regexp.MustCompile(`^[0-9a-f]{16}\.json$`)

// Names lists the presets in a HandBrake preset export, looking inside
// folders. HandBrake selects presets by name alone.
func Names(data []byte) ([]string, error) {
	var export struct {
		PresetList []json.RawMessage
	}
	if err := json.Unmarshal(data, &export); err != nil {
		return nil, fmt.Errorf("not a HandBrake preset export: %w", err)
	}
	var names []string
	var walk func([]json.RawMessage) error
	walk = func(list []json.RawMessage) error {
		for _, raw := range list {
			var p struct {
				PresetName    string
				Folder        bool
				ChildrenArray []json.RawMessage
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				return fmt.Errorf("not a HandBrake preset export: %w", err)
			}
			if p.Folder {
				if err := walk(p.ChildrenArray); err != nil {
					return err
				}
			} else if name := strings.TrimSpace(p.PresetName); name != "" {
				names = append(names, name)
			}
		}
		return nil
	}
	if err := walk(export.PresetList); err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, errors.New("the file contains no presets; export one from HandBrake with Presets → Export")
	}
	return names, nil
}

// List reads every uploaded preset in dir, sorted by name.
func List(dir string) []Preset {
	if dir == "" {
		return nil
	}
	entries, _ := os.ReadDir(dir)
	var out []Preset
	for _, e := range entries {
		if !FileName.MatchString(e.Name()) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		names, err := Names(data)
		if err != nil {
			continue
		}
		for _, name := range names {
			out = append(out, Preset{Name: name, File: path, ID: strings.TrimSuffix(e.Name(), ".json")})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Find returns the uploaded preset with this name, or nil.
func Find(dir, name string) *Preset {
	for _, p := range List(dir) {
		if p.Name == name {
			return &p
		}
	}
	return nil
}
