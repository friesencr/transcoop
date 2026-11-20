package farm

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef"

// attempt creates an attempt-shaped folder whose files were last touched age ago.
func attempt(t *testing.T, dir, name string, age time.Duration, files ...string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(p, 0700); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	for _, f := range files {
		fp := filepath.Join(p, f)
		if err := os.WriteFile(fp, []byte(strings.Repeat("x", 1000)), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(fp, when, when); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(p, when, when); err != nil {
		t.Fatal(err)
	}
	return p
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestPruneAttemptsByAgeAndFailedOutput(t *testing.T) {
	dir := t.TempDir()
	day := 24 * time.Hour
	fresh := attempt(t, dir, "job-1-"+testToken, time.Hour, "handbrake.log", "queue.json", "output.mkv")
	failed := attempt(t, dir, "job-2-"+testToken, 3*day, "handbrake.log", "queue.json", "output.mkv")
	expired := attempt(t, dir, "job-3-"+testToken, 15*day, "handbrake.log")
	fuseOld := attempt(t, dir, "job-4-attempt-2", 20*day, "handbrake.log")
	active := attempt(t, dir, "job-5-"+testToken, 30*day, "handbrake.log", "source.mkv")
	unrelated := attempt(t, dir, "my-notes", 30*day, "notes.txt")
	notOurs := attempt(t, dir, "job-6-not-a-token", 30*day, "handbrake.log")

	removed, freed, err := pruneAttempts(dir, Retention{}, map[string]bool{active: true}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 || freed == 0 {
		t.Fatalf("removed %d, freed %d", removed, freed)
	}
	for _, p := range []string{expired, fuseOld} {
		if exists(p) {
			t.Errorf("%s should be removed", filepath.Base(p))
		}
	}
	for _, p := range []string{fresh, active, unrelated, notOurs, filepath.Join(fresh, "output.mkv"), filepath.Join(active, "source.mkv")} {
		if !exists(p) {
			t.Errorf("%s should be kept", p)
		}
	}
	if exists(filepath.Join(failed, "output.mkv")) {
		t.Error("failed attempt's output should be removed after failed_output_days")
	}
	for _, f := range []string{"handbrake.log", "queue.json"} {
		if !exists(filepath.Join(failed, f)) {
			t.Errorf("failed attempt's %s should be kept", f)
		}
	}
}

func TestPruneAttemptsByCountKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	for i := range 5 {
		attempt(t, dir, fmt.Sprintf("job-%d-%s", i, testToken), time.Duration(i)*time.Minute, "handbrake.log")
	}
	removed, _, err := pruneAttempts(dir, Retention{Attempts: 3}, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed %d", removed)
	}
	for i := range 5 {
		if got, want := exists(filepath.Join(dir, fmt.Sprintf("job-%d-%s", i, testToken))), i < 3; got != want {
			t.Errorf("job-%d kept=%v want %v", i, got, want)
		}
	}
}

func TestRetentionValidation(t *testing.T) {
	if err := (Retention{}).Validate(); err != nil {
		t.Fatal(err)
	}
	if (Retention{Days: -1}).Validate() == nil {
		t.Fatal("negative days accepted")
	}
	if (Retention{Days: 1, FailedOutputDays: 5}).Validate() == nil {
		t.Fatal("failed_output_days beyond days accepted")
	}
}
