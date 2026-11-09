package observability

import (
	"sync"
	"time"
)

// RequestEntry is one recorded control-plane request.
type RequestEntry struct {
	Time       string `json:"time"`
	RequestID  string `json:"request_id"`
	Method     string `json:"method"`
	Host       string `json:"host"`
	Path       string `json:"path"`
	Status     int    `json:"status"`
	Bytes      int64  `json:"bytes"`
	DurationMS int64  `json:"duration_ms"`
}

// Metrics summarises control-plane traffic since start.
type Metrics struct {
	UptimeSeconds int64 `json:"uptime_seconds"`
	TotalRequests int64 `json:"total_requests"`
	Status2xx     int64 `json:"status_2xx"`
	Status3xx     int64 `json:"status_3xx"`
	Status4xx     int64 `json:"status_4xx"`
	Status5xx     int64 `json:"status_5xx"`
}

// Recorder keeps a bounded ring of recent requests plus aggregate counters.
// The zero value is not usable; construct with NewRecorder.
type Recorder struct {
	mu      sync.RWMutex
	started time.Time
	entries []RequestEntry
	next    int
	full    bool
	total   int64
	s2, s3  int64
	s4, s5  int64
}

// NewRecorder returns a recorder retaining at most capacity recent entries.
func NewRecorder(capacity int) *Recorder {
	if capacity <= 0 {
		capacity = 200
	}
	return &Recorder{
		started: time.Now(),
		entries: make([]RequestEntry, capacity),
	}
}

// Record appends one entry and updates counters.
func (r *Recorder) Record(e RequestEntry) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[r.next] = e
	r.next = (r.next + 1) % len(r.entries)
	if r.next == 0 {
		r.full = true
	}
	r.total++
	switch {
	case e.Status >= 500:
		r.s5++
	case e.Status >= 400:
		r.s4++
	case e.Status >= 300:
		r.s3++
	case e.Status >= 200:
		r.s2++
	}
}

// Recent returns up to limit entries, newest first.
func (r *Recorder) Recent(limit int) []RequestEntry {
	if r == nil {
		return []RequestEntry{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if limit <= 0 || limit > len(r.entries) {
		limit = len(r.entries)
	}
	out := make([]RequestEntry, 0, limit)
	for i := 1; i <= limit; i++ {
		idx := (r.next - i + len(r.entries)) % len(r.entries)
		if !r.full && idx >= r.next {
			break
		}
		out = append(out, r.entries[idx])
	}
	return out
}

// Metrics returns aggregate counters.
func (r *Recorder) Metrics() Metrics {
	if r == nil {
		return Metrics{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return Metrics{
		UptimeSeconds: int64(time.Since(r.started).Seconds()),
		TotalRequests: r.total,
		Status2xx:     r.s2,
		Status3xx:     r.s3,
		Status4xx:     r.s4,
		Status5xx:     r.s5,
	}
}
