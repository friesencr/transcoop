package farm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/friesencr/transcoop/internal/fsproto"
	"strings"
	"time"
	"unicode/utf8"
)

const TailLimit = 64 << 10

type WorkerStatus struct {
	Filesystem *fsproto.Stats `json:"filesystem,omitempty"`
	Instance   string         `json:"instance"`
	Name       string         `json:"name"`
	Version    string         `json:"version,omitempty"`
	HandBrake  string         `json:"handbrake,omitempty"`
	Priority   string         `json:"priority,omitempty"` // HandBrake process priority
	Encoders   []string       `json:"encoders"`
	Mode       string         `json:"mode"`
	DeviceID   string         `json:"device_id"`
	Scheduler  string         `json:"scheduler"`
	MaxJobs    int            `json:"max_jobs"`
	CPUTarget  float64        `json:"cpu_target"`
	CPU        *float64       `json:"cpu,omitempty"`
	Active     []int64        `json:"active"`
	Reason     string         `json:"reason"`
	Headroom   int            `json:"headroom"`
	Updated    int64          `json:"updated"`
}

func (w WorkerStatus) Presence() string {
	age := time.Now().Unix() - w.Updated
	if age >= 30 {
		return "offline"
	}
	if age >= 15 {
		return "stale"
	}
	return "online"
}

type JobTelemetry struct {
	Instance   string   `json:"instance"`
	Phase      string   `json:"phase"`
	Pass       int      `json:"pass"`
	PassCount  int      `json:"pass_count"`
	Fraction   *float64 `json:"fraction,omitempty"`
	FPS        *float64 `json:"fps,omitempty"`
	AverageFPS *float64 `json:"average_fps,omitempty"`
	ETA        *int     `json:"eta_seconds,omitempty"`
	Tail       string   `json:"tail"`
	Truncated  bool     `json:"truncated"`
	Updated    int64    `json:"updated"`
}
type Attempt struct {
	Number    int           `json:"number"`
	Worker    string        `json:"worker"`
	Started   int64         `json:"started"`
	Finished  int64         `json:"finished"`
	Status    string        `json:"status"`
	Error     string        `json:"error"`
	Telemetry *JobTelemetry `json:"telemetry,omitempty"`
}
type Event struct {
	Kind string `json:"kind"`
	At   int64  `json:"at"`
}
type JobDetail struct {
	Job      Job       `json:"job"`
	Attempts []Attempt `json:"attempts"`
	Events   []Event   `json:"events"`
}

func (s *Store) migrateTelemetry() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS execution_controls(device_id TEXT PRIMARY KEY, state TEXT NOT NULL CHECK(state IN ('playing','paused','stopped')));
CREATE TABLE IF NOT EXISTS worker_status(instance TEXT PRIMARY KEY, snapshot BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS attempts(job_id INTEGER NOT NULL, number INTEGER NOT NULL, worker TEXT NOT NULL, started INTEGER NOT NULL, finished INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL, error TEXT NOT NULL DEFAULT '', telemetry BLOB, PRIMARY KEY(job_id,number));
CREATE TABLE IF NOT EXISTS events(job_id INTEGER NOT NULL, kind TEXT NOT NULL, at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS events_job ON events(job_id,at);
CREATE TRIGGER IF NOT EXISTS job_import_event AFTER INSERT ON jobs BEGIN
 INSERT INTO events VALUES(NEW.id,'import',unixepoch()); END;
CREATE TRIGGER IF NOT EXISTS job_state_event AFTER UPDATE OF status ON jobs WHEN OLD.status != NEW.status BEGIN
 INSERT INTO events VALUES(NEW.id,CASE NEW.status WHEN 'running' THEN 'assignment' WHEN 'pending' THEN 'retry' WHEN 'succeeded' THEN 'completion' WHEN 'failed' THEN 'failure' ELSE NEW.status END,unixepoch());
 INSERT INTO attempts(job_id,number,worker,started,status) SELECT NEW.id,NEW.attempts,NEW.worker,unixepoch(),NEW.status WHERE NEW.status='running';
 UPDATE attempts SET status=NEW.status,error=NEW.error,finished=unixepoch() WHERE job_id=NEW.id AND number=NEW.attempts AND NEW.status IN ('succeeded','failed','interrupted');
 UPDATE attempts SET telemetry=NULL WHERE job_id=NEW.id AND number<=NEW.attempts-3;
END;
INSERT OR IGNORE INTO attempts(job_id,number,worker,started,status,error) SELECT id,attempts,worker,0,status,CASE WHEN token!='' THEN replace(error,token,'[redacted]') ELSE error END FROM jobs WHERE attempts>0;
`)
	return err
}
func Redact(text string, secrets ...string) string {
	for _, s := range secrets {
		if s != "" {
			text = strings.ReplaceAll(text, s, "[redacted]")
		}
	}
	return text
}

// cleanTail bounds the serialized UTF-8 tail even when redaction expands short secrets.
func cleanTail(tail string, truncated bool, secrets ...string) (string, bool) {
	tail = strings.ToValidUTF8(Redact(tail, secrets...), "�")
	if len(tail) > TailLimit {
		tail = tail[len(tail)-TailLimit:]
		truncated = true
		for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
			tail = tail[1:]
		}
	}
	return tail, truncated
}
func (s *Store) PutWorker(ctx context.Context, instance string, v WorkerStatus) error {
	if len(instance) < 16 || len(instance) > 128 || len(v.Name) > 200 || len(v.Encoders) > 100 || len(v.Active) > 10000 || len(v.Reason) > 1000 || len(v.Version) > 100 || len(v.HandBrake) > 200 || ValidatePriority(v.Priority) != nil {
		return errors.New("invalid worker snapshot")
	}
	v.Instance = instance
	v.Updated = time.Now().Unix()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO worker_status VALUES(?,?) ON CONFLICT(instance) DO UPDATE SET snapshot=excluded.snapshot WHERE json_extract(worker_status.snapshot,'$.device_id')=json_extract(excluded.snapshot,'$.device_id')`, instance, b)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("worker instance belongs to another device")
	}
	return nil
}

// staleWorkerAge is how long an instance may go unreported before it is
// dropped from listings and later pruned from the database.
const staleWorkerAge = 24 * 60 * 60

// visibleWorkers hides instances a restart has superseded (offline, with a
// newer instance from the same device) and instances silent for a day.
// Several live processes sharing one device credential all stay visible.
func visibleWorkers(all []WorkerStatus, now int64) []WorkerStatus {
	newest := map[string]int64{}
	for _, w := range all {
		if w.DeviceID != "" && w.Updated > newest[w.DeviceID] {
			newest[w.DeviceID] = w.Updated
		}
	}
	out := []WorkerStatus{}
	for _, w := range all {
		age := now - w.Updated
		if age > staleWorkerAge {
			continue
		}
		if age >= 30 && w.DeviceID != "" && newest[w.DeviceID] > w.Updated {
			continue
		}
		out = append(out, w)
	}
	return out
}

// pruneWorkers removes instances that have not reported for a day.
func (s *Store) pruneWorkers(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM worker_status WHERE json_extract(snapshot,'$.updated') < ?`, time.Now().Unix()-staleWorkerAge)
	return err
}
func (s *Store) Workers(ctx context.Context) ([]WorkerStatus, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT snapshot FROM worker_status ORDER BY instance`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	v := []WorkerStatus{}
	for rows.Next() {
		var b []byte
		var w WorkerStatus
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &w); err != nil {
			return nil, err
		}
		v = append(v, w)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return visibleWorkers(v, time.Now().Unix()), nil
}
func (s *Store) PutTelemetry(ctx context.Context, id int64, token string, v JobTelemetry) error {
	if len(v.Tail) > TailLimit || len(v.Phase) > 100 || len(v.Instance) > 128 || (v.Fraction != nil && (*v.Fraction < 0 || *v.Fraction > 1)) {
		return errors.New("invalid telemetry")
	}
	if v.ETA != nil && *v.ETA < 0 {
		v.ETA = nil
	}
	v.Updated = time.Now().Unix()
	v.Tail, v.Truncated = cleanTail(v.Tail, v.Truncated, token)
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE attempts SET telemetry=? WHERE job_id=? AND number=(SELECT attempts FROM jobs WHERE id=? AND token=? AND status='running' AND lease_until>?)`, b, id, id, token, time.Now().Unix())
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return ErrLease
	}
	return nil
}
func (s *Store) Detail(ctx context.Context, id int64) (JobDetail, error) {
	// List also performs authoritative lease expiration.
	jobs, err := s.List(ctx)
	if err != nil {
		return JobDetail{}, err
	}
	d := JobDetail{Attempts: []Attempt{}, Events: []Event{}}
	for _, j := range jobs {
		if j.ID == id {
			d.Job = j
			break
		}
	}
	if d.Job.ID == 0 {
		return d, sql.ErrNoRows
	}
	rows, err := s.db.QueryContext(ctx, `SELECT number,worker,started,finished,status,error,telemetry FROM attempts WHERE job_id=? ORDER BY number DESC`, id)
	if err != nil {
		return d, err
	}
	for rows.Next() {
		var a Attempt
		var b []byte
		if err = rows.Scan(&a.Number, &a.Worker, &a.Started, &a.Finished, &a.Status, &a.Error, &b); err != nil {
			rows.Close()
			return d, err
		}
		if len(b) > 0 {
			if err = json.Unmarshal(b, &a.Telemetry); err != nil {
				rows.Close()
				return d, err
			}
		}
		d.Attempts = append(d.Attempts, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return d, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT kind,at FROM events WHERE job_id=? ORDER BY at DESC,rowid DESC LIMIT 100`, id)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	for rows.Next() {
		var e Event
		if err = rows.Scan(&e.Kind, &e.At); err != nil {
			return d, err
		}
		d.Events = append(d.Events, e)
	}
	return d, rows.Err()
}
func (s *Store) Preview(ctx context.Context, queue []json.RawMessage) ([]Job, int, error) {
	jobs := []Job{}
	for i, raw := range queue {
		j, err := parseJob(raw)
		if err != nil {
			return jobs, 0, fmt.Errorf("job %d: %w", i+1, err)
		}
		j.Payload = nil
		jobs = append(jobs, j)
	}
	n, err := s.importQueue(ctx, queue, true)
	return jobs, n, err
}

// AttemptSummary is the latest attempt of a job without its log tail, for lists.
type AttemptSummary struct {
	Number     int
	Worker     string
	Started    int64
	Finished   int64
	Fraction   *float64
	FPS        *float64
	AverageFPS *float64
	ETA        *int
	Updated    int64
}

// Summaries returns the latest attempt of each job that has one, keyed by job ID.
func (s *Store) Summaries(ctx context.Context) (map[int64]AttemptSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT a.job_id,a.number,a.worker,a.started,a.finished,
 json_extract(t,'$.fraction'),json_extract(t,'$.fps'),json_extract(t,'$.average_fps'),
 json_extract(t,'$.eta_seconds'),coalesce(json_extract(t,'$.updated'),0)
FROM (SELECT *,CAST(telemetry AS TEXT) AS t FROM attempts) a
JOIN (SELECT job_id,MAX(number) AS number FROM attempts GROUP BY job_id) l
 ON a.job_id=l.job_id AND a.number=l.number`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]AttemptSummary{}
	for rows.Next() {
		var id int64
		var a AttemptSummary
		var eta sql.NullInt64
		if err = rows.Scan(&id, &a.Number, &a.Worker, &a.Started, &a.Finished, &a.Fraction, &a.FPS, &a.AverageFPS, &eta, &a.Updated); err != nil {
			return nil, err
		}
		if eta.Valid {
			n := int(eta.Int64)
			a.ETA = &n
		}
		out[id] = a
	}
	return out, rows.Err()
}
