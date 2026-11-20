package farm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Worker ranks order job assignment between idle workers. A job is held back
// from a worker while a higher-ranked worker is online, accepting jobs, has a
// free slot and supports the job's encoder. The hold lasts at most ClaimGrace
// per job, so a preferred worker that declines work cannot stall the queue.
const (
	RankFirst  = 1
	RankNormal = 0
	RankLast   = -1
)

// ClaimGrace is how long a job waits for a higher-ranked worker.
var ClaimGrace = 30 * time.Second

func RankName(rank int) string {
	switch rank {
	case RankFirst:
		return "first"
	case RankLast:
		return "last"
	}
	return "normal"
}

func ParseRank(name string) (int, error) {
	switch name {
	case "first":
		return RankFirst, nil
	case "normal":
		return RankNormal, nil
	case "last":
		return RankLast, nil
	}
	return 0, fmt.Errorf("rank must be first, normal or last, not %q", name)
}

func (s *Store) migrateRanks() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS worker_ranks(device_id TEXT PRIMARY KEY, rank INTEGER NOT NULL)`)
	return err
}

// Ranks returns non-normal ranks by worker device ID.
func (s *Store) Ranks(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT device_id,rank FROM worker_ranks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var rank int
		if err = rows.Scan(&id, &rank); err != nil {
			return nil, err
		}
		out[id] = rank
	}
	return out, rows.Err()
}

// SetRank sets a paired worker's rank.
func (s *Store) SetRank(ctx context.Context, deviceID string, rank int) error {
	if rank < RankLast || rank > RankFirst {
		return errors.New("invalid rank")
	}
	var role string
	err := s.db.QueryRowContext(ctx, `SELECT role FROM devices WHERE id=?`, deviceID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) || role != "worker" {
		return errors.New("unknown worker")
	}
	if err != nil {
		return err
	}
	if rank == RankNormal {
		_, err = s.db.ExecContext(ctx, `DELETE FROM worker_ranks WHERE device_id=?`, deviceID)
	} else {
		_, err = s.db.ExecContext(ctx, `INSERT INTO worker_ranks VALUES(?,?) ON CONFLICT(device_id) DO UPDATE SET rank=excluded.rank`, deviceID, rank)
	}
	return err
}

// reservedEncoders lists encoders that an idle worker ranked above deviceID
// can run; jobs using them are held back from deviceID for ClaimGrace.
func (s *Store) reservedEncoders(ctx context.Context, deviceID string) (map[string]bool, error) {
	ranks, err := s.Ranks(ctx)
	if err != nil || len(ranks) == 0 {
		return nil, err
	}
	workers, err := s.Workers(ctx)
	if err != nil {
		return nil, err
	}
	controls, err := s.Controls(ctx)
	if err != nil {
		return nil, err
	}
	accepting := func(id string) bool { st := controls[id]; return st == "" || st == "playing" }
	if !accepting("") {
		return nil, nil
	}
	mine := ranks[deviceID]
	out := map[string]bool{}
	for _, w := range workers {
		if w.DeviceID == "" || w.DeviceID == deviceID || ranks[w.DeviceID] <= mine {
			continue
		}
		if w.Presence() != "online" || !accepting(w.DeviceID) || len(w.Active) >= max(w.MaxJobs, 1) {
			continue
		}
		for _, e := range w.Encoders {
			out[e] = true
		}
	}
	return out, nil
}

// holdKey identifies one queued attempt of a job, so a retried or requeued
// job waits for a higher-ranked worker again instead of inheriting the wait it
// used up last time.
type holdKey struct {
	id       int64
	attempts int
}

// holdJob reports whether the job is still within its wait for a
// higher-ranked worker, starting the wait on first use.
func (s *Store) holdJob(j Job, now time.Time) bool {
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	if s.held == nil {
		s.held = map[holdKey]time.Time{}
	}
	for k, t := range s.held {
		if now.Sub(t) > 10*ClaimGrace {
			delete(s.held, k)
		}
	}
	key := holdKey{j.ID, j.Attempts}
	first, ok := s.held[key]
	if !ok {
		first = now
		s.held[key] = now
	}
	return now.Sub(first) < ClaimGrace
}
