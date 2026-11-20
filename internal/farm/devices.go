package farm

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrUnauthorized = errors.New("device credential is missing, expired, or revoked")
var ErrPairing = errors.New("invalid or expired pairing code")

type Device struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	Created  int64  `json:"created"`
	LastSeen int64  `json:"last_seen"`
	Expires  int64  `json:"expires"`
	Revoked  bool   `json:"revoked"`
}
type Enrollment struct {
	Device Device `json:"device"`
	Token  string `json:"token"`
}
type Pairing struct {
	Code    string `json:"code"`
	Role    string `json:"role"`
	Expires int64  `json:"expires"`
}

func randomToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func credentialHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
func validRole(role string) bool { return role == "worker" || role == "operator" || role == "browser" }

func (s *Store) migrateV2(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err = tx.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > 2 {
		return errors.New("database was created by a newer transcoop")
	}
	if version == 2 {
		return nil
	}
	var running int
	if err = tx.QueryRow(`SELECT count(*) FROM jobs WHERE status='running' AND lease_until>?`, time.Now().Unix()).Scan(&running); err != nil {
		return err
	}
	if running > 0 {
		return errors.New("stop/drain workers and wait for active leases to expire before upgrading; back up the database first")
	}
	if err = expire(ctx, tx); err != nil {
		return err
	}
	for _, col := range []string{"cost_points", "cost_source"} {
		var n int
		if err = tx.QueryRow(`SELECT count(*) FROM pragma_table_info('jobs') WHERE name=?`, col).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			if _, err = tx.Exec("ALTER TABLE jobs DROP COLUMN " + col); err != nil {
				return err
			}
		}
	}
	if _, err = tx.Exec(`ALTER TABLE jobs ADD COLUMN device_id TEXT NOT NULL DEFAULT '';
CREATE TABLE devices(id TEXT PRIMARY KEY,name TEXT NOT NULL,role TEXT NOT NULL,hash TEXT NOT NULL UNIQUE,created INTEGER NOT NULL,last_seen INTEGER NOT NULL DEFAULT 0,expires INTEGER NOT NULL DEFAULT 0,revoked INTEGER NOT NULL DEFAULT 0);
CREATE TABLE pairing(role TEXT PRIMARY KEY,hash TEXT NOT NULL,expires INTEGER NOT NULL,failures INTEGER NOT NULL DEFAULT 0);
DROP TABLE IF EXISTS worker_status;
PRAGMA user_version=2;`); err != nil {
		return err
	}
	return tx.Commit()
}

// InstallAdmin installs the current private recovery credential. Replacing that
// local credential rotates the single administrator identity instead of leaving
// unrevokable administrator credentials behind.
func (s *Store) InstallAdmin(ctx context.Context, token string) error {
	if len(token) < 32 {
		return errors.New("local administrator credential is invalid; run setup")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO devices(id,name,role,hash,created) VALUES('local-admin','Local administrator','admin',?,?) ON CONFLICT(id) DO UPDATE SET hash=excluded.hash,revoked=0`, credentialHash(token), time.Now().Unix())
	return err
}
func (s *Store) Authenticate(ctx context.Context, token string) (Device, error) {
	var d Device
	if token == "" {
		return d, ErrUnauthorized
	}
	err := s.db.QueryRowContext(ctx, `SELECT id,name,role,created,last_seen,expires,revoked FROM devices WHERE hash=? AND revoked=0 AND (expires=0 OR expires>?)`, credentialHash(token), time.Now().Unix()).Scan(&d.ID, &d.Name, &d.Role, &d.Created, &d.LastSeen, &d.Expires, &d.Revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrUnauthorized
	}
	if err != nil {
		return d, err
	}
	if d.LastSeen < time.Now().Unix()-5 {
		if _, err = s.db.ExecContext(ctx, `UPDATE devices SET last_seen=? WHERE id=?`, time.Now().Unix(), d.ID); err != nil {
			return Device{}, err
		}
	}
	return d, nil
}
func (s *Store) CreatePairing(ctx context.Context, role string) (Pairing, error) {
	if !validRole(role) {
		return Pairing{}, errors.New("role must be browser, worker, or operator")
	}
	const alphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
	// Rejection sampling avoids modulo bias.
	code := make([]byte, 0, 8)
	for len(code) < 8 {
		var b [1]byte
		if _, err := rand.Read(b[:]); err != nil {
			return Pairing{}, err
		}
		if int(b[0]) < 256-256%len(alphabet) {
			code = append(code, alphabet[int(b[0])%len(alphabet)])
		}
	}
	p := Pairing{string(code), role, time.Now().Add(10 * time.Minute).Unix()}
	_, err := s.db.ExecContext(ctx, `INSERT INTO pairing(role,hash,expires,failures) VALUES(?,?,?,0) ON CONFLICT(role) DO UPDATE SET hash=excluded.hash,expires=excluded.expires,failures=0`, role, credentialHash(p.Code), p.Expires)
	return p, err
}
func (s *Store) Redeem(ctx context.Context, role, code, name string) (Enrollment, error) {
	if !validRole(role) || len(name) == 0 || len(name) > 200 {
		return Enrollment{}, ErrPairing
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Enrollment{}, err
	}
	defer tx.Rollback()
	var hash string
	var expires int64
	var failures int
	err = tx.QueryRowContext(ctx, `SELECT hash,expires,failures FROM pairing WHERE role=?`, role).Scan(&hash, &expires, &failures)
	if errors.Is(err, sql.ErrNoRows) {
		return Enrollment{}, ErrPairing
	}
	if err != nil {
		return Enrollment{}, err
	}
	if expires <= time.Now().Unix() || failures >= 5 {
		return Enrollment{}, ErrPairing
	}
	if hash != credentialHash(strings.ToUpper(strings.TrimSpace(code))) {
		if _, err = tx.ExecContext(ctx, `UPDATE pairing SET failures=failures+1 WHERE role=?`, role); err != nil {
			return Enrollment{}, err
		}
		if err = tx.Commit(); err != nil {
			return Enrollment{}, err
		}
		return Enrollment{}, ErrPairing
	}
	e := Enrollment{Device: Device{ID: randomToken(), Name: name, Role: role, Created: time.Now().Unix()}, Token: randomToken()}
	if role == "browser" {
		e.Device.Expires = time.Now().Add(30 * 24 * time.Hour).Unix()
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO devices(id,name,role,hash,created,expires) VALUES(?,?,?,?,?,?)`, e.Device.ID, name, role, credentialHash(e.Token), e.Device.Created, e.Device.Expires); err != nil {
		return Enrollment{}, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM pairing WHERE role=?`, role); err != nil {
		return Enrollment{}, err
	}
	return e, tx.Commit()
}
func (s *Store) Devices(ctx context.Context) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,role,created,last_seen,expires,revoked FROM devices ORDER BY created,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Device{}
	for rows.Next() {
		var d Device
		if err = rows.Scan(&d.ID, &d.Name, &d.Role, &d.Created, &d.LastSeen, &d.Expires, &d.Revoked); err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}
func (s *Store) Revoke(ctx context.Context, id string) error {
	r, err := s.db.ExecContext(ctx, `UPDATE devices SET revoked=1 WHERE id=? AND role!='admin'`, id)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return fmt.Errorf("device not found or is the local administrator")
	}
	return nil
}

type deviceContext struct{}

func requestDevice(ctx context.Context) Device { d, _ := ctx.Value(deviceContext{}).(Device); return d }

// A bounded, coordinator-wide limit also covers browser redemption.
func (s *Store) AllowPairing() bool {
	s.pairMu.Lock()
	defer s.pairMu.Unlock()
	if time.Since(s.pairWindow) >= time.Minute {
		s.pairWindow = time.Now()
		s.pairCount = 0
	}
	if s.pairCount >= 30 {
		return false
	}
	s.pairCount++
	return true
}
