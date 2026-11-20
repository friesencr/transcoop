package farm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
)

func workloadPayload(t *testing.T, source string, width int, quality int) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{"Job": map[string]any{"Source": map[string]any{"Path": testRoot + source}, "Destination": map[string]any{"File": testRoot + source + ".out.mkv"}, "Video": map[string]any{"Encoder": "x265_10bit", "Quality": quality}}, "Title": map[string]any{"Geometry": map[string]int{"Width": width, "Height": 480}, "FrameRate": map[string]int{"Num": 30000, "Den": 1001}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestWorkloadFingerprint(t *testing.T) {
	a := workloadKey(workloadPayload(t, "/a", 720, 20))
	if a == "" || a != workloadKey(workloadPayload(t, "/b", 720, 20)) {
		t.Fatal("paths split equivalent workloads")
	}
	if a == workloadKey(workloadPayload(t, "/a", 1920, 20)) || a == workloadKey(workloadPayload(t, "/a", 720, 22)) {
		t.Fatal("mixed resolution or settings")
	}
	if workloadKey(fixture(t, t.TempDir(), "unknown")) != "" {
		t.Fatal("guessed missing metadata")
	}
}
func TestCandidatePreviewAndFencedClaim(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	worker := enroll(t, s, "worker")
	raw := workloadPayload(t, "/a", 720, 20)
	if _, err := s.Import(ctx, []json.RawMessage{raw}); err != nil {
		t.Fatal(err)
	}
	body := `{"worker":"test","encoders":["x265_10bit"]}`
	w := apiRequest(s, worker.Token, "POST", "/api/claim/preview", body, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var candidate ClaimCandidate
	if err := json.Unmarshal(w.Body.Bytes(), &candidate); err != nil {
		t.Fatal(err)
	}
	if candidate.Workload == "" || candidate.Digest == "" {
		t.Fatal(candidate)
	}
	jobs, _ := s.List(ctx)
	if len(jobs) != 1 || jobs[0].Status != "pending" || jobs[0].Attempts != 0 {
		t.Fatal("preview claimed", jobs)
	}
	// Clearing/reimporting can reuse the ID. Its payload digest must still match.
	if _, err := s.ClearQueue(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Import(ctx, []json.RawMessage{workloadPayload(t, "/other", 1920, 25)}); err != nil {
		t.Fatal(err)
	}
	req, _ := json.Marshal(ClaimRequest{Worker: "test", Encoders: []string{"x265_10bit"}, Expected: &candidate})
	w = apiRequest(s, worker.Token, "POST", "/api/claim", string(req), "")
	if w.Code != http.StatusNoContent {
		t.Fatal("claimed unreviewed workload", w.Code)
	}
	w = apiRequest(s, worker.Token, "POST", "/api/claim/preview", body, "")
	json.Unmarshal(w.Body.Bytes(), &candidate)
	req, _ = json.Marshal(ClaimRequest{Worker: "test", Encoders: []string{"x265_10bit"}, Expected: &candidate})
	w = apiRequest(s, worker.Token, "POST", "/api/claim", string(req), "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := s.SetControl(ctx, "", "pause"); err != nil {
		t.Fatal(err)
	}
	if w = apiRequest(s, worker.Token, "POST", "/api/claim/preview", body, ""); w.Code != 204 {
		t.Fatal("preview ignored pause", w.Code)
	}
}

func TestPresetClaimCompatibility(t *testing.T) {
	for _, key := range []string{"TranscoopPreset", "HBBatchPreset"} {
		t.Run(key, func(t *testing.T) {
			s := newStore(t)
			worker := enroll(t, s, "worker")
			dir := t.TempDir()
			var presetJob map[string]json.RawMessage
			if err := json.Unmarshal(fixture(t, dir, "preset"), &presetJob); err != nil {
				t.Fatal(err)
			}
			presetJob[key] = json.RawMessage(`{"Version":2,"Name":"test","Export":{"PresetList":[{"PresetName":"test","VideoEncoder":"x265_10bit"}]}}`)
			raw, err := json.Marshal(presetJob)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Import(t.Context(), []json.RawMessage{raw, fixture(t, dir, "ordinary")}); err != nil {
				t.Fatal(err)
			}

			const body = `{"worker":"test","encoders":["x265_10bit"]}`
			for _, step := range []struct {
				path string
				id   int64
			}{
				{"/api/claim/preview?preset_jobs=1", 1},
				{"/api/claim/preview", 2},
				{"/api/claim", 2},
				{"/api/claim?preset_jobs=1", 1},
			} {
				w := apiRequest(s, worker.Token, "POST", step.path, body, "")
				var reply struct{ ID int64 }
				if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &reply) != nil || reply.ID != step.id {
					t.Fatalf("%s: expected job %d, got HTTP %d: %s", step.path, step.id, w.Code, w.Body.String())
				}
			}
		})
	}
}

func TestLearningDemandSettlingAndSuccess(t *testing.T) {
	now := time.Unix(100, 0)
	observation := func(ramp int) workloadObservation {
		o := workloadObservation{started: now}
		for i := 1; i <= ramp+30; i++ {
			v := 20.0
			if i <= ramp {
				v = float64(i) * 20 / float64(ramp)
			}
			o.sample(now.Add(time.Duration(i)*time.Second), v, 50, 85)
		}
		return o
	}
	fast, slow := observation(1), observation(30)
	if !fast.usable() || !slow.usable() || slow.settle <= fast.settle {
		t.Fatal(fast, slow)
	}
	a := admissionState{cap: 4}
	a.cpu.last = now.Add(time.Minute)
	r := &attemptReservation{workload: "dvd", observation: slow}
	a.learnCompletion(r)
	if len(a.profiles) != 0 {
		t.Fatal("learned failed job")
	}
	r.succeeded = true
	a.learnCompletion(r)
	if a.profiles["dvd"].trusted(a.cpu.last) {
		t.Fatal("trusted a single run")
	}
	a.learnCompletion(r)
	if !a.profiles["dvd"].trusted(a.cpu.last) || a.profiles["dvd"].settle != slow.settle {
		t.Fatal(a.profiles)
	}
	o := workloadObservation{started: now}
	for i := 1; i <= 40; i++ {
		o.sample(now.Add(time.Duration(i)*time.Second), 10, 95, 85)
	}
	if o.usable() {
		t.Fatal("learned demand under saturation")
	}
}
func TestProcessCPULearningNormalizationAndFailure(t *testing.T) {
	now := time.Unix(100, 0)
	r := &attemptReservation{phase: "video encoding", workload: "dvd", pid: 1, observation: workloadObservation{started: now}}
	a := admissionState{cap: 4, target: 85, active: map[attemptKey]*attemptReservation{{1, "a"}: r}, profiles: map[string]workloadProfile{}}
	total := 0.0
	deps := admissionDeps{cpuCount: 8, processCPU: func(context.Context, int) (float64, error) { return total, nil }}
	for i := 0; i < 30; i++ {
		now = now.Add(time.Second)
		total += 1.6
		a.observe(now, 40, nil)
		a.sampleProcesses(context.Background(), now, deps)
	}
	if !r.observation.usable() || r.observation.peak < 19.9 || r.observation.peak > 20.1 {
		t.Fatal(r.observation)
	}
	deps.processCPU = func(context.Context, int) (float64, error) { return 0, errors.New("unavailable") }
	a.sampleProcesses(context.Background(), now.Add(time.Second), deps)
	if !a.metricsFailed || r.observation.usable() {
		t.Fatal("used failed measurement")
	}
}

func TestLateCPURiseAndProfileInvalidation(t *testing.T) {
	now := time.Unix(100, 0)
	o := workloadObservation{started: now}
	for i := 1; i <= 25; i++ {
		o.sample(now.Add(time.Duration(i)*time.Second), 10, 50, 85)
	}
	early := o.settle
	for i := 26; i <= 50; i++ {
		o.sample(now.Add(time.Duration(i)*time.Second), 30, 50, 85)
	}
	if o.settle <= early || o.peak != 30 {
		t.Fatal("late ramp ignored", o)
	}
	r := &attemptReservation{phase: "video encoding", workload: "dvd", pid: 1, previousCPU: 0, previousAt: now, observation: workloadObservation{started: now}}
	a := admissionState{target: 85, active: map[attemptKey]*attemptReservation{{1, "a"}: r}, profiles: map[string]workloadProfile{"dvd": {successes: 2, demand: 10, settle: 10, updated: now}}, refills: map[string][]time.Time{"dvd": {now.Add(time.Minute)}}}
	now = now.Add(time.Second)
	a.observe(now, 50, nil)
	a.sampleProcesses(context.Background(), now, admissionDeps{cpuCount: 4, processCPU: func(context.Context, int) (float64, error) { return 1.2, nil }})
	if len(a.profiles) != 0 || len(a.refills) != 0 {
		t.Fatal("contradicted estimate retained")
	}
}

func TestFingerprintIncludesPresetSettingsNotLabels(t *testing.T) {
	raw := workloadPayload(t, "/a", 720, 20)
	var root map[string]any
	json.Unmarshal(raw, &root)
	makeRaw := func(label string, quality int) []byte {
		p := map[string]any{"PresetName": label, "VideoEncoder": "x265_10bit", "VideoQualitySlider": quality}
		root["TranscoopPreset"] = map[string]any{"Version": 1, "Name": label, "Export": map[string]any{"PresetList": []any{p}}}
		b, _ := json.Marshal(root)
		return b
	}
	a := workloadKey(makeRaw("DVD", 20))
	if a == "" || a != workloadKey(makeRaw("Renamed", 20)) {
		t.Fatal("label changed workload")
	}
	if a == workloadKey(makeRaw("DVD", 22)) {
		t.Fatal("preset quality ignored")
	}
}
