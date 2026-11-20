// Package webui provides the embedded operator UI; API authentication stays in farm.
package webui

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a-h/templ"
	"github.com/friesencr/transcoop/internal/farm"
	"github.com/friesencr/transcoop/internal/watch"
)

//go:generate go run github.com/a-h/templ/cmd/templ@v0.3.960 generate
//go:embed assets/*
var assets embed.FS

const cookieName = "transcoop_session"

type session struct {
	CSRF    string
	Expires time.Time
	Auth    bool
	Queue   []json.RawMessage
	Media   *mediaDraft
	Browse  string // last folder browsed on the Add jobs page
}
type Server struct {
	// SecureCookies enables HTTPS cookies behind a trusted TLS reverse proxy.
	SecureCookies bool
	HandBrake     string
	Version       string // coordinator's Transcoop version, compared with workers'
	PresetDir     string // uploaded HandBrake preset files; empty disables uploads
	Watcher       *watch.Watcher
	mediaScans    chan struct{}
	store         *farm.Store
	mu            sync.Mutex
	sessions      map[string]*session
	now           func() time.Time
}

func New(store *farm.Store) *Server {
	return &Server{HandBrake: "HandBrakeCLI", mediaScans: make(chan struct{}, 2), store: store, sessions: map[string]*session{}, now: time.Now}
}
func nonce() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	files, _ := fs.Sub(assets, "assets")
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(files))))
	mux.HandleFunc("GET /pair", s.pairPage)
	mux.HandleFunc("POST /pair", s.pair)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /devices", s.devices)
	mux.HandleFunc("POST /devices/pair", s.createPair)
	mux.HandleFunc("POST /devices/revoke", s.revokeDevice)
	mux.HandleFunc("GET /{$}", s.page)
	mux.HandleFunc("GET /jobs", s.page)
	mux.HandleFunc("GET /jobs/{id}", s.page)
	mux.HandleFunc("GET /workers", s.page)
	mux.HandleFunc("GET /add", s.addMedia)
	mux.HandleFunc("POST /add/preview", s.previewMedia)
	mux.HandleFunc("POST /add/submit", s.submitMedia)
	mux.HandleFunc("POST /add/presets", s.uploadPreset)
	mux.HandleFunc("POST /add/presets/delete", s.deletePreset)
	mux.HandleFunc("GET /watch", s.watchPage)
	mux.HandleFunc("POST /watch", s.addWatch)
	mux.HandleFunc("POST /watch/scan", s.scanWatches)
	mux.HandleFunc("POST /watch/{id}/{action}", s.changeWatch)
	mux.HandleFunc("GET /import", s.importPage)
	mux.HandleFunc("POST /import/preview", s.preview)
	mux.HandleFunc("POST /import", s.submit)
	mux.HandleFunc("POST /jobs/{id}/retry", s.retry)
	mux.HandleFunc("POST /jobs/clear", s.clearQueue)
	mux.HandleFunc("POST /controls", s.control)
	mux.HandleFunc("POST /workers/rank", s.rank)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			mux.ServeHTTP(w, r)
			return
		}
		if s.get(r) == nil {
			if _, err := s.browserDevice(r); err == nil {
				s.newSession(w, r, true)
			}
		}
		if r.Method == "POST" {
			limit := int64(64 << 20)
			if r.URL.Path == "/pair" || strings.HasPrefix(r.URL.Path, "/devices/") {
				limit = 4096
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				scheme := "http"
				if r.TLS != nil || s.SecureCookies {
					scheme = "https"
				}
				if err != nil || u.Host != r.Host || u.Scheme != scheme {
					http.Error(w, "origin rejected", http.StatusForbidden)
					return
				}
			}
			if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					http.Error(w, "invalid upload (maximum 64 MiB)", 400)
					return
				}
				defer r.MultipartForm.RemoveAll()
			} else if err := r.ParseForm(); err != nil {
				http.Error(w, "invalid form", 400)
				return
			}
			v := s.get(r)
			if v == nil || subtle.ConstantTimeCompare([]byte(v.CSRF), []byte(r.FormValue("csrf"))) != 1 {
				http.Error(w, "CSRF validation failed; reload the page", http.StatusForbidden)
				return
			}
		}
		if r.URL.Path != "/pair" {
			v := s.get(r)
			if v == nil || !v.Auth {
				if r.Header.Get("HX-Request") == "true" {
					w.Header().Set("HX-Redirect", "/pair")
					w.WriteHeader(401)
				} else {
					http.Redirect(w, r, "/pair", http.StatusSeeOther)
				}
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}
func (s *Server) get(r *http.Request) *session {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.sessions[c.Value]
	if v == nil {
		return nil
	}
	if !s.now().Before(v.Expires) {
		delete(s.sessions, c.Value)
		return nil
	}
	copy := *v
	if copy.Auth {
		if _, err := s.browserDevice(r); err != nil {
			return nil
		}
	}
	return &copy
}
func (s *Server) newSession(w http.ResponseWriter, r *http.Request, auth bool) *session {
	v := &session{CSRF: nonce(), Auth: auth, Expires: s.now().Add(8 * time.Hour)}
	id := nonce()
	s.mu.Lock()
	for key, v := range s.sessions {
		if !s.now().Before(v.Expires) {
			delete(s.sessions, key)
		}
	}
	if c, err := r.Cookie(cookieName); err == nil {
		delete(s.sessions, c.Value)
	}
	if len(s.sessions) >= 128 {
		for key := range s.sessions {
			delete(s.sessions, key)
			break
		}
	}
	s.sessions[id] = v
	s.mu.Unlock()
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cookies {
		if c.Name != cookieName {
			r.AddCookie(c)
		}
	}
	r.AddCookie(&http.Cookie{Name: cookieName, Value: id})
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: id, Path: "/", HttpOnly: true, Secure: r.TLS != nil || s.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: 8 * 60 * 60})
	return v
}
func (s *Server) render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	var b bytes.Buffer
	if err := c.Render(context.WithValue(r.Context(), versionKey{}, s.Version), &b); err != nil {
		http.Error(w, "render failed", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, farm.Redact(b.String(), browserToken(r), html.EscapeString(browserToken(r))))
}
func (s *Server) pairPage(w http.ResponseWriter, r *http.Request) {
	v := s.get(r)
	if v != nil && v.Auth {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if v == nil {
		v = s.newSession(w, r, false)
	}
	s.render(w, r, pairView(v.CSRF, ""))
}
func (s *Server) pair(w http.ResponseWriter, r *http.Request) {
	if !s.store.AllowPairing() {
		http.Error(w, "Too many pairing attempts; try again in a minute", http.StatusTooManyRequests)
		return
	}
	e, err := s.store.Redeem(r.Context(), "browser", r.FormValue("code"), r.FormValue("name"))
	if err != nil {
		w.WriteHeader(401)
		s.render(w, r, pairView(s.get(r).CSRF, "Invalid or expired pairing code"))
		return
	}
	cookie := &http.Cookie{Name: "transcoop_device", Value: e.Token, Path: "/", HttpOnly: true, Secure: r.TLS != nil || s.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: 30 * 24 * 60 * 60, Expires: time.Unix(e.Device.Expires, 0)}
	http.SetCookie(w, cookie)
	r.AddCookie(cookie)
	s.newSession(w, r, true)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
func browserToken(r *http.Request) string {
	c, err := r.Cookie("transcoop_device")
	if err != nil {
		return ""
	}
	return c.Value
}
func (s *Server) browserDevice(r *http.Request) (farm.Device, error) {
	d, err := s.store.Authenticate(r.Context(), browserToken(r))
	if err != nil {
		return d, err
	}
	if d.Role != "browser" {
		return farm.Device{}, farm.ErrUnauthorized
	}
	return d, nil
}
func (s *Server) devices(w http.ResponseWriter, r *http.Request) { s.renderDevices(w, r, nil) }
func (s *Server) renderDevices(w http.ResponseWriter, r *http.Request, pairing *farm.Pairing) {
	d, err := s.store.Devices(r.Context())
	if err != nil {
		http.Error(w, "Cannot load devices", 500)
		return
	}
	s.render(w, r, devicesView(s.get(r).CSRF, d, pairing))
}
func (s *Server) createPair(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.CreatePairing(r.Context(), r.FormValue("role"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	s.renderDevices(w, r, &p)
}
func (s *Server) revokeDevice(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Revoke(r.Context(), r.FormValue("id")); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	http.Redirect(w, r, "/devices", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if d, err := s.browserDevice(r); err == nil {
		if err = s.store.Revoke(r.Context(), d.ID); err != nil {
			http.Error(w, "Cannot revoke browser", 500)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: "transcoop_device", Path: "/", MaxAge: -1, HttpOnly: true, Secure: r.TLS != nil || s.SecureCookies, SameSite: http.SameSiteStrictMode})

	if c, err := r.Cookie(cookieName); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1, HttpOnly: true, Secure: r.TLS != nil || s.SecureCookies, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/pair", http.StatusSeeOther)
}

type pageData struct {
	Title, Path, CSRF, Query, Status, Worker, Message string
	Rows                                              []jobRow
	Workers                                           []farm.WorkerStatus
	Counts                                            map[string]int
	Controls                                          map[string]string
	Ranks                                             map[string]int
	Detail                                            *farm.JobDetail
	Active                                            []jobRow
	SrcSize, OutSize                                  int64
	Page, Total                                       int
	Previous, Next                                    string
	Warnings                                          []string
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	d := pageData{CSRF: s.get(r).CSRF, Path: r.URL.RequestURI(), Title: "Dashboard", Counts: map[string]int{}, Query: r.URL.Query().Get("q"), Status: r.URL.Query().Get("status"), Worker: r.URL.Query().Get("worker"), Message: r.URL.Query().Get("result")}
	jobs, err := s.store.List(r.Context())
	if err != nil {
		http.Error(w, "queue unavailable", 500)
		return
	}
	for _, j := range jobs {
		d.Counts[j.Status]++
	}
	d.Controls, err = s.store.Controls(r.Context())
	if err != nil {
		http.Error(w, "controls unavailable", 500)
		return
	}
	d.Workers, err = s.store.Workers(r.Context())
	if err != nil {
		http.Error(w, "workers unavailable", 500)
		return
	}
	d.Warnings = farm.VersionWarnings(d.Workers, s.Version)
	if d.Ranks, err = s.store.Ranks(r.Context()); err != nil {
		http.Error(w, "worker ranks unavailable", 500)
		return
	}
	summaries, err := s.store.Summaries(r.Context())
	if err != nil {
		http.Error(w, "attempts unavailable", 500)
		return
	}
	switch r.URL.Path {
	case "/jobs":
		d.Title = "Queue"
		var matched []farm.Job
		for _, j := range jobs {
			if (d.Status == "" || d.Status == j.Status) && (d.Worker == "" || d.Worker == j.Worker) && (d.Query == "" || strings.Contains(strings.ToLower(j.Source+" "+j.Destination), strings.ToLower(d.Query))) {
				matched = append(matched, j)
			}
		}
		d.Total = len(matched)
		d.Page, _ = strconv.Atoi(r.URL.Query().Get("page"))
		d.Page = max(1, min(d.Page, max(1, (d.Total+24)/25)))
		start := (d.Page - 1) * 25
		for _, j := range matched[start:min(start+25, d.Total)] {
			d.Rows = append(d.Rows, newJobRow(j, summaries))
		}
		q := r.URL.Query()
		if d.Page > 1 {
			q.Set("page", strconv.Itoa(d.Page-1))
			d.Previous = "/jobs?" + q.Encode()
		}
		if start+25 < d.Total {
			q.Set("page", strconv.Itoa(d.Page+1))
			d.Next = "/jobs?" + q.Encode()
		}
	case "/workers":
		d.Title = "Workers"
	case "/":
		for _, j := range jobs {
			if j.Status == "running" {
				d.Active = append(d.Active, newJobRow(j, summaries))
			}
		}
	default:
		id, e := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if e != nil {
			http.NotFound(w, r)
			return
		}
		detail, e := s.store.Detail(r.Context(), id)
		if e != nil {
			http.NotFound(w, r)
			return
		}
		d.Detail = &detail
		d.SrcSize, d.OutSize = fileSize(detail.Job.Source), -1
		if detail.Job.Status == "succeeded" {
			d.OutSize = fileSize(detail.Job.Destination)
		}
		d.Title = fmt.Sprintf("Job %d", id)
	}
	if r.Header.Get("HX-Request") == "true" {
		s.render(w, r, panels(d))
		return
	}
	s.render(w, r, page(d))
}
func (s *Server) importPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, importView(s.get(r).CSRF, nil, 0, "", false))
}
func (s *Server) setQueue(r *http.Request, q []json.RawMessage) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if v := s.sessions[c.Value]; v != nil {
		v.Queue = q
	}
}
func (s *Server) preview(w http.ResponseWriter, r *http.Request) {
	s.setQueue(r, nil)
	file, _, err := r.FormFile("queue")
	if err != nil {
		s.render(w, r, importView(s.get(r).CSRF, nil, 0, "Choose a HandBrake queue JSON file", false))
		return
	}
	defer file.Close()
	var q []json.RawMessage
	dec := json.NewDecoder(file)
	err = dec.Decode(&q)
	if err == nil {
		var extra any
		if dec.Decode(&extra) != io.EOF {
			err = fmt.Errorf("expected one JSON queue array")
		}
	}
	var jobs []farm.Job
	var count int
	if err == nil {
		jobs, count, err = s.store.Preview(r.Context(), q)
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	} else {
		s.setQueue(r, q)
	}
	s.render(w, r, importView(s.get(r).CSRF, jobs, count, msg, err == nil))
}
func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	v := s.get(r)
	n, err := s.store.Import(r.Context(), v.Queue)
	if err != nil {
		s.render(w, r, importView(v.CSRF, nil, 0, err.Error(), false))
		return
	}
	s.setQueue(r, nil)
	http.Redirect(w, r, "/jobs?result="+url.QueryEscape(fmt.Sprintf("Imported %d jobs; skipped %d duplicates", n, len(v.Queue)-n)), http.StatusSeeOther)
}
func (s *Server) control(w http.ResponseWriter, r *http.Request) {
	deviceID := r.FormValue("device")
	if err := s.store.SetControl(r.Context(), deviceID, r.FormValue("action")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	target := "/jobs"
	if deviceID != "" {
		target = "/workers"
	}
	if next := r.FormValue("next"); localPath(next) {
		target = next
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Server) rank(w http.ResponseWriter, r *http.Request) {
	rank, err := farm.ParseRank(r.FormValue("rank"))
	if err == nil {
		err = s.store.SetRank(r.Context(), r.FormValue("device"), rank)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	target := "/workers"
	if next := r.FormValue("next"); localPath(next) {
		target = next
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func controlState(states map[string]string, deviceID string) string {
	if state := states[deviceID]; state != "" {
		return state
	}
	return "playing"
}

func (s *Server) clearQueue(w http.ResponseWriter, r *http.Request) {
	n, err := s.store.ClearQueue(r.Context())
	if err != nil {
		http.Error(w, "Could not clear queue", http.StatusInternalServerError)
		return
	}
	message := fmt.Sprintf("Removed %d jobs. Running jobs and media files were kept.", n)
	http.Redirect(w, r, "/jobs?result="+url.QueryEscape(message), http.StatusSeeOther)
}
func (s *Server) retry(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	err = s.store.Retry(r.Context(), id)
	message := "Job queued for retry"
	if err != nil {
		message = err.Error()
	}
	http.Redirect(w, r, fmt.Sprintf("/jobs/%d?result=%s", id, url.QueryEscape(message)), http.StatusSeeOther)
}
func jobURL(id int64) string { return fmt.Sprintf("/jobs/%d", id) }
func tailID(n int) string    { return fmt.Sprintf("log-%d", n) }

func deviceState(d farm.Device) string {
	if d.Revoked {
		return "Revoked"
	}
	if d.Expires > 0 && d.Expires <= time.Now().Unix() {
		return "Expired"
	}
	return "Paired"
}

func deviceRole(role string) string {
	switch role {
	case "browser":
		return "Browser"
	case "worker":
		return "Encoding worker"
	case "operator":
		return "Operator CLI"
	case "admin":
		return "Local administrator"
	}
	return role
}

// localPath accepts only same-site absolute paths for post-action redirects.
func localPath(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") && !strings.ContainsAny(p, "\\\r\n")
}
