package farm

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func TestReleaseRequeuesJobOnWorkerShutdown(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	worker := enroll(t, s, "worker")
	if _, err := s.Import(ctx, []json.RawMessage{fixture(t, t.TempDir(), "a")}); err != nil {
		t.Fatal(err)
	}
	claim := func() Job {
		t.Helper()
		w := apiRequest(s, worker.Token, "POST", "/api/claim", `{"worker":"w1","encoders":["x265_10bit"]}`, "")
		var j Job
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &j) != nil || j.Token == "" {
			t.Fatal(w.Code, w.Body.String())
		}
		return j
	}
	j := claim()
	path := fmt.Sprintf("/api/jobs/%d/complete", j.ID)
	if w := apiRequest(s, worker.Token, "POST", path, `{"release":true}`, "wrong-token"); w.Code < 400 {
		t.Fatal("release accepted with a wrong attempt token", w.Code)
	}
	if w := apiRequest(s, worker.Token, "POST", path, `{"error":"worker w1 shut down","release":true}`, j.Token); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	d, err := s.Detail(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Job.Status != "pending" || d.Job.Worker != "" || d.Job.Error != "" {
		t.Fatalf("job not requeued: %+v", d.Job)
	}
	if len(d.Attempts) != 1 || d.Attempts[0].Status != "interrupted" || d.Attempts[0].Error != "worker w1 shut down" || d.Attempts[0].Finished == 0 {
		t.Fatalf("attempt not closed: %+v", d.Attempts)
	}
	if w := apiRequest(s, worker.Token, "POST", path, `{"release":true}`, j.Token); w.Code < 400 {
		t.Fatal("stale attempt released twice", w.Code)
	}
	if again := claim(); again.ID != j.ID || again.Attempts != 2 {
		t.Fatalf("released job not claimable: %+v", again)
	}
}
