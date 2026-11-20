package webui

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/friesencr/transcoop/internal/presets"
)

func TestExportPresetNames(t *testing.T) {
	names, err := presets.Names([]byte(`{"PresetList":[{"PresetName":"Folder","Folder":true,"ChildrenArray":[{"PresetName":"Inner"}]},{"PresetName":"Top"}]}`))
	if err != nil || strings.Join(names, ",") != "Inner,Top" {
		t.Fatal(names, err)
	}
	for _, bad := range []string{`nope`, `{"PresetList":[]}`, `{"PresetList":[{"PresetName":"F","Folder":true}]}`} {
		if _, err := presets.Names([]byte(bad)); err == nil {
			t.Error("accepted", bad)
		}
	}
}

// fakeHandBrake lists one built-in preset and exports any preset as JSON.
func fakeHandBrake(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script stand-in for HandBrakeCLI")
	}
	path := filepath.Join(t.TempDir(), "HandBrakeCLI")
	script := "#!/bin/sh\ncase \"$*\" in *--preset-list*) printf 'General/\\n    Fast 1080p30\\n        desc\\n';; *) echo '{}';; esac\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func uploadPresetFile(s *Server, c *http.Cookie, token, body string) *httptest.ResponseRecorder {
	var b bytes.Buffer
	m := multipart.NewWriter(&b)
	_ = m.WriteField("csrf", token)
	_ = m.WriteField("source", "/media/in")
	f, _ := m.CreateFormFile("preset", "preset.json")
	_, _ = f.Write([]byte(body))
	_ = m.Close()
	r := httptest.NewRequest("POST", "/add/presets", &b)
	r.Header.Set("Content-Type", m.FormDataContentType())
	addTestCookies(r, c)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestPresetUploadAndRemoval(t *testing.T) {
	s, _ := testUI(t)
	s.HandBrake = fakeHandBrake(t)
	s.PresetDir = filepath.Join(t.TempDir(), "presets")
	c, token := authenticated(t, s)

	w := uploadPresetFile(s, c, token, `{"PresetList":[{"PresetName":"Fast 1080p30"}]}`)
	if loc := w.Header().Get("Location"); w.Code != 303 || !strings.Contains(loc, "same+name") || !strings.Contains(loc, "source=%2Fmedia%2Fin") {
		t.Fatal("clash with built-in accepted:", w.Code, loc)
	}
	w = uploadPresetFile(s, c, token, `{"PresetList":[{"PresetName":"Anime AV1"},{"PresetName":"Film AV1"}]}`)
	loc := w.Header().Get("Location")
	if w.Code != 303 || !strings.Contains(loc, "preset=Anime+AV1") {
		t.Fatal(w.Code, loc)
	}
	if got := presets.List(s.PresetDir); len(got) != 2 || got[0].Name != "Anime AV1" || got[0].File != got[1].File {
		t.Fatal(got)
	}
	if w = uploadPresetFile(s, c, token, `{"PresetList":[{"PresetName":"Film AV1"}]}`); !strings.Contains(w.Header().Get("Location"), "uploaded+preset") {
		t.Fatal("duplicate upload accepted")
	}
	page := request(s, "GET", loc, nil, c).Body.String()
	if !strings.Contains(page, `<optgroup label="Uploaded">`) || !strings.Contains(page, `<option value="Anime AV1" selected>`) {
		t.Fatal(page)
	}
	id := presets.List(s.PresetDir)[0].ID
	if w = request(s, "POST", "/add/presets/delete", url.Values{"csrf": {token}, "id": {"../farm"}}, c); w.Code != 400 {
		t.Fatal("bad id accepted", w.Code)
	}
	if w = request(s, "POST", "/add/presets/delete", url.Values{"csrf": {token}, "id": {id}}, c); w.Code != 303 || len(presets.List(s.PresetDir)) != 0 {
		t.Fatal("preset not removed", w.Code)
	}
}
