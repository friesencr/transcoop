package webui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/friesencr/transcoop/internal/presets"
	"github.com/friesencr/transcoop/internal/review"
)

const maxPresetFile = 1 << 20

// presetReturn sends the browser back to Add jobs with the form state kept.
func presetReturn(r *http.Request, preset, message string) string {
	q := url.Values{}
	for _, key := range []string{"path", "source", "output", "recursive"} {
		if v := r.FormValue(key); v != "" {
			q.Set(key, v)
		}
	}
	if preset != "" {
		q.Set("preset", preset)
	}
	if message != "" {
		q.Set("result", message)
	}
	return "/add?" + q.Encode()
}

func (s *Server) uploadPreset(w http.ResponseWriter, r *http.Request) {
	fail := func(message string) { http.Redirect(w, r, presetReturn(r, "", message), http.StatusSeeOther) }
	if s.PresetDir == "" {
		fail("Preset uploads are disabled on this coordinator.")
		return
	}
	file, header, err := r.FormFile("preset")
	if err != nil {
		fail("Choose a HandBrake preset file (.json).")
		return
	}
	defer file.Close()
	if header.Size > maxPresetFile {
		fail("Preset files are limited to 1 MiB.")
		return
	}
	data, err := io.ReadAll(io.LimitReader(file, maxPresetFile+1))
	if err != nil || len(data) > maxPresetFile {
		fail("Preset files are limited to 1 MiB.")
		return
	}
	names, err := presets.Names(data)
	if err != nil {
		fail(err.Error())
		return
	}
	// HandBrake silently prefers a built-in or GUI preset with the same name,
	// so a clashing upload would encode with different settings than expected.
	taken := map[string]string{}
	if installed, err := review.PresetNames(r.Context(), s.HandBrake); err == nil {
		for _, name := range installed {
			taken[name] = "a preset installed in HandBrake"
		}
	}
	for _, p := range presets.List(s.PresetDir) {
		taken[p.Name] = "an uploaded preset"
	}
	seen := map[string]bool{}
	for _, name := range names {
		if what, ok := taken[name]; ok {
			fail(fmt.Sprintf("“%s” has the same name as %s. Rename it in HandBrake and export it again.", name, what))
			return
		}
		if seen[name] {
			fail(fmt.Sprintf("The file contains “%s” more than once.", name))
			return
		}
		seen[name] = true
	}
	if err = os.MkdirAll(s.PresetDir, 0700); err != nil {
		fail("Cannot store presets: " + err.Error())
		return
	}
	sum := sha256.Sum256(data)
	path := filepath.Join(s.PresetDir, hex.EncodeToString(sum[:8])+".json")
	tmp, err := os.CreateTemp(s.PresetDir, ".upload-*")
	if err != nil {
		fail("Cannot store presets: " + err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if e := tmp.Close(); err == nil {
		err = e
	}
	if err == nil {
		err = checkPresetFile(r.Context(), s.HandBrake, tmp.Name(), names[0])
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		fail(err.Error())
		return
	}
	message := fmt.Sprintf("Uploaded %d preset", len(names))
	if len(names) != 1 {
		message += "s"
	}
	http.Redirect(w, r, presetReturn(r, names[0], message+": "+strings.Join(names, ", ")), http.StatusSeeOther)
}

// checkPresetFile confirms HandBrake can load the uploaded file.
func checkPresetFile(ctx context.Context, handbrake, path, name string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, handbrake, "--preset-import-file", path, "--preset", name, "--preset-export", "transcoop-check").Output()
	if err != nil || !json.Valid(out) {
		return errors.New("HandBrake could not load this preset file; check that it was exported by HandBrake on a similar version")
	}
	return nil
}

func (s *Server) deletePreset(w http.ResponseWriter, r *http.Request) {
	id := r.FormValue("id")
	if s.PresetDir == "" || !presets.FileName.MatchString(id+".json") {
		http.Error(w, "unknown preset", http.StatusBadRequest)
		return
	}
	message := "Removed the uploaded preset file."
	if err := os.Remove(filepath.Join(s.PresetDir, id+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		message = "Cannot remove preset: " + err.Error()
	}
	http.Redirect(w, r, presetReturn(r, "", message), http.StatusSeeOther)
}
