package farm

import (
	"context"
	"errors"
)

// Controls uses an empty key for the whole queue and device IDs for workers.
// An absent entry means playing. Queue and worker controls are independent:
// both must be playing before a worker can claim another job.
func (s *Store) Controls(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT device_id, state FROM execution_controls")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := map[string]string{}
	for rows.Next() {
		var id, state string
		if err := rows.Scan(&id, &state); err != nil {
			return nil, err
		}
		states[id] = state
	}
	return states, rows.Err()
}

func (s *Store) SetControl(ctx context.Context, deviceID, action string) error {
	state := ""
	switch action {
	case "play":
		state = "playing"
	case "pause":
		state = "paused"
	case "stop":
		state = "stopped"
	default:
		return errors.New("choose play, pause, or stop")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if deviceID != "" {
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM devices WHERE id=? AND role='worker'", deviceID).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return errors.New("worker device not found")
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO execution_controls(device_id,state) VALUES(?,?) ON CONFLICT(device_id) DO UPDATE SET state=excluded.state`, deviceID, state); err != nil {
		return err
	}
	if action == "stop" {
		// Invalidating the lease fences publication immediately. Workers cancel their
		// local encode when the next heartbeat is rejected (within 15 seconds online).
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status='interrupted',error='stopped by operator',lease_until=0 WHERE status='running' AND (?='' OR device_id=?)`, deviceID, deviceID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
