package farm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"math"
	"time"
)

// ClaimCandidate describes the oldest compatible job without granting a lease.
// Digest fences the subsequent claim even if clear/import reuses a job ID.
type ClaimCandidate struct {
	ID       int64  `json:"id"`
	Digest   string `json:"digest"`
	Workload string `json:"workload"`
}

func candidateFor(j Job) ClaimCandidate {
	hash := sha256.Sum256(j.Payload)
	return ClaimCandidate{ID: j.ID, Digest: hex.EncodeToString(hash[:]), Workload: workloadKey(j.Payload)}
}

// Fingerprint actual encoding settings, not a user-editable preset label. Exact
// source geometry and frame rate separate DVD, HD and other source workloads.
// Missing metadata deliberately leaves the job on the conservative path.
func workloadKey(raw json.RawMessage) string {
	var root map[string]any
	if json.Unmarshal(raw, &root) != nil {
		return ""
	}
	title, _ := root["Title"].(map[string]any)
	geometry, _ := title["Geometry"].(map[string]any)
	rate, _ := title["FrameRate"].(map[string]any)
	positive := func(m map[string]any, k string) bool { v, ok := m[k].(float64); return ok && v > 0 }
	if !positive(geometry, "Width") || !positive(geometry, "Height") || !positive(rate, "Num") || !positive(rate, "Den") {
		return ""
	}
	job, _ := root["Job"].(map[string]any)
	if job == nil {
		return ""
	}
	video, _ := job["Video"].(map[string]any)
	if encoder, ok := video["Encoder"].(string); !ok || encoder == "" {
		return ""
	}
	delete(job, "Source")
	delete(job, "Metadata")
	delete(job, "SequenceID")
	if dest, ok := job["Destination"].(map[string]any); ok {
		delete(dest, "File")
		delete(dest, "ChapterList") // Per-source chapter labels do not change encode demand.
	}
	snapshot, preset, err := ReadPresetSnapshot(raw)
	if err != nil {
		return ""
	}
	if snapshot != nil {
		delete(preset, "PresetName")
		delete(preset, "PresetDescription")
	}
	data, err := json.Marshal([]any{job, preset, geometry, rate, title["VideoCodec"], title["InterlaceDetected"]})
	if err != nil {
		return ""
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

const profileLifetime = 30 * time.Minute

type workloadProfile struct {
	successes int
	demand    float64 // Percent of total host CPU, including all HandBrake threads.
	settle    int     // Seconds from video encoding to a sustained CPU plateau.
	updated   time.Time
}

func (p workloadProfile) trusted(now time.Time) bool {
	return p.successes >= 2 && p.demand >= 3 && now.Sub(p.updated) < profileLifetime
}

// An observation belongs to a single HandBrake process. Only CPU measurements
// made with host headroom train demand; saturated measurements understate it.
type workloadObservation struct {
	started       time.Time
	window        []float64
	samples       int
	peak, plateau float64
	settle        int
	maxSettle     int
	invalid       bool
}

func (o *workloadObservation) sample(now time.Time, value, host, target float64) {
	if o.invalid || o.started.IsZero() {
		return
	}
	if host >= target-5 {
		o.window = nil
		return
	}
	o.samples++
	o.window = append(o.window, value)
	if len(o.window) > 10 {
		o.window = o.window[1:]
	}
	if len(o.window) < 10 {
		return
	}
	lo, hi, sum := 100.0, 0.0, 0.0
	for _, v := range o.window {
		lo = math.Min(lo, v)
		hi = math.Max(hi, v)
		sum += v
	}
	mean := sum / 10
	o.peak = math.Max(o.peak, hi)
	// A late CPU rise invalidates the earlier settling estimate. Require a new
	// plateau, and retain the later settling time rather than speeding up.
	if o.plateau > 0 && mean > o.plateau*1.25+3 {
		o.settle = 0
		o.plateau = 0
	}
	if o.settle == 0 && hi-lo <= math.Max(3, mean*.2) {
		o.settle = max(10, int(math.Ceil(now.Sub(o.started).Seconds())))
		o.plateau = mean
		o.maxSettle = max(o.maxSettle, o.settle)
	}
}
func (o *workloadObservation) usable() bool {
	return !o.invalid && o.samples >= 20 && o.settle > 0 && o.peak >= 3
}

func (a *admissionState) sampleProcesses(ctx context.Context, now time.Time, d admissionDeps) {
	a.metricsFailed = d.processCPU == nil || d.cpuCount < 1 || !a.cpu.valid
	if a.metricsFailed {
		return
	}
	for _, r := range a.active {
		if r.workload == "" || r.phase != "video encoding" || r.pid == 0 {
			continue
		}
		total, err := d.processCPU(ctx, r.pid)
		if err != nil || math.IsNaN(total) || math.IsInf(total, 0) {
			a.metricsFailed = true
			r.observation.invalid = true
			continue
		}
		elapsed := now.Sub(r.previousAt).Seconds()
		previous := r.previousCPU
		first := r.previousAt.IsZero()
		r.previousCPU = total
		r.previousAt = now
		if first {
			continue
		}
		if elapsed < .5 || elapsed > 1.5 || total < previous {
			r.observation.invalid = true
			a.metricsFailed = true
			continue
		}
		value := (total - previous) / elapsed * 100 / float64(d.cpuCount)
		if value > 100.5 {
			r.observation.invalid = true
			a.metricsFailed = true
			continue
		}
		value = math.Min(100, value)
		if p := a.profiles[r.workload]; p.trusted(now) && value > p.demand*1.25+5 {
			// New evidence contradicts the estimate: earn confidence again.
			delete(a.profiles, r.workload)
			delete(a.refills, r.workload)
		}
		r.observation.sample(now, value, a.latestCPU(), a.target)
	}
}

func (a *admissionState) learnCompletion(r *attemptReservation) {
	if r.workload == "" || !r.succeeded || !r.observation.usable() {
		return
	}
	now := a.cpu.last
	if a.profiles == nil {
		a.profiles = map[string]workloadProfile{}
	}
	if a.refills == nil {
		a.refills = map[string][]time.Time{}
	}
	for key, p := range a.profiles {
		if now.Sub(p.updated) >= profileLifetime {
			delete(a.profiles, key)
			delete(a.refills, key)
		}
	}
	if len(a.profiles) >= 128 {
		oldest := ""
		var at time.Time
		for key, p := range a.profiles {
			if oldest == "" || p.updated.Before(at) {
				oldest = key
				at = p.updated
			}
		}
		delete(a.profiles, oldest)
		delete(a.refills, oldest)
	}
	p := a.profiles[r.workload]
	p.successes = min(2, p.successes+1)
	p.demand = math.Max(p.demand, r.observation.peak)
	p.settle = max(p.settle, r.observation.settle, r.observation.maxSettle)
	p.updated = now
	a.profiles[r.workload] = p
	log.Printf("auto workload %.12s: %d/2 successful observations, peak CPU %.1f%%, settling %ds", r.workload, p.successes, p.demand, p.settle)
	alive := []time.Time{}
	for _, until := range a.refills[r.workload] {
		if now.Before(until) {
			alive = append(alive, until)
		}
	}
	alive = append(alive, now.Add(time.Minute))
	if len(alive) > a.cap {
		alive = alive[len(alive)-a.cap:]
	}
	a.refills[r.workload] = alive
}

func (a *admissionState) consumeRefill(key string, now time.Time) {
	alive := []time.Time{}
	consumed := false
	for _, until := range a.refills[key] {
		if !now.Before(until) {
			continue
		}
		if !consumed {
			consumed = true
			continue
		}
		alive = append(alive, until)
	}
	if len(alive) == 0 {
		delete(a.refills, key)
	} else {
		a.refills[key] = alive
	}
}
