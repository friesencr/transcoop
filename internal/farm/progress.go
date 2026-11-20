package farm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sync"
	"time"
)

// stdout and stderr share a synchronized log, but only stdout is parsed.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *lockedWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(b)
}

type progressWriter struct {
	log      io.Writer
	phase    func(string)
	progress func(encodeProgress)
	observed func(JobTelemetry)
	buffer   []byte
	last     string
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.log.Write(b)
	if err != nil {
		return n, err
	}
	// Bound parser memory independently of log size, and resynchronize at each
	// Progress marker even if a previous record was truncated or malformed.
	for len(b) > 0 {
		size := min(len(b), 4096)
		p.buffer = append(p.buffer, b[:size]...)
		b = b[size:]
		p.parse()
	}
	return n, nil
}
func (p *progressWriter) parse() {
	marker := []byte("Progress: ")
	for {
		start := bytes.Index(p.buffer, marker)
		if start < 0 {
			if len(p.buffer) > len(marker) {
				p.buffer = append([]byte(nil), p.buffer[len(p.buffer)-len(marker):]...)
			}
			return
		}
		p.buffer = p.buffer[start:]
		body := p.buffer[len(marker):]
		next := bytes.Index(body, marker)
		candidate := body
		if next >= 0 {
			candidate = body[:next]
		}
		var v struct {
			State   string
			Working struct {
				PassID          *int
				Progress        *float64
				Rate, RateAvg   float64
				ETASeconds      *int
				Pass, PassCount int
			}
		}
		d := json.NewDecoder(bytes.NewReader(candidate))
		err := d.Decode(&v)
		if err == nil {
			phase := "unknown"
			switch v.State {
			case "SCANNING", "SCANDONE", "SEARCHING", "IDLE":
				phase = "preparation"
			case "WORKING":
				if v.Working.PassID != nil {
					switch *v.Working.PassID {
					case -1:
						phase = "subtitle search"
					case 0, 2:
						phase = "video encoding"
					case 1:
						phase = "video analysis"
					}
				}
			case "MUXING":
				phase = "muxing"
			case "WORKDONE":
				phase = "awaiting process exit"
			case "PAUSED":
				phase = "paused"
			}
			if phase != p.last {
				p.last = phase
				if p.phase != nil {
					p.phase(phase)
				}
			}
			if p.observed != nil {
				t := JobTelemetry{Phase: phase}
				if v.State == "WORKING" {
					t.Pass = max(0, v.Working.Pass)
					t.PassCount = max(0, v.Working.PassCount)
					if v.Working.Progress != nil && *v.Working.Progress >= 0 && *v.Working.Progress <= 1 {
						t.Fraction = v.Working.Progress
					}
					if v.Working.Rate > 0 {
						t.FPS = &v.Working.Rate
					}
					if v.Working.RateAvg > 0 {
						t.AverageFPS = &v.Working.RateAvg
					}
					if v.Working.ETASeconds != nil && *v.Working.ETASeconds >= 0 {
						t.ETA = v.Working.ETASeconds
					}
				}
				p.observed(t)
			}
			if v.State == "WORKING" && p.progress != nil && v.Working.Progress != nil && *v.Working.Progress >= 0 && *v.Working.Progress <= 1 {
				p.progress(encodeProgress{phase, *v.Working.Progress, v.Working.Rate, v.Working.RateAvg, v.Working.ETASeconds, v.Working.Pass, v.Working.PassCount})
			}
			p.buffer = p.buffer[len(marker)+int(d.InputOffset()):]
			continue
		}
		if next >= 0 {
			p.buffer = body[next:]
			continue
		}
		if len(p.buffer) > 64*1024 {
			p.buffer = append([]byte(nil), p.buffer[len(p.buffer)-len(marker):]...)
		}
		return
	}
}

type encodeProgress struct {
	phase                   string
	fraction, rate, average float64
	etaSeconds              *int
	pass, passCount         int
}

// HandBrake emits several updates per second; keep service logs readable while
// retaining every original record in handbrake.log.
type jobProgressLogger struct {
	id       int64
	now      func() time.Time
	lastTime time.Time
	last     encodeProgress
}

func (l *jobProgressLogger) report(p encodeProgress) {
	now := l.now()
	changed := p.phase != l.last.phase || p.pass != l.last.pass
	finished := p.fraction == 1 && l.last.fraction != 1
	if !l.lastTime.IsZero() && !changed && !finished && now.Sub(l.lastTime) < 10*time.Second {
		return
	}
	message := fmt.Sprintf("job %d: %s %.1f%%", l.id, p.phase, p.fraction*100)
	if p.passCount > 1 {
		message += fmt.Sprintf(" pass %d/%d", p.pass, p.passCount)
	}
	if p.rate > 0 {
		message += fmt.Sprintf(" | %.1f fps", p.rate)
	}
	if p.average > 0 {
		message += fmt.Sprintf(" (avg %.1f)", p.average)
	}
	if p.etaSeconds != nil && *p.etaSeconds >= 0 {
		message += fmt.Sprintf(" | ETA %s", time.Duration(*p.etaSeconds)*time.Second)
	}
	log.Print(message)
	l.lastTime, l.last = now, p
}
