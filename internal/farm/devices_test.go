package farm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func enroll(t *testing.T, s *Store, role string) Enrollment {
	t.Helper()
	p, err := s.CreatePairing(context.Background(), role)
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.Redeem(context.Background(), role, p.Code, "test "+role)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func TestPairingAtomicExpiryAndRevocation(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	p, err := s.CreatePairing(ctx, "worker")
	if err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Redeem(ctx, "worker", p.Code, "worker"); err == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatal(successes.Load())
	}
	p, _ = s.CreatePairing(ctx, "worker")
	for i := 0; i < 5; i++ {
		if _, err = s.Redeem(ctx, "worker", "wrong", "worker"); !errors.Is(err, ErrPairing) {
			t.Fatal(err)
		}
	}
	if _, err = s.Redeem(ctx, "worker", p.Code, "worker"); !errors.Is(err, ErrPairing) {
		t.Fatal("exhausted code accepted")
	}
	p, _ = s.CreatePairing(ctx, "worker")
	s.db.Exec(`UPDATE pairing SET expires=0`)
	if _, err = s.Redeem(ctx, "worker", p.Code, "worker"); !errors.Is(err, ErrPairing) {
		t.Fatal("expired code accepted")
	}
	e := enroll(t, s, "browser")
	if e.Device.Expires <= time.Now().Unix() {
		t.Fatal("missing browser expiry")
	}
	if _, err = s.Authenticate(ctx, e.Token); err != nil {
		t.Fatal(err)
	}
	if err = s.Revoke(ctx, e.Device.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Authenticate(ctx, e.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("revoked accepted")
	}
	for i := 0; i < 30; i++ {
		if !s.AllowPairing() {
			t.Fatal("premature rate limit")
		}
	}
	if s.AllowPairing() {
		t.Fatal("unbounded redemption")
	}
}
func TestDevicesPersistAndSecretsAreHashed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "farm.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	e := enroll(t, s, "worker")
	var hash string
	if err = s.db.QueryRow(`SELECT hash FROM devices WHERE id=?`, e.Device.ID).Scan(&hash); err != nil || hash == e.Token {
		t.Fatal(hash, err)
	}
	s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if d, err := s.Authenticate(context.Background(), e.Token); err != nil || d.ID != e.Device.ID {
		t.Fatal(d, err)
	}
}
func apiRequest(s *Store, token, method, path, body, attempt string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("X-Attempt-Token", attempt)
	w := httptest.NewRecorder()
	(&Server{Store: s}).Handler().ServeHTTP(w, r)
	return w
}
func TestWorkerRoleAndAttemptOwnership(t *testing.T) {
	s := newStore(t)
	worker := enroll(t, s, "worker")
	other := enroll(t, s, "worker")
	operator := enroll(t, s, "operator")
	browser := enroll(t, s, "browser")
	for _, path := range []string{"/api/jobs", "/api/devices", "/api/workers"} {
		if w := apiRequest(s, "", "GET", path, "", ""); w.Code != 401 {
			t.Fatal(w.Code)
		}
		if w := apiRequest(s, worker.Token, "GET", path, "", ""); w.Code != 403 {
			t.Fatal(w.Code)
		}
	}
	if w := apiRequest(s, browser.Token, "GET", "/api/jobs", "", ""); w.Code != 401 {
		t.Fatal("browser bearer accepted")
	}
	if w := apiRequest(s, worker.Token, "POST", "/api/pairing", `{"role":"operator"}`, ""); w.Code != 403 {
		t.Fatal("worker escalated")
	}
	if _, err := s.Import(context.Background(), []json.RawMessage{fixture(t, t.TempDir(), "one")}); err != nil {
		t.Fatal(err)
	}
	w := apiRequest(s, worker.Token, "POST", "/api/claim", `{"worker":"same-name","encoders":["x265_10bit"]}`, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var j Job
	json.Unmarshal(w.Body.Bytes(), &j)
	if w = apiRequest(s, other.Token, "POST", "/api/jobs/1/heartbeat", "{}", j.Token); w.Code != 403 {
		t.Fatal("another worker stole attempt", w.Code)
	}
	if w = apiRequest(s, worker.Token, "POST", "/api/jobs/1/heartbeat", "{}", j.Token); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = apiRequest(s, operator.Token, "GET", "/api/jobs", "", ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if err := s.Revoke(context.Background(), worker.Device.ID); err != nil {
		t.Fatal(err)
	}
	if w = apiRequest(s, worker.Token, "POST", "/api/jobs/1/heartbeat", "{}", j.Token); w.Code != 401 {
		t.Fatal("revoked worker still active")
	}
}
func TestMigrationPreservesJobsAndRejectsLiveLeases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "farm.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Import(context.Background(), []json.RawMessage{fixture(t, t.TempDir(), "saved")})
	j, err := s.Claim(context.Background(), "worker", []string{"x265_10bit"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`ALTER TABLE jobs DROP COLUMN device_id; ALTER TABLE jobs ADD COLUMN cost_points INTEGER NOT NULL DEFAULT 8 CHECK(cost_points>0); ALTER TABLE jobs ADD COLUMN cost_source TEXT NOT NULL DEFAULT ''; DROP TABLE devices; DROP TABLE pairing; PRAGMA user_version=0;`)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.migrateV2(context.Background()); err == nil {
		t.Fatal("migrated active farm")
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('jobs') WHERE name='cost_points'`).Scan(&n)
	if n != 1 {
		t.Fatal("failed migration changed schema")
	}
	s.db.Exec(`UPDATE jobs SET lease_until=0`)
	s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	jobs, err := s.List(context.Background())
	if err != nil || len(jobs) != 1 || jobs[0].ID != j.ID || jobs[0].Status != "interrupted" || jobs[0].Attempts != 1 {
		t.Fatal(jobs, err)
	}
	s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('jobs') WHERE name IN ('cost_points','cost_source')`).Scan(&n)
	if n != 0 {
		t.Fatal("cost columns retained")
	}
	detail, err := s.Detail(context.Background(), j.ID)
	if err != nil || len(detail.Attempts) != 1 {
		t.Fatal(detail, err)
	}
}

func TestDeviceExpiryAndStatusOwnership(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	browser := enroll(t, s, "browser")
	if _, err := s.db.Exec(`UPDATE devices SET expires=1 WHERE id=?`, browser.Device.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, browser.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("expired credential accepted")
	}
	a := enroll(t, s, "worker")
	b := enroll(t, s, "worker")
	path := "/api/workers/instance-0123456789/status"
	body := `{"name":"worker","scheduler":"fixed","max_jobs":1}`
	if w := apiRequest(s, a.Token, "POST", path, body, ""); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := apiRequest(s, b.Token, "POST", path, body, ""); w.Code == 204 {
		t.Fatal("overwrote another device's report")
	}
	status, err := s.Workers(ctx)
	if err != nil || len(status) != 1 || status[0].DeviceID != a.Device.ID {
		t.Fatal(status, err)
	}
}

func TestLocalAdminRotation(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	first := strings.Repeat("a", 64)
	second := strings.Repeat("b", 64)
	if err := s.InstallAdmin(ctx, first); err != nil {
		t.Fatal(err)
	}
	d, err := s.Authenticate(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Revoke(ctx, d.ID); err == nil {
		t.Fatal("remote revocation removed recovery access")
	}
	if err = s.InstallAdmin(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Authenticate(ctx, first); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("old administrator credential retained")
	}
	next, err := s.Authenticate(ctx, second)
	if err != nil || next.ID != d.ID {
		t.Fatal(next, err)
	}
}
