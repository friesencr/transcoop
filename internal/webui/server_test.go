package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/friesencr/transcoop/internal/farm"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const testToken = "test-farm-token-super-secret"

func testUI(t *testing.T) (*Server, *farm.Store) {
	t.Helper()
	s, err := farm.OpenStore(filepath.Join(t.TempDir(), "farm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return New(s), s
}
func request(s *Server, method, path string, form url.Values, c *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	if method == "POST" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if c != nil {
		addTestCookies(r, c)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func csrf(w *httptest.ResponseRecorder) string {
	m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(w.Body.String())
	if len(m) != 2 {
		return ""
	}
	return m[1]
}
func authenticated(t *testing.T, s *Server) (*http.Cookie, string) {
	t.Helper()
	w := request(s, "GET", "/pair", nil, nil)
	c := w.Result().Cookies()[0]
	v := csrf(w)
	if v == "" {
		t.Fatal("missing CSRF")
	}
	pair, err := s.store.CreatePairing(context.Background(), "browser")
	if err != nil {
		t.Fatal(err)
	}
	w = request(s, "POST", "/pair", url.Values{"csrf": {v}, "code": {pair.Code}, "name": {"test browser"}}, c)
	if w.Code != 303 {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	var device *http.Cookie
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == cookieName {
			c = cookie
		}
		if cookie.Name == "transcoop_device" {
			device = cookie
		}
	}
	testDeviceCookies[c.Value] = device
	w = request(s, "GET", "/", nil, c)
	return c, csrf(w)
}
func TestSessionsAndCSRF(t *testing.T) {
	s, _ := testUI(t)
	c, token := authenticated(t, s)
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Secure || c.Value == testToken {
		t.Fatal("unsafe session cookie")
	}
	for _, path := range []string{"/logout", "/import", "/jobs/1/retry"} {
		if w := request(s, "POST", path, nil, c); w.Code != 403 {
			t.Fatalf("CSRF %s: %d", path, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "/logout", strings.NewReader(url.Values{"csrf": {token}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://evil.example")
	addTestCookies(r, c)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross origin accepted")
	}
	if w = request(s, "POST", "/logout", url.Values{"csrf": {token}}, c); w.Code != 303 {
		t.Fatal(w.Code)
	}
	if w = request(s, "GET", "/jobs", nil, c); w.Code != 303 {
		t.Fatal("logout did not revoke")
	}
	c, _ = authenticated(t, s)
	s.now = func() time.Time { return time.Now().Add(9 * time.Hour) }
	if w = request(s, "GET", "/", nil, c); w.Code != 200 {
		t.Fatal("paired browser did not renew expired session")
	}
}
func TestAPISessionSeparationAndRestart(t *testing.T) {
	s, store := testUI(t)
	c, _ := authenticated(t, s)
	r := httptest.NewRequest("GET", "/api/jobs", nil)
	addTestCookies(r, c)
	w := httptest.NewRecorder()
	(&farm.Server{Store: store}).Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("cookie authorized API")
	}
	r = httptest.NewRequest("GET", "/jobs", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 303 {
		t.Fatal("bearer authorized UI")
	}
	if request(New(store), "GET", "/", nil, c).Code != 200 {
		t.Fatal("restart lost paired browser")
	}
}
func uiQueue(t *testing.T, n int) []json.RawMessage {
	t.Helper()
	q := []json.RawMessage{}
	for i := 0; i < n; i++ {
		b, _ := json.Marshal(map[string]any{"Job": map[string]any{"Source": map[string]string{"Path": testRoot + fmt.Sprintf("/source/<script>alert(1)</script>-%d.mkv", i)}, "Destination": map[string]string{"File": testRoot + fmt.Sprintf("/output/%d.mkv", i)}, "Video": map[string]string{"Encoder": "x264"}}})
		q = append(q, b)
	}
	return q
}
func TestPagesFilteringAndEscaping(t *testing.T) {
	s, store := testUI(t)
	if _, err := store.Import(context.Background(), uiQueue(t, 30)); err != nil {
		t.Fatal(err)
	}
	c, _ := authenticated(t, s)
	for _, path := range []string{"/", "/jobs", "/workers", "/jobs/1", "/import"} {
		w := request(s, "GET", path, nil, c)
		if w.Code != 200 || strings.Contains(w.Body.String(), "<script>alert") || strings.Contains(w.Body.String(), testToken) || strings.Contains(w.Body.String(), ">else ") {
			t.Fatalf("bad page %s: %d", path, w.Code)
		}
	}
	w := request(s, "GET", "/jobs?q=script&status=pending&page=2", nil, c)
	if !strings.Contains(w.Body.String(), "30 jobs · page 2") || !strings.Contains(w.Body.String(), "page=1&amp;q=script&amp;status=pending") {
		t.Fatal(w.Body.String())
	}
	w = request(s, "GET", "/jobs?worker=nobody", nil, c)
	if !strings.Contains(w.Body.String(), "No matching jobs") {
		t.Fatal("worker filter")
	}
	j, err := store.Claim(context.Background(), "worker", []string{"x264"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.PutTelemetry(context.Background(), j.ID, j.Token, farm.JobTelemetry{Phase: "encoding", Tail: "<img src=x onerror=alert(1)> " + j.Token}); err != nil {
		t.Fatal(err)
	}
	w = request(s, "GET", "/jobs/1", nil, c)
	if strings.Contains(w.Body.String(), "<img src=x") || strings.Contains(w.Body.String(), testToken) || strings.Contains(w.Body.String(), j.Token) {
		t.Fatal("unescaped log or secret")
	}
}
func TestPreviewAndSubmission(t *testing.T) {
	s, store := testUI(t)
	c, token := authenticated(t, s)
	preview := func(data []byte) *httptest.ResponseRecorder {
		var b bytes.Buffer
		m := multipart.NewWriter(&b)
		_ = m.WriteField("csrf", token)
		f, _ := m.CreateFormFile("queue", "queue.json")
		_, _ = f.Write(data)
		m.Close()
		r := httptest.NewRequest("POST", "/import/preview", &b)
		r.Header.Set("Content-Type", m.FormDataContentType())
		addTestCookies(r, c)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	q := uiQueue(t, 2)
	b, _ := json.Marshal(q)
	w := preview(b)
	if !strings.Contains(w.Body.String(), "Submit queue") {
		t.Fatal(w.Body.String())
	}
	jobs, _ := store.List(context.Background())
	if len(jobs) != 0 {
		t.Fatal("preview mutated database")
	}
	w = request(s, "POST", "/import", url.Values{"csrf": {token}}, c)
	if w.Code != 303 {
		t.Fatal(w.Body.String())
	}
	jobs, _ = store.List(context.Background())
	if len(jobs) != 2 {
		t.Fatal("submission missing")
	}
	w = preview(b)
	if !strings.Contains(w.Body.String(), "0 new · 2 duplicates") {
		t.Fatal("duplicate preview")
	}
	w = preview([]byte(`{"invalid":true}`))
	if strings.Contains(w.Body.String(), "Submit queue") {
		t.Fatal("malformed preview accepted")
	}
	w = request(s, "POST", "/import", url.Values{"csrf": {token}}, c)
	if !strings.Contains(w.Body.String(), "queue must contain") {
		t.Fatal("old preview retained")
	}
}

func TestSecureCookiesAndLoginCSRF(t *testing.T) {
	s, _ := testUI(t)
	if w := request(s, "POST", "/pair", url.Values{"token": {testToken}}, nil); w.Code != 403 {
		t.Fatal("login CSRF accepted")
	}
	s.SecureCookies = true
	w := request(s, "GET", "/pair", nil, nil)
	if !w.Result().Cookies()[0].Secure {
		t.Fatal("proxy HTTPS cookie missing Secure")
	}
	s.SecureCookies = false
	r := httptest.NewRequest("GET", "https://farm.example/pair", nil)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if !w.Result().Cookies()[0].Secure {
		t.Fatal("TLS cookie missing Secure")
	}
}

func TestPreviewDestinationConflictAndFormAttributes(t *testing.T) {
	s, store := testUI(t)
	q := uiQueue(t, 1)
	if _, err := store.Import(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	conflict := json.RawMessage(strings.Replace(string(q[0]), "x264", "x265", 1))
	if _, _, err := store.Preview(context.Background(), []json.RawMessage{conflict}); err == nil {
		t.Fatal("preview missed destination conflict")
	}
	jobs, _ := store.List(context.Background())
	if len(jobs) != 1 || jobs[0].Encoder != "x264" {
		t.Fatal("preview changed jobs")
	}
	c, _ := authenticated(t, s)
	w := request(s, "GET", "/jobs", nil, c)
	if strings.Contains(w.Body.String(), `selected="false"`) || strings.Contains(w.Body.String(), ` selected`) {
		t.Fatal("incorrect default filter selection")
	}
	w = request(s, "GET", "/jobs/1", nil, c)
	if strings.Contains(w.Body.String(), `/jobs/1/retry`) {
		t.Fatal("retry offered for a pending job")
	}
}

var testDeviceCookies = map[string]*http.Cookie{}

func addTestCookies(r *http.Request, c *http.Cookie) {
	r.AddCookie(c)
	if d := testDeviceCookies[c.Value]; d != nil {
		r.AddCookie(d)
	}
}

func TestDeviceManagementAndRemoteRevocation(t *testing.T) {
	s, store := testUI(t)
	c, csrfToken := authenticated(t, s)
	w := request(s, "POST", "/devices/pair", url.Values{"csrf": {csrfToken}, "role": {"worker"}}, c)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "worker pairing code:") {
		t.Fatal(w.Code, w.Body.String())
	}
	code := regexp.MustCompile(`worker pairing code:</p><p class="pair-code">([A-Z0-9]{8})<`).FindStringSubmatch(w.Body.String())
	if len(code) != 2 {
		t.Fatal("missing code")
	}
	e, err := store.Redeem(context.Background(), "worker", code[1], "worker")
	if err != nil {
		t.Fatal(err)
	}
	w = request(s, "GET", "/devices", nil, c)
	if w.Code != 200 || strings.Contains(w.Body.String(), e.Token) {
		t.Fatal("device page leaked credential")
	}
	w = request(s, "POST", "/devices/revoke", url.Values{"csrf": {csrfToken}, "id": {e.Device.ID}}, c)
	if w.Code != 303 {
		t.Fatal(w.Code)
	}
	if _, err = store.Authenticate(context.Background(), e.Token); err == nil {
		t.Fatal("revocation ineffective")
	}
	d, err := store.Authenticate(context.Background(), testDeviceCookies[c.Value].Value)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Revoke(context.Background(), d.ID); err != nil {
		t.Fatal(err)
	}
	if w = request(s, "GET", "/", nil, c); w.Code != 303 {
		t.Fatal("revoked browser accepted")
	}
}

func TestClearQueue(t *testing.T) {
	s, store := testUI(t)
	raw := json.RawMessage(`{"Job":{"Source":{"Path":"` + testRoot + `/source.mp4"},"Destination":{"File":"` + testRoot + `/output.mp4"},"Video":{"Encoder":"x264"}}}`)
	if _, err := store.Import(context.Background(), []json.RawMessage{raw}); err != nil {
		t.Fatal(err)
	}
	c, token := authenticated(t, s)
	page := request(s, "GET", "/jobs", nil, c)
	if !strings.Contains(page.Body.String(), "Clear queue") || !strings.Contains(page.Body.String(), "data-confirm=") {
		t.Fatal("missing clear control or confirmation")
	}
	bad := request(s, "POST", "/jobs/clear", nil, c)
	if bad.Code != http.StatusForbidden {
		t.Fatal(bad.Code)
	}
	jobs, err := store.List(context.Background())
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	good := request(s, "POST", "/jobs/clear", url.Values{"csrf": {token}}, c)
	if good.Code != http.StatusSeeOther {
		t.Fatal(good.Code, good.Body.String())
	}
	jobs, err = store.List(context.Background())
	if err != nil || len(jobs) != 0 {
		t.Fatal(jobs, err)
	}
}

func TestExecutionControls(t *testing.T) {
	s, store := testUI(t)
	c, token := authenticated(t, s)
	ctx := context.Background()
	code, err := store.CreatePairing(ctx, "worker")
	if err != nil {
		t.Fatal(err)
	}
	worker, err := store.Redeem(ctx, "worker", code.Code, "worker")
	if err != nil {
		t.Fatal(err)
	}
	for _, device := range []string{"", worker.Device.ID} {
		for _, action := range []string{"pause", "stop", "play"} {
			form := url.Values{"device": {device}, "action": {action}}
			if w := request(s, "POST", "/controls", form, c); w.Code != 403 {
				t.Fatal(w.Code)
			}
			form.Set("csrf", token)
			if w := request(s, "POST", "/controls", form, c); w.Code != 303 {
				t.Fatal(w.Code, w.Body.String())
			}
			states, err := store.Controls(ctx)
			want := map[string]string{"pause": "paused", "stop": "stopped", "play": "playing"}[action]
			if err != nil || states[device] != want {
				t.Fatal(states, err)
			}
		}
	}
	w := request(s, "POST", "/controls", url.Values{"csrf": {token}, "action": {"pause"}, "next": {"/workers?x=1"}}, c)
	if got := w.Header().Get("Location"); got != "/workers?x=1" {
		t.Fatal("next ignored:", got)
	}
	for _, next := range []string{"//evil.example", "https://evil.example", "/\\evil"} {
		w = request(s, "POST", "/controls", url.Values{"csrf": {token}, "action": {"play"}, "next": {next}}, c)
		if got := w.Header().Get("Location"); got != "/jobs" {
			t.Fatalf("unsafe next %q redirected to %q", next, got)
		}
	}
	w = request(s, "POST", "/controls", url.Values{"csrf": {token}, "action": {"garbage"}}, c)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
}

func TestWorkerRank(t *testing.T) {
	s, store := testUI(t)
	c, token := authenticated(t, s)
	ctx := context.Background()
	code, err := store.CreatePairing(ctx, "worker")
	if err != nil {
		t.Fatal(err)
	}
	worker, err := store.Redeem(ctx, "worker", code.Code, "gizmo")
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"device": {worker.Device.ID}, "rank": {"first"}, "next": {"/"}}
	if w := request(s, "POST", "/workers/rank", form, c); w.Code != 403 {
		t.Fatal("rank changed without CSRF", w.Code)
	}
	form.Set("csrf", token)
	if w := request(s, "POST", "/workers/rank", form, c); w.Code != 303 || w.Header().Get("Location") != "/" {
		t.Fatal(w.Code, w.Header().Get("Location"))
	}
	if ranks, _ := store.Ranks(ctx); ranks[worker.Device.ID] != farm.RankFirst {
		t.Fatal(ranks)
	}
	form.Set("rank", "urgent")
	if w := request(s, "POST", "/workers/rank", form, c); w.Code != 400 {
		t.Fatal("bad rank accepted", w.Code)
	}
}
