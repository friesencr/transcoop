package farm

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var ErrLease = errors.New("job lease is no longer valid")

type Job struct {
	ID          int64           `json:"id"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	Source      string          `json:"source"`
	Destination string          `json:"destination"`
	Staging     string          `json:"staging,omitempty"`
	Encoder     string          `json:"encoder"`
	Status      string          `json:"status"`
	Worker      string          `json:"worker"`
	Token       string          `json:"token,omitempty"`
	LeaseUntil  int64           `json:"lease_until"`
	Attempts    int             `json:"attempts"`
	Error       string          `json:"error,omitempty"`
	DeviceID    string          `json:"device_id,omitempty"`
}

type Store struct {
	filesystemCancel context.CancelFunc
	filesystemDone   chan struct{}
	db               *sql.DB
	pairMu           sync.Mutex
	pairWindow       time.Time
	pairCount        int
	holdMu           sync.Mutex
	held             map[holdKey]time.Time // job attempts waiting for a higher-ranked worker
}

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// One connection serializes short queue operations; media never goes through SQLite.
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;
CREATE TABLE IF NOT EXISTS jobs (
 id INTEGER PRIMARY KEY, digest TEXT NOT NULL UNIQUE, payload BLOB NOT NULL,
 source TEXT NOT NULL, destination TEXT NOT NULL UNIQUE, encoder TEXT NOT NULL,
 status TEXT NOT NULL DEFAULT 'pending', worker TEXT NOT NULL DEFAULT '',
 token TEXT NOT NULL DEFAULT '', lease_until INTEGER NOT NULL DEFAULT 0,
 attempts INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS jobs_status ON jobs(status, id);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err = s.migrateV2(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.migrateTelemetry(); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.migrateFilesystem(); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.migrateWatches(); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.migrateRanks(); err != nil {
		db.Close()
		return nil, err
	}

	cleanupCtx, cancel := context.WithCancel(context.Background())
	s.filesystemCancel = cancel
	s.filesystemDone = make(chan struct{})
	go func() {
		defer close(s.filesystemDone)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-cleanupCtx.Done():
				return
			case <-ticker.C:
				_ = s.CleanupFilesystem(cleanupCtx)
				_ = s.pruneWorkers(cleanupCtx)
			}
		}
	}()
	return s, nil
}

func (s *Store) Close() error {
	if s.filesystemCancel != nil {
		s.filesystemCancel()
		<-s.filesystemDone
	}
	return s.db.Close()
}

func parseJob(raw json.RawMessage) (Job, error) {
	var q struct {
		Job struct {
			Source      struct{ Path string }
			Destination struct{ File string }
			Video       struct{ Encoder string }
		}
	}
	if err := json.Unmarshal(raw, &q); err != nil {
		return Job{}, err
	}
	snapshot, preset, err := ReadPresetSnapshot(raw)
	if err != nil {
		return Job{}, err
	}
	if snapshot != nil && preset["VideoEncoder"] != q.Job.Video.Encoder {
		return Job{}, errors.New("preset job encoder does not match snapshot")
	}
	j := Job{Payload: raw, Source: q.Job.Source.Path, Destination: q.Job.Destination.File, Encoder: q.Job.Video.Encoder}
	if j.Source == "" || j.Destination == "" || j.Encoder == "" {
		return Job{}, errors.New("each job requires Job.Source.Path, Job.Destination.File and Job.Video.Encoder")
	}
	if !filepath.IsAbs(j.Source) || !filepath.IsAbs(j.Destination) {
		return Job{}, errors.New("source and destination must be absolute paths on the coordinator")
	}
	j.Source, j.Destination = filepath.Clean(j.Source), filepath.Clean(j.Destination)
	if j.Source == j.Destination {
		return Job{}, errors.New("source and destination must differ")
	}
	return j, nil
}

// Import is atomic and idempotent. A different job targeting an existing destination is rejected.
func (s *Store) Import(ctx context.Context, queue []json.RawMessage) (int, error) {
	return s.importQueue(ctx, queue, false)
}
func (s *Store) importQueue(ctx context.Context, queue []json.RawMessage, preview bool) (int, error) {
	if len(queue) == 0 {
		return 0, errors.New("queue must contain at least one job")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n := 0
	for i, raw := range queue {
		j, err := parseJob(raw)
		if err != nil {
			return 0, fmt.Errorf("job %d: %w", i+1, err)
		}
		// Normalize JSON formatting/key order while preserving all HandBrake fields.
		var value any
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.UseNumber()
		if err = dec.Decode(&value); err != nil {
			return 0, err
		}
		canonical, err := json.Marshal(value)
		if err != nil {
			return 0, err
		}
		sum := sha256.Sum256(canonical)
		result, err := tx.ExecContext(ctx, `INSERT INTO jobs(digest,payload,source,destination,encoder) VALUES(?,?,?,?,?) ON CONFLICT(digest) DO NOTHING`, hex.EncodeToString(sum[:]), canonical, j.Source, j.Destination, j.Encoder)
		if err != nil {
			return 0, fmt.Errorf("job %d (destination %s): %w", i+1, j.Destination, err)
		}
		added, _ := result.RowsAffected()
		n += int(added)
	}
	if preview {
		return n, nil
	}
	return n, tx.Commit()
}

const columns = `id,payload,source,destination,encoder,status,worker,token,lease_until,attempts,error,device_id`

type scanner interface{ Scan(...any) error }

func scanJob(row scanner) (Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.Payload, &j.Source, &j.Destination, &j.Encoder, &j.Status, &j.Worker, &j.Token, &j.LeaseUntil, &j.Attempts, &j.Error, &j.DeviceID)
	return j, err
}

func expire(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE jobs SET status='interrupted',error='worker lease expired' WHERE status='running' AND lease_until<=?`, time.Now().Unix())
	return err
}

func (s *Store) Claim(ctx context.Context, worker string, encoders []string, lease time.Duration) (*Job, error) {
	return s.claimMatching(ctx, worker, encoders, lease, "")
}
func (s *Store) claimMatching(ctx context.Context, worker string, encoders []string, lease time.Duration, deviceID string) (*Job, error) {
	return s.selectClaim(ctx, ClaimRequest{Worker: worker, Encoders: encoders}, lease, deviceID, false)
}
func (s *Store) selectClaim(ctx context.Context, req ClaimRequest, lease time.Duration, deviceID string, preview bool) (*Job, error) {
	if req.Worker == "" || len(req.Encoders) == 0 {
		return nil, errors.New("worker and at least one encoder are required")
	}
	// Read outside the transaction: the store uses a single connection.
	reserved, err := s.reservedEncoders(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = expire(ctx, tx); err != nil {
		return nil, err
	}
	var blocked int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM execution_controls WHERE device_id IN ('',?) AND state != 'playing'", deviceID).Scan(&blocked); err != nil {
		return nil, err
	}
	if blocked > 0 {
		return nil, tx.Commit()
	}
	args := []any{}
	for _, encoder := range req.Encoders {
		args = append(args, encoder)
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(req.Encoders)), ",")
	query := `SELECT ` + columns + ` FROM jobs WHERE status='pending' AND encoder IN (` + marks + `)`
	if !req.presetJobs {
		query += ` AND json_type(CAST(payload AS TEXT), '$.TranscoopPreset') IS NULL AND json_type(CAST(payload AS TEXT), '$.HBBatchPreset') IS NULL`
	}
	j, err := s.firstClaimable(ctx, tx, query+` ORDER BY id LIMIT 500`, args, reserved)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, err
	}
	if expected := req.Expected; expected != nil && (expected.ID != j.ID || expected.Digest != candidateFor(j).Digest) {
		return nil, tx.Commit()
	}
	if preview {
		return &j, tx.Commit()
	}
	b := make([]byte, 24)
	if _, err = rand.Read(b); err != nil {
		return nil, err
	}
	j.Token, j.Worker, j.Status = hex.EncodeToString(b), req.Worker, "running"
	j.LeaseUntil = time.Now().Add(lease).Unix()
	j.DeviceID = deviceID
	j.Attempts++
	_, err = tx.ExecContext(ctx, `UPDATE jobs SET status='running',worker=?,token=?,lease_until=?,attempts=attempts+1,error='',device_id=? WHERE id=?`, j.Worker, j.Token, j.LeaseUntil, deviceID, j.ID)
	if err != nil {
		return nil, err
	}
	return &j, tx.Commit()
}

// firstClaimable returns the oldest pending job not held for a higher-ranked worker.
func (s *Store) firstClaimable(ctx context.Context, tx *sql.Tx, query string, args []any, reserved map[string]bool) (Job, error) {
	if len(reserved) == 0 {
		return scanJob(tx.QueryRowContext(ctx, strings.Replace(query, "LIMIT 500", "LIMIT 1", 1), args...))
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return Job{}, err
	}
	defer rows.Close()
	now := time.Now()
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return Job{}, err
		}
		if !reserved[j.Encoder] || !s.holdJob(j, now) {
			return j, nil
		}
	}
	if err = rows.Err(); err != nil {
		return Job{}, err
	}
	return Job{}, sql.ErrNoRows
}

func (s *Store) Heartbeat(ctx context.Context, id int64, token string, lease time.Duration) error {
	result, err := s.db.ExecContext(ctx, `UPDATE jobs SET lease_until=? WHERE id=? AND token=? AND status='running' AND lease_until>?`, time.Now().Add(lease).Unix(), id, token, time.Now().Unix())
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return ErrLease
	}
	return nil
}

func (s *Store) Active(ctx context.Context, id int64, token string) (Job, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM jobs WHERE id=? AND token=? AND status='running' AND lease_until>?`, id, token, time.Now().Unix()))
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrLease
	}
	return j, err
}

// Finish fences completion by attempt token. publish must be a short local filesystem operation.
func (s *Store) Finish(ctx context.Context, id int64, token, failure string, publish func(Job) error) error {
	failure = Redact(failure, token)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	j, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+columns+` FROM jobs WHERE id=? AND token=? AND status='running' AND lease_until>?`, id, token, time.Now().Unix()))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLease
	}
	if err != nil {
		return err
	}
	status := "failed"
	if failure == "" {
		var state []byte
		e := tx.QueryRowContext(ctx, `SELECT state FROM filesystem_sessions WHERE job_id=? AND token=?`, id, token).Scan(&state)
		if e == nil {
			var session fsSession
			if e = json.Unmarshal(state, &session); e != nil {
				return e
			}
			if !session.Sealed {
				return errors.New("filesystem output has not been sealed")
			}
		} else if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if err = publish(j); err != nil {
			return err
		}
		status = "succeeded"
	}
	_, err = tx.ExecContext(ctx, `UPDATE jobs SET status=?,error=?,lease_until=0 WHERE id=?`, status, failure, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Release returns a running job to the queue because its worker is shutting
// down. The attempt is recorded as interrupted, then the job is pending again,
// so a restart or upgrade does not need an operator to retry it.
func (s *Store) Release(ctx context.Context, id int64, token, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET status='interrupted',error=?,lease_until=0 WHERE id=? AND token=? AND status='running' AND lease_until>?`, Redact(reason, token), id, token, time.Now().Unix())
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return ErrLease
	}
	if _, err = tx.ExecContext(ctx, `UPDATE jobs SET status='pending',worker='',token='',lease_until=0,error='',device_id='' WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) List(ctx context.Context) ([]Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = expire(ctx, tx); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+columns+` FROM jobs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	jobs := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		j.Payload = nil
		j.Error = Redact(j.Error, j.Token)
		j.Token = ""
		jobs = append(jobs, j)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return jobs, tx.Commit()
}

func (s *Store) Retry(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = expire(ctx, tx); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET status='pending',worker='',token='',lease_until=0,error='',device_id='' WHERE id=? AND status IN ('failed','interrupted')`, id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return errors.New("only failed or interrupted jobs can be retried")
	}
	return tx.Commit()
}

// ClearQueue atomically removes non-running jobs and their history. Media and
// active leases are untouched, including jobs claimed just before this transaction.
func (s *Store) ClearQueue(ctx context.Context) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, table := range []string{"attempts", "events"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE job_id IN (SELECT id FROM jobs WHERE status != 'running')"); err != nil {
			return 0, err
		}
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM jobs WHERE status != 'running'")
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}
