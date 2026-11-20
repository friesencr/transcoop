package farm

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

func TestClearQueue(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "farm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for i, status := range []string{"pending", "running", "succeeded", "failed", "interrupted"} {
		id := i + 1
		_, err := s.db.Exec(`INSERT INTO jobs(id,digest,payload,source,destination,encoder,status) VALUES(?,?,'{}','source',?,'x264',?)`, id, fmt.Sprint(id), fmt.Sprint(id), status)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO attempts(job_id,number,worker,started,status) VALUES(?,1,'worker',1,?)`, id, status); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO events(job_id,kind,at) VALUES(?,'test',1)`, id); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.ClearQueue(ctx)
	if err != nil || n != 4 {
		t.Fatal(n, err)
	}
	for _, table := range []string{"jobs", "attempts", "events"} {
		var count int
		want := 1
		query := "SELECT COUNT(*) FROM " + table
		if table == "events" {
			want = 2 // The import trigger and the explicit test event.
		}
		if err := s.db.QueryRow(query).Scan(&count); err != nil || count != want {
			t.Fatal(table, count, err)
		}
	}
	var status string
	if err := s.db.QueryRow("SELECT status FROM jobs WHERE id=2").Scan(&status); err != nil || status != "running" {
		t.Fatal(status, err)
	}
	if n, err := s.ClearQueue(ctx); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}
