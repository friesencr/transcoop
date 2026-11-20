package farm

import (
	"bytes"
	"fmt"
	"log"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFragmentedMalformedProgress(t *testing.T) {
	data := `ordinary stdout
Progress: {"State":"SCANNING"}
Progress: {broken
Progress: {"State":"WORKING","Working":{"PassID":-1}}
Progress: {
 "State":"WORKING", "Working":{"PassID":1}
}

Progress: {"State":"WORKING","Working":{"PassID":2}}
Progress: {"State":"WORKING","Working":{}}
Progress: {"State":"MUXING"}
Progress: {"State":"WORKDONE"}
`
	want := []string{"preparation", "subtitle search", "video analysis", "video encoding", "unknown", "muxing", "awaiting process exit"}
	for _, size := range []int{1, 3, 19, len(data)} {
		var log bytes.Buffer
		var phases []string
		p := &progressWriter{log: &log, phase: func(s string) { phases = append(phases, s) }}
		for i := 0; i < len(data); i += size {
			_, _ = p.Write([]byte(data[i:min(len(data), i+size)]))
		}
		if log.String() != data {
			t.Fatal("lost log data")
		}
		if !reflect.DeepEqual(phases, want) {
			t.Fatalf("fragment %d: %v", size, phases)
		}
		_, _ = p.Write([]byte("Progress: {" + strings.Repeat("x", 1000000)))
		if len(p.buffer) > 64*1024 {
			t.Fatal("unbounded buffer")
		}
		_, _ = p.Write([]byte(`Progress: {"State":"SCANNING"}`))
		if phases[len(phases)-1] != "preparation" {
			t.Fatal("failed to resynchronize")
		}
	}
}
func TestSeparateStderrAndLosslessConcurrentLog(t *testing.T) {
	var log bytes.Buffer
	locked := &lockedWriter{w: &log}
	var phases []string
	p := &progressWriter{log: locked, phase: func(s string) { phases = append(phases, s) }}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			fmt.Fprintf(locked, "stderr %d Progress: {\"State\":\"MUXING\"}\n", i)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			fmt.Fprintf(p, "stdout %d Progress: {\"State\":\"WORKING\",\"Working\":{\"PassID\":1}}\n", i)
		}
	}()
	wg.Wait()
	if !reflect.DeepEqual(phases, []string{"video analysis"}) {
		t.Fatal(phases)
	}
	for _, stream := range []string{"stdout", "stderr"} {
		for i := 0; i < 1000; i++ {
			if strings.Count(log.String(), fmt.Sprintf("%s %d Progress:", stream, i)) != 1 {
				t.Fatal("lost or duplicate log")
			}
		}
	}
}

func TestProgressValuesFromFragmentedRecords(t *testing.T) {
	data := `Progress: {"State":"WORKING","Working":{"PassID":2,"Progress":0.421,"Rate":110.2,"RateAvg":106.1,"ETASeconds":491,"Pass":2,"PassCount":2}}
Progress: {"State":"WORKING","Working":{"PassID":0}}
Progress: {"State":"WORKING","Working":{"Progress":-1}}
Progress: {"State":"WORKING","Working":{"Progress":1.1}}
Progress: {"State":"SCANNING","Working":{"Progress":0.1}}
`
	var raw bytes.Buffer
	var got []encodeProgress
	w := &progressWriter{log: &raw, progress: func(p encodeProgress) { got = append(got, p) }}
	for _, b := range []byte(data) {
		if _, err := w.Write([]byte{b}); err != nil {
			t.Fatal(err)
		}
	}
	eta := 491
	want := []encodeProgress{{"video encoding", 0.421, 110.2, 106.1, &eta, 2, 2}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("progress: %+v", got)
	}
	if raw.String() != data {
		t.Fatal("lost raw progress")
	}
}

func TestProgressLogThrottling(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	now := time.Unix(100, 0)
	l := jobProgressLogger{id: 42, now: func() time.Time { return now }}
	eta := 491
	p := encodeProgress{"video encoding", 0.421, 110.2, 106.1, &eta, 1, 2}
	l.report(p)
	for i := 0; i < 9; i++ {
		now = now.Add(time.Second)
		l.report(p)
	}
	if strings.Count(output.String(), "job 42:") != 1 {
		t.Fatal("progress not throttled")
	}
	if !strings.Contains(output.String(), "42.1% pass 1/2 | 110.2 fps (avg 106.1) | ETA 8m11s") {
		t.Fatal(output.String())
	}
	now = now.Add(time.Second)
	l.report(p)
	p.pass = 2
	l.report(p)
	p.fraction = 1
	l.report(p)
	l.report(p)
	p.phase = "subtitle search"
	p.etaSeconds = nil
	p.rate, p.average, p.fraction = 0, 0, 0
	l.report(p)
	if strings.Count(output.String(), "job 42:") != 5 {
		t.Fatalf("missing interval/pass/completion/phase update: %s", &output)
	}
	lastLine := strings.Split(strings.TrimSpace(output.String()), "\n")[4]
	if strings.Contains(lastLine, "ETA") || strings.Contains(lastLine, "fps") {
		t.Fatal("invented unavailable metrics")
	}
}
