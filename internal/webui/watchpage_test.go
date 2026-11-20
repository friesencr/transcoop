package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/friesencr/transcoop/internal/watch"
)

func TestWatchPage(t *testing.T) {
	s, store := testUI(t)
	s.HandBrake = fakeHandBrake(t)
	s.Watcher = &watch.Watcher{Store: store}
	c, token := authenticated(t, s)
	src, out := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "old.mkv"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if page := request(s, "GET", "/watch?source=/in&preset=Fast+1080p30", nil, c).Body.String(); !strings.Contains(page, `value="/in"`) || !strings.Contains(page, "No watched folders yet") || !strings.Contains(page, `aria-current="page"`) {
		t.Fatal(page)
	}
	form := url.Values{"csrf": {token}, "source": {src}, "output": {out}, "preset": {"Not installed"}}
	if w := request(s, "POST", "/watch", form, c); !strings.Contains(w.Header().Get("Location"), "Choose+a+preset") {
		t.Fatal("unknown preset accepted", w.Header().Get("Location"))
	}
	form.Set("preset", "Fast 1080p30")
	w := request(s, "POST", "/watch", form, c)
	if loc := w.Header().Get("Location"); w.Code != 303 || !strings.Contains(loc, "1+video+already+there") {
		t.Fatal(w.Code, loc)
	}
	if w = request(s, "POST", "/watch", form, c); !strings.Contains(w.Header().Get("Location"), "already+watched") {
		t.Fatal("duplicate watch accepted")
	}
	watches, _ := store.Watches(context.Background())
	if len(watches) != 1 || !watches[0].Enabled {
		t.Fatal(watches)
	}
	files, _ := store.WatchFiles(context.Background(), watches[0].ID)
	if len(files) != 1 {
		t.Fatal("existing file not recorded", files)
	}
	r := httptestHX(s, "/watch", c)
	if !strings.Contains(r, `id="live"`) || !strings.Contains(r, filepath.Base(src)+"/") || strings.Contains(r, "<html") {
		t.Fatal(r)
	}
	id := watches[0].ID
	request(s, "POST", "/watch/"+itoa(id)+"/pause", url.Values{"csrf": {token}}, c)
	if watches, _ = store.Watches(context.Background()); watches[0].Enabled {
		t.Fatal("not paused")
	}
	if w = request(s, "POST", "/watch/"+itoa(id)+"/explode", url.Values{"csrf": {token}}, c); w.Code != 404 {
		t.Fatal(w.Code)
	}
	request(s, "POST", "/watch/"+itoa(id)+"/delete", url.Values{"csrf": {token}}, c)
	if watches, _ = store.Watches(context.Background()); len(watches) != 0 {
		t.Fatal("not deleted")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func httptestHX(s *Server, path string, c *http.Cookie) string {
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("HX-Request", "true")
	addTestCookies(r, c)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w.Body.String()
}
