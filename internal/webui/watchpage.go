package webui

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/friesencr/transcoop/internal/farm"
	"github.com/friesencr/transcoop/internal/presets"
	"github.com/friesencr/transcoop/internal/review"
	"github.com/friesencr/transcoop/internal/watch"
)

type watchRow struct {
	farm.Watch
	Summary farm.WatchSummary
	Status  watch.Status
	Scanned bool
}

type watchData struct {
	CSRF, Message   string
	Rows            []watchRow
	Presets, Custom []string
	Form            farm.Watch
	IncludeExisting bool
	Running         bool // the watcher is attached to this coordinator
}

func (r watchRow) State() string {
	switch {
	case !r.Enabled:
		return "paused"
	case r.Status.Error != "":
		return "problem"
	}
	return "watching"
}

func (s *Server) watchRows(r *http.Request) ([]watchRow, error) {
	watches, err := s.store.Watches(r.Context())
	if err != nil {
		return nil, err
	}
	summaries, err := s.store.WatchSummaries(r.Context(), 5)
	if err != nil {
		return nil, err
	}
	status := map[int64]watch.Status{}
	if s.Watcher != nil {
		status = s.Watcher.Status()
	}
	rows := make([]watchRow, 0, len(watches))
	for _, wt := range watches {
		st, scanned := status[wt.ID]
		sum := summaries[wt.ID]
		if sum.Counts == nil {
			sum.Counts = map[string]int{}
		}
		rows = append(rows, watchRow{Watch: wt, Summary: sum, Status: st, Scanned: scanned})
	}
	return rows, nil
}

func (s *Server) watchPage(w http.ResponseWriter, r *http.Request) {
	rows, err := s.watchRows(r)
	if err != nil {
		http.Error(w, "watches unavailable", http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	d := watchData{CSRF: s.get(r).CSRF, Message: q.Get("result"), Rows: rows, Running: s.Watcher != nil,
		Form: farm.Watch{Source: q.Get("source"), Output: q.Get("output"), Preset: q.Get("preset"), Recursive: q.Get("recursive") == "on"}}
	if r.Header.Get("HX-Request") == "true" {
		s.render(w, r, watchList(d))
		return
	}
	for _, p := range presets.List(s.PresetDir) {
		d.Custom = append(d.Custom, p.Name)
	}
	d.Presets, err = review.PresetNames(r.Context(), s.HandBrake)
	if err != nil {
		d.Message = strings.TrimSpace(d.Message + " " + err.Error())
	}
	if d.Form.Preset == "" {
		d.Form.Preset = "Fast 1080p30"
	}
	s.render(w, r, watchView(d))
}

func watchReturn(message string, form url.Values) string {
	q := url.Values{"result": {message}}
	for _, k := range []string{"source", "output", "preset", "recursive"} {
		if v := form.Get(k); v != "" {
			q.Set(k, v)
		}
	}
	return "/watch?" + q.Encode()
}

func (s *Server) addWatch(w http.ResponseWriter, r *http.Request) {
	wt := farm.Watch{Source: strings.TrimSpace(r.FormValue("source")), Output: strings.TrimSpace(r.FormValue("output")), Preset: r.FormValue("preset"), Recursive: r.FormValue("recursive") == "on"}
	fail := func(message string) { http.Redirect(w, r, watchReturn(message, r.Form), http.StatusSeeOther) }
	if err := watch.Validate(wt); err != nil {
		fail(err.Error())
		return
	}
	if presets.Find(s.PresetDir, wt.Preset) == nil {
		installed, err := review.PresetNames(r.Context(), s.HandBrake)
		found := false
		for _, name := range installed {
			found = found || name == wt.Preset
		}
		if err != nil || !found {
			fail("Choose a preset installed on this coordinator or uploaded on the Add jobs page.")
			return
		}
	}
	id, err := s.store.AddWatch(r.Context(), wt)
	if err != nil {
		fail(err.Error())
		return
	}
	wt.ID = id
	message := "Watching " + wt.Source + ". New videos are queued once they have stopped changing for a minute."
	if r.FormValue("existing") != "on" {
		n, err := watch.Baseline(r.Context(), s.store, wt)
		if err != nil {
			_ = s.store.DeleteWatch(r.Context(), id)
			fail("Cannot read the folder: " + err.Error())
			return
		}
		if n == 1 {
			message += " 1 video already there was left alone."
		} else if n > 1 {
			message += fmt.Sprintf(" %d videos already there were left alone.", n)
		}
	}
	if s.Watcher != nil {
		s.Watcher.Trigger()
	}
	http.Redirect(w, r, "/watch?"+url.Values{"result": {message}}.Encode(), http.StatusSeeOther)
}

func (s *Server) changeWatch(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var message string
	switch r.PathValue("action") {
	case "pause":
		err, message = s.store.SetWatchEnabled(r.Context(), id, false), "Watch paused."
	case "resume":
		err, message = s.store.SetWatchEnabled(r.Context(), id, true), "Watch resumed."
	case "delete":
		err, message = s.store.DeleteWatch(r.Context(), id), "Stopped watching. Queued jobs and files were kept."
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		message = "Could not change the watch: " + err.Error()
	} else if s.Watcher != nil {
		s.Watcher.Trigger()
	}
	http.Redirect(w, r, "/watch?"+url.Values{"result": {message}}.Encode(), http.StatusSeeOther)
}

func (s *Server) scanWatches(w http.ResponseWriter, r *http.Request) {
	if s.Watcher != nil {
		s.Watcher.Trigger()
	}
	http.Redirect(w, r, "/watch?"+url.Values{"result": {"Scanning now. Files still need a minute without changes before they are queued."}}.Encode(), http.StatusSeeOther)
}
