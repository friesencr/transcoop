package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/friesencr/transcoop/internal/farm"
	"github.com/friesencr/transcoop/internal/presets"
	"github.com/friesencr/transcoop/internal/review"
)

type mediaDraft struct {
	ID    string
	Queue []json.RawMessage
	Jobs  []farm.Job
}
type mediaItem struct {
	Name, Open, Select string
	Directory, Chosen  bool
}
type presetUpload struct {
	ID    string
	Names []string
}
type mediaData struct {
	CSRF, Path, Source, Output, Preset, Message string
	Parent, SelectSource, SelectOutput          string
	Recursive                                   bool
	Items                                       []mediaItem
	Crumbs                                      []crumb
	Custom                                      []string       // uploaded preset names
	Uploaded                                    []presetUpload // uploaded files, for removal
	CanUpload                                   bool
	Presets                                     []string
}

func mediaURL(path, source, output, preset string, recursive bool) string {
	q := url.Values{"path": {path}, "source": {source}, "output": {output}, "preset": {preset}}
	if recursive {
		q.Set("recursive", "on")
	}
	return "/add?" + q.Encode()
}

func (s *Server) addMedia(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	d := mediaData{CSRF: s.get(r).CSRF, Path: q.Get("path"), Source: q.Get("source"), Output: q.Get("output"), Preset: q.Get("preset"), Recursive: q.Get("recursive") == "on", Message: q.Get("result"), CanUpload: s.PresetDir != ""}
	for _, p := range presets.List(s.PresetDir) {
		d.Custom = append(d.Custom, p.Name)
		if n := len(d.Uploaded); n > 0 && d.Uploaded[n-1].ID == p.ID {
			d.Uploaded[n-1].Names = append(d.Uploaded[n-1].Names, p.Name)
		} else {
			d.Uploaded = append(d.Uploaded, presetUpload{ID: p.ID, Names: []string{p.Name}})
		}
	}
	if d.Path == "" {
		d.Path = s.get(r).Browse
	}
	if d.Path == "" {
		if home, err := os.UserHomeDir(); err == nil {
			d.Path = home
		} else {
			d.Path, _ = os.Getwd()
		}
	}
	var err error
	d.Path, err = filepath.Abs(d.Path)
	if err == nil {
		var entries []os.DirEntry
		entries, err = os.ReadDir(d.Path)
		if err == nil {
			d.Parent = mediaURL(filepath.Dir(d.Path), d.Source, d.Output, d.Preset, d.Recursive)
			d.SelectSource = mediaURL(d.Path, d.Path, d.Output, d.Preset, d.Recursive)
			d.SelectOutput = mediaURL(d.Path, d.Source, d.Path, d.Preset, d.Recursive)
			d.Crumbs = s.crumbs(d)
			s.setBrowse(r, d.Path)
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".") {
					continue // hidden files and folders are rarely media
				}
				// Follow directory links when explicitly browsing, but folder discovery
				// does not recursively traverse links (avoids loops and duplicate inputs).
				path := filepath.Join(d.Path, entry.Name())
				info, e := os.Stat(path)
				if e != nil || (!info.IsDir() && (!info.Mode().IsRegular() || !review.IsVideoFile(path))) {
					continue
				}
				item := mediaItem{Name: entry.Name(), Directory: info.IsDir(), Chosen: path == d.Source}
				if item.Directory {
					item.Open = mediaURL(path, d.Source, d.Output, d.Preset, d.Recursive)
				}
				item.Select = mediaURL(d.Path, path, d.Output, d.Preset, d.Recursive)
				d.Items = append(d.Items, item)
			}
		}
	}
	// Folders first, then videos; ReadDir already sorts each group by name.
	sort.SliceStable(d.Items, func(i, j int) bool { return d.Items[i].Directory && !d.Items[j].Directory })
	if err != nil {
		d.Message = strings.TrimSpace(d.Message + " Cannot browse folder: " + err.Error())
	}
	d.Presets, err = review.PresetNames(r.Context(), s.HandBrake)
	if err != nil {
		d.Message = strings.TrimSpace(d.Message + " " + err.Error())
	}
	if d.Preset == "" {
		for _, name := range d.Presets {
			if name == "Fast 1080p30" {
				d.Preset = name
				break
			}
		}
		if d.Preset == "" && len(d.Presets) > 0 {
			d.Preset = d.Presets[0]
		}
	}
	s.render(w, r, mediaView(d))
}

// crumbs links each ancestor of the browsed folder.
func (s *Server) crumbs(d mediaData) []crumb {
	var out []crumb
	for p := d.Path; ; p = filepath.Dir(p) {
		name := filepath.Base(p)
		if p == filepath.Dir(p) {
			name = p
		}
		out = append([]crumb{{Name: name, URL: mediaURL(p, d.Source, d.Output, d.Preset, d.Recursive)}}, out...)
		if p == filepath.Dir(p) {
			return out
		}
	}
}

func (s *Server) setBrowse(r *http.Request, path string) {
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if session := s.sessions[cookie.Value]; session != nil {
		session.Browse = path
	}
}

func (s *Server) setMedia(r *http.Request, draft *mediaDraft) {
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if session := s.sessions[cookie.Value]; session != nil {
		session.Media = draft
	}
}

func (s *Server) mediaResult(w http.ResponseWriter, r *http.Request, draft *mediaDraft, count int, message string) {
	if r.Header.Get("HX-Request") == "true" {
		s.render(w, r, mediaPreview(s.get(r).CSRF, draft, count, message))
	} else {
		s.render(w, r, mediaPreviewPage(s.get(r).CSRF, draft, count, message))
	}
}

func (s *Server) previewMedia(w http.ResponseWriter, r *http.Request) {
	s.setMedia(r, nil)
	select {
	case s.mediaScans <- struct{}{}:
		defer func() { <-s.mediaScans }()
	default:
		s.mediaResult(w, r, nil, 0, "Other media scans are in progress. Try again shortly.")
		return
	}
	source, output, preset := strings.TrimSpace(r.FormValue("source")), strings.TrimSpace(r.FormValue("output")), r.FormValue("preset")
	if !filepath.IsAbs(source) || !filepath.IsAbs(output) || preset == "" {
		s.mediaResult(w, r, nil, 0, "Choose an absolute input path, an absolute output folder, and a preset.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	// Only allow presets actually present on this coordinator. HandBrake can
	// otherwise fall back to defaults for a misspelled preset name.
	opts := review.Options{Queue: source, OutputDir: output, Preset: preset, HandBrake: s.HandBrake, Recursive: r.FormValue("recursive") == "on"}
	var err error
	if custom := presets.Find(s.PresetDir, preset); custom != nil {
		opts.PresetFile = custom.File
	} else {
		var installed []string
		installed, err = review.PresetNames(ctx, s.HandBrake)
		found := false
		for _, name := range installed {
			if name == preset {
				found = true
				break
			}
		}
		if err == nil && !found {
			err = errors.New("choose a preset installed on this coordinator or uploaded here")
		}
	}
	var queue []json.RawMessage
	if err == nil {
		queue, err = review.PrepareMedia(ctx, opts, 500)
	}
	var jobs []farm.Job
	var count int
	if err == nil {
		jobs, count, err = s.store.Preview(ctx, queue)
	}
	if err != nil {
		s.mediaResult(w, r, nil, 0, err.Error())
		return
	}
	draft := &mediaDraft{ID: nonce(), Queue: queue, Jobs: jobs}
	s.setMedia(r, draft)
	s.mediaResult(w, r, draft, count, "")
}

func (s *Server) submitMedia(w http.ResponseWriter, r *http.Request) {
	v := s.get(r)
	draft := v.Media
	if draft == nil || draft.ID != r.FormValue("draft") {
		s.mediaResult(w, r, nil, 0, "This preview has expired or was replaced. Preview the media again.")
		return
	}
	// Check the filesystem again after the operator's review. Import handles
	// concurrent queue changes atomically and rejects destination conflicts.
	for _, job := range draft.Jobs {
		if _, err := os.Lstat(job.Destination); !errors.Is(err, os.ErrNotExist) {
			message := "Output already exists: " + job.Destination
			if err != nil {
				message = "Cannot check output: " + err.Error()
			}
			s.setMedia(r, nil)
			s.mediaResult(w, r, nil, 0, message)
			return
		}
	}
	n, err := s.store.Import(r.Context(), draft.Queue)
	if err != nil {
		s.mediaResult(w, r, draft, 0, err.Error())
		return
	}
	// Do not clear a newer preview produced by a different tab.
	cookie, _ := r.Cookie(cookieName)
	s.mu.Lock()
	if session := s.sessions[cookie.Value]; session != nil && session.Media == draft {
		session.Media = nil
	}
	s.mu.Unlock()
	http.Redirect(w, r, "/jobs?result="+url.QueryEscape(fmt.Sprintf("Added %d jobs; skipped %d duplicates", n, len(draft.Queue)-n)), http.StatusSeeOther)
}

// watchLink opens the Watch page with this form's folders and preset.
func watchLink(d mediaData) string {
	q := url.Values{}
	for k, v := range map[string]string{"source": d.Source, "output": d.Output, "preset": d.Preset} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if d.Recursive {
		q.Set("recursive", "on")
	}
	return "/watch?" + q.Encode()
}
