package farm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const LeaseDuration = 90 * time.Second

type Server struct {
	Store   *Store
	Version string // Transcoop version reported by the health endpoint
}

// A query parameter lets preset-aware workers also use older coordinators,
// which reject unknown fields in a claim's JSON body.
const presetJobQuery = "?preset_jobs=1"

type ClaimRequest struct {
	Expected *ClaimCandidate `json:"expected,omitempty"`
	Worker   string          `json:"worker"`
	Encoders []string        `json:"encoders"`

	presetJobs bool // Set from the request URL.
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/capabilities", func(w http.ResponseWriter, r *http.Request) {
		respond(w, map[string]bool{"pairing": true, "preset_jobs": true, "telemetry": true, "filesystem": true})
	})
	mux.HandleFunc("POST /api/workers/{instance}/status", func(w http.ResponseWriter, r *http.Request) {
		var v WorkerStatus
		err := decodeLimit(w, r, &v, 512<<10)
		if err == nil {
			v.DeviceID = requestDevice(r.Context()).ID
			err = s.Store.PutWorker(r.Context(), r.PathValue("instance"), v)
		}
		if err != nil {
			apiError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/jobs/{id}/telemetry", func(w http.ResponseWriter, r *http.Request) {
		var v JobTelemetry
		err := decodeLimit(w, r, &v, 512<<10)
		id, e := jobID(r)
		if err == nil {
			err = e
		}
		if err == nil {
			v.Tail, v.Truncated = cleanTail(v.Tail, v.Truncated, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), r.Header.Get("X-Attempt-Token"))
			err = s.Store.PutTelemetry(r.Context(), id, r.Header.Get("X-Attempt-Token"), v)
		}
		if err != nil {
			apiError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/workers", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.Workers(r.Context())
		if err != nil {
			apiError(w, err)
			return
		}
		respond(w, v)
	})
	mux.HandleFunc("GET /api/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := jobID(r)
		if err != nil {
			apiError(w, err)
			return
		}
		v, err := s.Store.Detail(r.Context(), id)
		if err != nil {
			apiError(w, err)
			return
		}
		respond(w, v)
	})
	mux.HandleFunc("POST /api/jobs", s.importQueue)
	mux.HandleFunc("GET /api/jobs", s.list)
	mux.HandleFunc("POST /api/claim", s.claim)
	mux.HandleFunc("POST /api/claim/preview", s.claim)
	mux.HandleFunc("POST /api/jobs/{id}/heartbeat", s.heartbeat)
	mux.HandleFunc("POST /api/jobs/{id}/complete", s.complete)
	mux.HandleFunc("POST /api/jobs/{id}/retry", s.retry)
	mux.HandleFunc("/api/jobs/{id}/fs/{action}", s.filesystem)
	mux.HandleFunc("GET /api/jobs/{id}/source", s.source)
	mux.HandleFunc("PUT /api/jobs/{id}/output", s.output)

	mux.HandleFunc("POST /api/pairing", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Role string `json:"role"`
		}
		if err := decode(w, r, &req); err != nil {
			apiError(w, err)
			return
		}
		p, err := s.Store.CreatePairing(r.Context(), req.Role)
		if err != nil {
			apiError(w, err)
			return
		}
		respond(w, p)
	})
	mux.HandleFunc("GET /api/devices", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.Devices(r.Context())
		if err != nil {
			apiError(w, err)
			return
		}
		respond(w, v)
	})
	mux.HandleFunc("DELETE /api/devices/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Store.Revoke(r.Context(), r.PathValue("id")); err != nil {
			apiError(w, err)
			return
		}
		w.WriteHeader(204)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/health" && r.Method == "GET" {
			// Unauthenticated so connectivity can be checked before pairing.
			respond(w, map[string]string{"status": "ok", "version": s.Version})
			return
		}
		if r.URL.Path == "/api/pairing/redeem" && r.Method == "POST" {
			if !s.Store.AllowPairing() {
				http.Error(w, "too many pairing attempts; try again in a minute", http.StatusTooManyRequests)
				return
			}
			var req struct {
				Role string `json:"role"`
				Code string `json:"code"`
				Name string `json:"name"`
			}
			if err := decodeLimit(w, r, &req, 4096); err != nil {
				apiError(w, err)
				return
			}
			if req.Role == "browser" {
				http.Error(w, "pair browsers through the console", http.StatusForbidden)
				return
			}
			e, err := s.Store.Redeem(r.Context(), req.Role, req.Code, req.Name)
			if err != nil {
				http.Error(w, "invalid or expired pairing code", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			respond(w, e)
			return
		}
		d, err := s.Store.Authenticate(r.Context(), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if err != nil || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || d.Role == "browser" {
			http.Error(w, "unauthorized; pair this device", http.StatusUnauthorized)
			return
		}
		if d.Role == "worker" && !s.workerAllowed(r, d) {
			http.Error(w, "device does not own this operation", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), deviceContext{}, d)))
	})
}
func (s *Server) workerAllowed(r *http.Request, d Device) bool {
	path := r.URL.Path
	if path == "/api/capabilities" && r.Method == "GET" || (path == "/api/claim" || path == "/api/claim/preview") && r.Method == "POST" {
		return true
	}
	if strings.HasPrefix(path, "/api/workers/") && strings.HasSuffix(path, "/status") && r.Method == "POST" {
		return true
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	// /api/jobs/{id}/{action}, or /api/jobs/{id}/fs/{operation}
	if len(parts) == 5 && parts[3] == "fs" {
		parts = parts[:4]
	}
	if len(parts) != 4 || parts[0] != "api" || parts[1] != "jobs" {
		return false
	}
	methods := map[string]string{"heartbeat": "POST", "complete": "POST", "telemetry": "POST", "source": "GET", "output": "PUT"}
	if parts[3] != "fs" && methods[parts[3]] != r.Method {
		return false
	}
	id, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return false
	}
	j, err := s.Store.Active(r.Context(), id, r.Header.Get("X-Attempt-Token"))
	return err == nil && j.DeviceID == d.ID

}

func respond(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
func apiError(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) {
		// Canceled clients (including discarded FUSE read-ahead) are routine.
		// 499 conventionally means Client Closed Request; do not log a bad request
		// or let a still-connected caller mistake cancellation for success.
		w.WriteHeader(499)
		return
	}
	code := http.StatusBadRequest
	if errors.Is(err, ErrLease) {
		code = http.StatusConflict
	}
	log.Printf("request failed: status=%d error=%q", code, err)
	http.Error(w, err.Error(), code)
}
func decode(w http.ResponseWriter, r *http.Request, value any) error {
	return decodeLimit(w, r, value, 64<<20)
}
func decodeLimit(w http.ResponseWriter, r *http.Request, value any, limit int64) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("expected one JSON value")
	}
	return nil
}
func jobID(r *http.Request) (int64, error) { return strconv.ParseInt(r.PathValue("id"), 10, 64) }
func (s *Server) active(r *http.Request) (Job, error) {
	id, err := jobID(r)
	if err != nil {
		return Job{}, err
	}
	return s.Store.Active(r.Context(), id, r.Header.Get("X-Attempt-Token"))
}
func (s *Server) importQueue(w http.ResponseWriter, r *http.Request) {
	var queue []json.RawMessage
	if err := decode(w, r, &queue); err != nil {
		apiError(w, err)
		return
	}
	n, err := s.Store.Import(r.Context(), queue)
	if err != nil {
		apiError(w, err)
		return
	}
	log.Printf("queue imported: added=%d duplicates=%d", n, len(queue)-n)
	respond(w, map[string]int{"imported": n})
}
func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.Store.List(r.Context())
	if err != nil {
		apiError(w, err)
		return
	}
	respond(w, jobs)
}
func (s *Server) claim(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.CleanupFilesystem(r.Context()); err != nil {
		apiError(w, err)
		return
	}
	var req ClaimRequest
	if err := decode(w, r, &req); err != nil {
		apiError(w, err)
		return
	}
	if len(req.Encoders) > 100 || len(req.Worker) > 200 {
		apiError(w, errors.New("worker or encoder list is too long"))
		return
	}
	preview := r.URL.Path == "/api/claim/preview"
	req.presetJobs = r.URL.Query().Get("preset_jobs") == "1"
	j, err := s.Store.selectClaim(r.Context(), req, LeaseDuration, requestDevice(r.Context()).ID, preview)
	if err != nil {
		apiError(w, err)
		return
	}
	if j == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if preview {
		respond(w, candidateFor(*j))
		return
	}
	j.Staging = StagePath(*j)
	log.Printf("job %d: assigned worker=%q attempt=%d encoder=%s source=%q", j.ID, j.Worker, j.Attempts, j.Encoder, j.Source)
	respond(w, j)
}
func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	id, err := jobID(r)
	if err == nil {
		err = s.Store.Heartbeat(r.Context(), id, r.Header.Get("X-Attempt-Token"), LeaseDuration)
	}
	if err != nil {
		apiError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// StagePath is shared by the coordinator and workers. A new attempt never reuses an old output.
func StagePath(j Job) string {
	return filepath.Join(filepath.Dir(j.Destination), fmt.Sprintf(".transcoop-%d-%s%s", j.ID, j.Token, filepath.Ext(j.Destination)))
}

// outputMode is the permission requested for encoded outputs; the process
// umask applies, so the usual result is 0644 (readable by media servers).
const outputMode = 0666

// createOutputTemp creates a uniquely named file for an encoded output. Unlike
// os.CreateTemp, which always uses 0600, it honours the umask, because the file
// becomes the published output through a hard link.
func createOutputTemp(dir, prefix string) (*os.File, error) {
	for range 100 {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(filepath.Join(dir, prefix+hex.EncodeToString(b)), os.O_RDWR|os.O_CREATE|os.O_EXCL, outputMode)
		if !errors.Is(err, os.ErrExist) {
			return f, err
		}
	}
	return nil, errors.New("could not create a unique upload file")
}

func publish(j Job) error {
	stage := StagePath(j)
	info, err := os.Lstat(stage)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return errors.New("encoded output must be a nonempty regular file")
	}
	// Link atomically creates the final name without replacing an existing file.
	// Both names are on the destination filesystem. Fail closed if hard links are unsupported.
	if err = os.Link(stage, j.Destination); err != nil {
		return fmt.Errorf("publish output (destination must not exist; filesystem must support hard links): %w", err)
	}
	_ = os.Remove(stage)
	return nil
}
func (s *Server) complete(w http.ResponseWriter, r *http.Request) {
	idForLock, _ := jobID(r)
	lock := fsLock(idForLock)
	lock.Lock()
	defer lock.Unlock()
	var req struct {
		Error   string `json:"error"`
		Release bool   `json:"release"` // worker is shutting down; requeue the job
	}
	if err := decode(w, r, &req); err != nil {
		apiError(w, err)
		return
	}
	if len(req.Error) > 8192 {
		req.Error = req.Error[:8192]
	}
	req.Error = Redact(req.Error, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), r.Header.Get("X-Attempt-Token"))
	id, err := jobID(r)
	if err == nil && req.Release {
		if req.Error == "" {
			req.Error = "worker shut down"
		}
		if err = s.Store.Release(r.Context(), id, r.Header.Get("X-Attempt-Token"), req.Error); err == nil {
			log.Printf("job %d: released to the queue: %s", id, req.Error)
			w.WriteHeader(http.StatusNoContent)
			return
		}
	} else if err == nil {
		err = s.Store.Finish(r.Context(), id, r.Header.Get("X-Attempt-Token"), req.Error, publish)
	}
	if err != nil {
		apiError(w, err)
		return
	}
	if req.Error != "" {
		log.Printf("job %d: failed error=%q", id, req.Error)
	} else {
		log.Printf("job %d: completed; output published", id)
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) retry(w http.ResponseWriter, r *http.Request) {
	id, err := jobID(r)
	if err == nil {
		err = s.Store.Retry(r.Context(), id)
	}
	if err != nil {
		apiError(w, err)
		return
	}
	log.Printf("job %d: queued for retry", id)
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) source(w http.ResponseWriter, r *http.Request) {
	j, err := s.active(r)
	if err != nil {
		apiError(w, err)
		return
	}
	f, err := os.Open(j.Source)
	if err != nil {
		apiError(w, err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		apiError(w, err)
		return
	}
	if !info.Mode().IsRegular() {
		apiError(w, errors.New("transfer mode requires a single regular source file; use shared mode for disc directories"))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}
func (s *Server) output(w http.ResponseWriter, r *http.Request) {
	j, err := s.active(r)
	if err != nil {
		apiError(w, err)
		return
	}
	if err = os.MkdirAll(filepath.Dir(j.Destination), 0755); err != nil {
		apiError(w, err)
		return
	}
	f, err := createOutputTemp(filepath.Dir(j.Destination), ".transcoop-upload-")
	if err != nil {
		apiError(w, err)
		return
	}
	defer os.Remove(f.Name())
	_, err = io.Copy(f, r.Body)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		apiError(w, err)
		return
	}
	// Check ownership again after a potentially long transfer.
	if _, err = s.active(r); err != nil {
		apiError(w, err)
		return
	}
	if err = os.Link(f.Name(), StagePath(j)); err != nil {
		apiError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
