package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMediaDraftSubmission(t *testing.T) {
	s, store := testUI(t)
	c, token := authenticated(t, s)
	dir := t.TempDir()
	output := filepath.Join(dir, "output.mp4")
	raw, _ := json.Marshal(map[string]any{"Job": map[string]any{"Source": map[string]string{"Path": filepath.Join(dir, "source.mp4")}, "Destination": map[string]string{"File": output}, "Video": map[string]string{"Encoder": "x264"}}})
	jobs, _, err := store.Preview(context.Background(), []json.RawMessage{raw})
	if err != nil {
		t.Fatal(err)
	}
	draft := &mediaDraft{ID: "reviewed-draft", Queue: []json.RawMessage{raw}, Jobs: jobs}
	r := httptest.NewRequest("GET", "/add", nil)
	r.AddCookie(c)
	s.setMedia(r, draft)
	form := url.Values{"draft": {draft.ID}}
	if w := request(s, "POST", "/add/submit", form, c); w.Code != http.StatusForbidden {
		t.Fatal(w.Code)
	}
	form.Set("csrf", token)
	form.Set("draft", "stale")
	w := request(s, "POST", "/add/submit", form, c)
	if !strings.Contains(w.Body.String(), "expired or was replaced") {
		t.Fatal(w.Body.String())
	}
	list, _ := store.List(context.Background())
	if len(list) != 0 {
		t.Fatal("submitted unreviewed jobs")
	}
	form.Set("draft", draft.ID)
	if err := os.WriteFile(output, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	w = request(s, "POST", "/add/submit", form, c)
	if !strings.Contains(w.Body.String(), "Output already exists") {
		t.Fatal(w.Body.String())
	}
	if err := os.Remove(output); err != nil {
		t.Fatal(err)
	}
	s.setMedia(r, draft)
	w = request(s, "POST", "/add/submit", form, c)
	if w.Code != http.StatusSeeOther {
		t.Fatal(w.Code, w.Body.String())
	}
	list, _ = store.List(context.Background())
	if len(list) != 1 {
		t.Fatal(list)
	}
	w = request(s, "POST", "/add/submit", form, c)
	if !strings.Contains(w.Body.String(), "expired or was replaced") {
		t.Fatal("draft reused")
	}
}

func TestMediaBrowserAndValidation(t *testing.T) {
	s, _ := testUI(t)
	s.HandBrake = filepath.Join(t.TempDir(), "missing-HandBrake")
	c, token := authenticated(t, s)
	dir := t.TempDir()
	for _, name := range []string{"video & test.mp4", "private.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	w := request(s, "GET", mediaURL(dir, "", "", "", false), nil, c)
	if !strings.Contains(w.Body.String(), "video &amp; test.mp4") || strings.Contains(w.Body.String(), "private.txt") || !strings.Contains(w.Body.String(), "install HandBrakeCLI") {
		t.Fatal(w.Body.String())
	}
	if w := request(s, "POST", "/add/preview", nil, c); w.Code != 403 {
		t.Fatal(w.Code)
	}
	w = request(s, "POST", "/add/preview", url.Values{"csrf": {token}, "source": {"relative"}}, c)
	if !strings.Contains(w.Body.String(), "Choose an absolute input path") {
		t.Fatal(w.Body.String())
	}
}

func TestRealMediaPreview(t *testing.T) {
	if os.Getenv("TRANSCOOP_TEST_MEDIA") == "" {
		t.Skip("set TRANSCOOP_TEST_MEDIA=1 to test installed HandBrake")
	}
	s, store := testUI(t)
	c, token := authenticated(t, s)
	dir := t.TempDir()
	clip, err := os.ReadFile("../../cmd/transcoop/testdata/encoder-probe.mp4")
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "source.mp4")
	if err := os.WriteFile(source, clip, 0600); err != nil {
		t.Fatal(err)
	}
	w := request(s, "POST", "/add/preview", url.Values{"csrf": {token}, "source": {source}, "output": {filepath.Join(dir, "encoded")}, "preset": {"Fast 1080p30"}}, c)
	if !strings.Contains(w.Body.String(), "Add jobs to queue") {
		t.Fatal(w.Code, w.Body.String())
	}
	list, _ := store.List(context.Background())
	if len(list) != 0 {
		t.Fatal("preview submitted jobs")
	}
	r := httptest.NewRequest("GET", "/add", nil)
	addTestCookies(r, c)
	draft := s.get(r).Media
	if draft == nil || len(draft.Jobs) != 1 || draft.Jobs[0].Source != source {
		t.Fatal(draft)
	}
	w = request(s, "POST", "/add/submit", url.Values{"csrf": {token}, "draft": {draft.ID}}, c)
	if w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	list, _ = store.List(context.Background())
	if len(list) != 1 {
		t.Fatal(list)
	}
}
