package farm

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestSummariesReportLatestAttempt(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	dir := t.TempDir()
	if _, err := s.Import(ctx, []json.RawMessage{fixture(t, dir, "a"), fixture(t, dir, "b")}); err != nil {
		t.Fatal(err)
	}
	j, err := s.Claim(ctx, "w1", []string{"x265_10bit"}, time.Minute)
	if err != nil || j == nil {
		t.Fatal(j, err)
	}
	fraction, eta := 0.42, 90
	if err = s.PutTelemetry(ctx, j.ID, j.Token, JobTelemetry{Phase: "encoding", Fraction: &fraction, ETA: &eta, Tail: "log"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Summaries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := got[j.ID]
	if len(got) != 1 || !ok || a.Worker != "w1" || a.Number != 1 || a.Started == 0 || a.Finished != 0 {
		t.Fatalf("%+v", got)
	}
	if a.Fraction == nil || *a.Fraction != fraction || a.ETA == nil || *a.ETA != eta || a.FPS != nil || a.Updated == 0 {
		t.Fatalf("telemetry %+v", a)
	}
}
