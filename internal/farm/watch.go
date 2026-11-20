package farm

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Watch is a folder the coordinator scans for new videos to queue.
type Watch struct {
	ID        int64
	Source    string
	Output    string
	Preset    string
	Recursive bool
	Enabled   bool
	Created   int64
}

// WatchFile records what happened to one version (size and modification time)
// of a file in a watched folder, so it is never queued twice.
type WatchFile struct {
	Path   string
	Size   int64
	MTime  int64 // nanoseconds
	Result string
	Detail string
	At     int64
}

// Watch file results.
const (
	WatchQueued   = "queued"   // added to the queue
	WatchSkipped  = "skipped"  // output exists, already queued, or present when the watch began
	WatchFailed   = "failed"   // could not be prepared; fixed by changing the file
	WatchExisting = "existing" // present when the watch was created
)

func (s *Store) migrateWatches() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS watches(id INTEGER PRIMARY KEY, source TEXT NOT NULL UNIQUE, output TEXT NOT NULL, preset TEXT NOT NULL, recursive INTEGER NOT NULL DEFAULT 0, enabled INTEGER NOT NULL DEFAULT 1, created INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS watch_files(watch_id INTEGER NOT NULL, path TEXT NOT NULL, size INTEGER NOT NULL, mtime INTEGER NOT NULL, result TEXT NOT NULL, detail TEXT NOT NULL DEFAULT '', at INTEGER NOT NULL, PRIMARY KEY(watch_id,path));
CREATE INDEX IF NOT EXISTS watch_files_recent ON watch_files(watch_id,at);`)
	return err
}

// ErrWatchExists reports a second watch on the same folder.
var ErrWatchExists = errors.New("this folder is already watched")

func (s *Store) AddWatch(ctx context.Context, w Watch) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO watches(source,output,preset,recursive,enabled,created) VALUES(?,?,?,?,1,?)`, w.Source, w.Output, w.Preset, w.Recursive, time.Now().Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, ErrWatchExists
		}
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) Watches(ctx context.Context) ([]Watch, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,source,output,preset,recursive,enabled,created FROM watches ORDER BY source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Watch{}
	for rows.Next() {
		var w Watch
		if err = rows.Scan(&w.ID, &w.Source, &w.Output, &w.Preset, &w.Recursive, &w.Enabled, &w.Created); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) SetWatchEnabled(ctx context.Context, id int64, enabled bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE watches SET enabled=? WHERE id=?`, enabled, id)
	if err == nil {
		if n, _ := res.RowsAffected(); n != 1 {
			err = sql.ErrNoRows
		}
	}
	return err
}

// DeleteWatch stops watching; queued jobs and media are untouched.
func (s *Store) DeleteWatch(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM watch_files WHERE watch_id=?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM watches WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

// WatchFiles returns every recorded file of a watch, keyed by path.
func (s *Store) WatchFiles(ctx context.Context, watchID int64) (map[string]WatchFile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path,size,mtime,result,detail,at FROM watch_files WHERE watch_id=?`, watchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]WatchFile{}
	for rows.Next() {
		var f WatchFile
		if err = rows.Scan(&f.Path, &f.Size, &f.MTime, &f.Result, &f.Detail, &f.At); err != nil {
			return nil, err
		}
		out[f.Path] = f
	}
	return out, rows.Err()
}

func (s *Store) RecordWatchFiles(ctx context.Context, watchID int64, files []WatchFile) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, f := range files {
		if f.At == 0 {
			f.At = time.Now().Unix()
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO watch_files(watch_id,path,size,mtime,result,detail,at) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(watch_id,path) DO UPDATE SET size=excluded.size,mtime=excluded.mtime,result=excluded.result,detail=excluded.detail,at=excluded.at`,
			watchID, f.Path, f.Size, f.MTime, f.Result, f.Detail, f.At); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// WatchSummary counts a watch's recorded files and lists its latest activity.
type WatchSummary struct {
	Counts map[string]int
	Recent []WatchFile
}

func (s *Store) WatchSummaries(ctx context.Context, recent int) (map[int64]WatchSummary, error) {
	out := map[int64]WatchSummary{}
	get := func(id int64) WatchSummary {
		v, ok := out[id]
		if !ok {
			v = WatchSummary{Counts: map[string]int{}}
		}
		return v
	}
	rows, err := s.db.QueryContext(ctx, `SELECT watch_id,result,count(*) FROM watch_files GROUP BY watch_id,result`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var result string
		var n int
		if err = rows.Scan(&id, &result, &n); err != nil {
			rows.Close()
			return nil, err
		}
		v := get(id)
		v.Counts[result] = n
		out[id] = v
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT watch_id,path,size,mtime,result,detail,at FROM (
 SELECT *,row_number() OVER (PARTITION BY watch_id ORDER BY at DESC,rowid DESC) AS n FROM watch_files WHERE result!='existing')
WHERE n<=? ORDER BY watch_id,at DESC`, recent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var f WatchFile
		if err = rows.Scan(&id, &f.Path, &f.Size, &f.MTime, &f.Result, &f.Detail, &f.At); err != nil {
			return nil, err
		}
		v := get(id)
		v.Recent = append(v.Recent, f)
		out[id] = v
	}
	return out, rows.Err()
}
