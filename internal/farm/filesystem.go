package farm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/friesencr/transcoop/internal/fsproto"
	"github.com/shirou/gopsutil/v4/disk"
)

// Striped locks serialize filesystem operations without holding a SQL transaction
// over bulk filesystem I/O. They also fence sealing and publication.
var filesystemLocks [64]sync.Mutex

func fsLock(id int64) *sync.Mutex { return &filesystemLocks[uint64(id)%64] }

type fsInput struct {
	Node           fsproto.Node
	Root, Relative string
	Identity       string
}
type fsSession struct {
	Manifest fsproto.Manifest
	Inputs   []fsInput
	Stage    string
	Sealed   bool
}

func (s *Store) migrateFilesystem() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS filesystem_sessions(job_id INTEGER NOT NULL, token TEXT NOT NULL, state BLOB NOT NULL, PRIMARY KEY(job_id,token));
 CREATE TABLE IF NOT EXISTS filesystem_operations(job_id INTEGER NOT NULL, token TEXT NOT NULL, sequence INTEGER NOT NULL, digest TEXT NOT NULL, operation BLOB NOT NULL, done INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(job_id,token,sequence));`)
	return err
}
func declaredSidecars(raw json.RawMessage) ([]string, error) {
	var q struct{ Job struct{ Subtitle any } }
	if err := json.Unmarshal(raw, &q); err != nil {
		return nil, err
	}
	found := map[string]bool{}
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, x := range v {
				if text, ok := x.(string); ok && text != "" && (strings.EqualFold(k, "filename") || strings.EqualFold(k, "file") || strings.EqualFold(k, "path")) {
					found[text] = true
				}
				walk(x)
			}
		case []any:
			for _, x := range v {
				walk(x)
			}
		}
	}
	walk(q.Job.Subtitle)
	out := []string{}
	for p := range found {
		if !filepath.IsAbs(p) {
			return nil, errors.New("external subtitles require absolute coordinator paths")
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}
func buildFSManifest(j Job) (fsSession, error) {
	session := fsSession{Manifest: fsproto.Manifest{Version: 1, Sidecars: map[string]string{}, Output: "output" + filepath.Ext(j.Destination)}, Stage: StagePath(j) + ".fs"}
	parent := filepath.Dir(j.Destination)
	for {
		if _, err := os.Stat(parent); err == nil {
			break
		}
		next := filepath.Dir(parent)
		if next == parent {
			break
		}
		parent = next
	}
	if usage, err := disk.Usage(parent); err == nil {
		session.Manifest.AvailableBytes = usage.Free
	}
	if !fsproto.PortableName(session.Manifest.Output) {
		return session, errors.New("output extension is not portable")
	}
	add := func(original, alias string) error {
		resolved, err := filepath.EvalSymlinks(original)
		if err != nil {
			return err
		}
		info, err := os.Lstat(original)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlink inputs are unsupported")
		}
		root, rel := filepath.Dir(resolved), filepath.Base(resolved)
		if info.IsDir() {
			root = resolved
			rel = "."
		}
		seen := map[string]bool{}
		return filepath.WalkDir(resolved, func(p string, d os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if d.Type()&os.ModeSymlink != 0 {
				return errors.New("symlinks in input trees are unsupported")
			}
			i, e := d.Info()
			if e != nil {
				return e
			}
			if !i.IsDir() && !i.Mode().IsRegular() {
				return errors.New("special input files are unsupported")
			}
			suffix, e := filepath.Rel(resolved, p)
			if e != nil {
				return e
			}
			name := alias
			if suffix != "." {
				for _, part := range strings.Split(filepath.ToSlash(suffix), "/") {
					if !fsproto.PortableName(part) {
						return fmt.Errorf("nonportable input name %q", part)
					}
				}
				name += "/" + filepath.ToSlash(suffix)
			}
			folded := strings.ToLower(name)
			if seen[folded] {
				return errors.New("case-colliding input names")
			}
			seen[folded] = true
			relative := rel
			if info.IsDir() {
				relative = suffix
			}
			n := fsproto.Node{ID: fmt.Sprint(len(session.Inputs) + 1), Path: name, Size: i.Size(), Modified: i.ModTime().UnixNano(), Directory: i.IsDir()}
			identity := ""
			if !i.IsDir() {
				handle, e := os.Open(p)
				if e != nil {
					return e
				}
				identity, e = fsIdentity(handle)
				handle.Close()
				if e != nil {
					return e
				}
			}
			session.Inputs = append(session.Inputs, fsInput{Node: n, Root: root, Relative: relative, Identity: identity})
			session.Manifest.Nodes = append(session.Manifest.Nodes, n)
			if len(session.Inputs) > 100000 {
				return errors.New("input tree exceeds 100000 nodes")
			}
			return nil
		})
	}
	info, err := os.Stat(j.Source)
	if err != nil {
		return session, err
	}
	session.Manifest.Source = "source"
	if !info.IsDir() {
		session.Manifest.Source += filepath.Ext(j.Source)
	}
	if !fsproto.PortableName(session.Manifest.Source) {
		return session, errors.New("source extension is not portable")
	}
	if err = add(j.Source, session.Manifest.Source); err != nil {
		return session, err
	}
	paths, err := declaredSidecars(j.Payload)
	if err != nil {
		return session, err
	}
	for i, p := range paths {
		info, e := os.Lstat(p)
		if e != nil {
			return session, e
		}
		if !info.Mode().IsRegular() {
			return session, errors.New("external subtitles must be regular files")
		}
		alias := fmt.Sprintf("subtitle-%d%s", i, filepath.Ext(p))
		if !fsproto.PortableName(alias) {
			return session, errors.New("subtitle extension is not portable")
		}
		if err = add(p, alias); err != nil {
			return session, err
		}
		session.Manifest.Sidecars[p] = alias
	}
	return session, nil
}
func (s *Store) fsSession(ctx context.Context, j Job) (fsSession, error) {
	var b []byte
	var v fsSession
	err := s.db.QueryRowContext(ctx, `SELECT state FROM filesystem_sessions WHERE job_id=? AND token=?`, j.ID, j.Token).Scan(&b)
	if err == nil {
		err = json.Unmarshal(b, &v)
		return v, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return v, err
	}
	v, err = buildFSManifest(j)
	if err != nil {
		return v, err
	}
	b, err = json.Marshal(v)
	if err != nil {
		return v, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO filesystem_sessions VALUES(?,?,?)`, j.ID, j.Token, b)
	return v, err
}
func (s *Server) fsAuthorized(r *http.Request) (Job, error) {
	j, err := s.active(r)
	if err != nil {
		return j, err
	}
	d := requestDevice(r.Context())
	if d.Role != "worker" || d.ID != j.DeviceID {
		return j, errors.New("filesystem access requires the assigned paired worker")
	}
	return j, nil
}
func (s *Server) filesystem(w http.ResponseWriter, r *http.Request) {
	j, err := s.fsAuthorized(r)
	if err != nil {
		apiError(w, err)
		return
	}
	lock := fsLock(j.ID)
	lock.Lock()
	locked := true
	defer func() {
		if locked {
			lock.Unlock()
		}
	}()
	if _, err = s.Store.Active(r.Context(), j.ID, j.Token); err != nil {
		apiError(w, err)
		return
	}
	v, err := s.Store.fsSession(r.Context(), j)
	if err != nil {
		apiError(w, err)
		return
	}
	switch r.PathValue("action") {
	case "manifest":
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		respond(w, v.Manifest)
		return
	case "read":
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		off, e1 := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		size, e2 := strconv.Atoi(r.URL.Query().Get("size"))
		if e1 != nil || e2 != nil || off < 0 || size < 1 || size > fsproto.BlockSize {
			apiError(w, errors.New("invalid read range"))
			return
		}
		var f *os.File
		if name := r.URL.Query().Get("output"); name != "" {
			if !fsproto.PortableName(name) {
				apiError(w, errors.New("invalid output name"))
				return
			}
			root, e := os.OpenRoot(v.Stage)
			if e != nil {
				apiError(w, e)
				return
			}
			defer root.Close()
			f, err = root.Open(name)
		} else {
			lock.Unlock()
			locked = false
			var input *fsInput
			for i := range v.Inputs {
				if v.Inputs[i].Node.ID == r.URL.Query().Get("node") {
					input = &v.Inputs[i]
					break
				}
			}
			if input == nil || input.Node.Directory {
				apiError(w, errors.New("unknown input node"))
				return
			}
			root, e := os.OpenRoot(input.Root)
			if e != nil {
				apiError(w, e)
				return
			}
			defer root.Close()
			f, err = root.Open(input.Relative)
			if err == nil {
				var info os.FileInfo
				info, err = f.Stat()
				if err == nil {
					identity, e := fsIdentity(f)
					if e != nil {
						err = e
					} else if identity != input.Identity {
						err = errors.New("source identity changed during encoding")
					}
				}
				if err == nil && (!info.Mode().IsRegular() || info.Size() != input.Node.Size || info.ModTime().UnixNano() != input.Node.Modified) {
					err = errors.New("source changed during encoding")
				}
			}
		}
		if f != nil {
			defer f.Close()
		}
		if err != nil {
			apiError(w, err)
			return
		}
		before, err := f.Stat()
		if err != nil {
			apiError(w, err)
			return
		}
		b := make([]byte, size)
		n, e := f.ReadAt(b, off)
		if e != nil && !errors.Is(e, io.EOF) {
			apiError(w, e)
			return
		}
		after, e := f.Stat()
		if e != nil || before.Size() != after.Size() || before.ModTime() != after.ModTime() {
			apiError(w, errors.New("source changed during read"))
			return
		}
		if _, err = s.Store.Active(r.Context(), j.ID, j.Token); err != nil {
			apiError(w, err)
			return
		}
		w.Header().Set("X-Content-SHA256", fsproto.Digest(b[:n]))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(b[:n])
		return
	case "batch":
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		var batch struct {
			Operations []fsproto.Operation `json:"operations"`
		}
		err = decodeLimit(w, r, &batch, 6<<20)
		if err == nil {
			size := 0
			if len(batch.Operations) == 0 || len(batch.Operations) > 4096 {
				err = errors.New("invalid operation batch")
			}
			for _, op := range batch.Operations {
				size += len(op.Data)
				if e := op.Validate(); e != nil {
					err = e
				}
			}
			if size > fsproto.MaxWrite {
				err = errors.New("batch exceeds 4 MiB")
			}
		}
		if err == nil {
			for _, op := range batch.Operations {
				if err = s.Store.applyFSOperation(r.Context(), j, &v, op); err != nil {
					break
				}
			}
		}
	case "operations":
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		var op fsproto.Operation
		err = decodeLimit(w, r, &op, 6<<20)
		if err == nil {
			err = op.Validate()
		}
		if err == nil {
			err = s.Store.applyFSOperation(r.Context(), j, &v, op)
		}
	case "cleanup":
		if r.Method != "DELETE" {
			w.WriteHeader(405)
			return
		}
		err = os.RemoveAll(v.Stage)
	default:
		w.WriteHeader(404)
		return
	}
	if err != nil {
		apiError(w, err)
		return
	}
	w.WriteHeader(204)
}
func (s *Store) applyFSOperation(ctx context.Context, j Job, v *fsSession, op fsproto.Operation) error {
	var digest string
	var done int
	err := s.db.QueryRowContext(ctx, `SELECT digest,done FROM filesystem_operations WHERE job_id=? AND token=? AND sequence=?`, j.ID, j.Token, op.Sequence).Scan(&digest, &done)
	if err == nil {
		if digest != op.Digest {
			return errors.New("conflicting operation replay")
		}
		if done == 1 {
			return nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if op.Kind == "seal" && op.Name != v.Manifest.Output {
		return errors.New("seal requires the declared output name")
	}
	if v.Sealed {
		return errors.New("output is sealed")
	}
	var last int64
	if err = s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0) FROM filesystem_operations WHERE job_id=? AND token=? AND done=1`, j.ID, j.Token).Scan(&last); err != nil {
		return err
	}
	if op.Sequence != last+1 {
		return errors.New("out-of-order operation")
	}
	b, _ := json.Marshal(op)
	if _, err = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO filesystem_operations VALUES(?,?,?,?,?,0)`, j.ID, j.Token, op.Sequence, op.Digest, b); err != nil {
		return err
	}
	if err = os.MkdirAll(v.Stage, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(v.Stage)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("unsafe filesystem staging directory")
	}
	root, err := os.OpenRoot(v.Stage)
	if err != nil {
		return err
	}
	defer root.Close()
	switch op.Kind {
	case "create", "write", "truncate", "flush", "seal":
		flags := os.O_RDWR
		if op.Kind == "create" {
			flags |= os.O_CREATE
		}
		f, e := root.OpenFile(op.Name, flags, outputMode)
		if e != nil {
			return e
		}
		switch op.Kind {
		case "write":
			_, err = f.WriteAt(op.Data, op.Offset)
		case "truncate":
			err = f.Truncate(op.Offset)
		}
		if err == nil {
			err = f.Sync()
		}
		ce := f.Close()
		if err == nil {
			err = ce
		}
	case "unlink":
		err = root.Remove(op.Name)
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	case "rename":
		err = root.Rename(op.Name, op.Target)
		if errors.Is(err, os.ErrNotExist) {
			_, err = root.Stat(op.Target)
		}
	}
	if err != nil {
		return err
	}
	// Sync namespace changes before acknowledging their sequence.
	if err = syncDirectory(v.Stage); err != nil {
		return err
	}
	if _, err = s.Active(ctx, j.ID, j.Token); err != nil {
		return err
	}
	if op.Kind == "seal" {
		if op.Name != v.Manifest.Output {
			return errors.New("seal requires the declared output name")
		}
		source := filepath.Join(v.Stage, op.Name)
		if err = os.Link(source, StagePath(j)); err != nil {
			a, e := os.Stat(source)
			b, e2 := os.Stat(StagePath(j))
			if e != nil || e2 != nil || !os.SameFile(a, b) {
				return err
			}
		}
		if err = syncDirectory(filepath.Dir(j.Destination)); err != nil {
			return err
		}
		v.Sealed = true
	}
	state, _ := json.Marshal(v)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE filesystem_sessions SET state=? WHERE job_id=? AND token=?`, state, j.ID, j.Token); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE filesystem_operations SET done=1,operation=X'' WHERE job_id=? AND token=? AND sequence=?`, j.ID, j.Token, op.Sequence); err != nil {
		return err
	}
	return tx.Commit()
}

// CleanupFilesystem removes only paths recorded by transcoop for inactive
// attempts. Sessions belonging to still-valid leases survive coordinator restart.
func (s *Store) CleanupFilesystem(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT f.job_id,f.token,f.state FROM filesystem_sessions f LEFT JOIN jobs j ON j.id=f.job_id WHERE j.id IS NULL OR j.token!=f.token OR j.status!='running' OR j.lease_until<=unixepoch()`)
	if err != nil {
		return err
	}
	type expired struct {
		id    int64
		token string
		state []byte
	}
	var list []expired
	for rows.Next() {
		var v expired
		if err = rows.Scan(&v.id, &v.token, &v.state); err != nil {
			rows.Close()
			return err
		}
		list = append(list, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, v := range list {
		lock := fsLock(v.id)
		lock.Lock()
		var state fsSession
		e := json.Unmarshal(v.state, &state)
		if e == nil {
			e = os.RemoveAll(state.Stage)
			if e == nil {
				stage := strings.TrimSuffix(state.Stage, ".fs")
				e = os.Remove(stage)
				if errors.Is(e, os.ErrNotExist) {
					e = nil
				}
			}
		}
		if e == nil {
			_, e = s.db.ExecContext(ctx, `DELETE FROM filesystem_sessions WHERE job_id=? AND token=?`, v.id, v.token)
		}
		if e == nil {
			_, e = s.db.ExecContext(ctx, `DELETE FROM filesystem_operations WHERE job_id=? AND token=?`, v.id, v.token)
		}
		lock.Unlock()
		if e != nil {
			return e
		}
	}
	return nil
}
