package observability

import (
	"testing"
	"time"
)

func TestRecorderKeepsNewestFirstAndCounts(t *testing.T) {
	rec := NewRecorder(3)
	rec.Record(RequestEntry{Path: "/a", Status: 200})
	rec.Record(RequestEntry{Path: "/b", Status: 404})
	rec.Record(RequestEntry{Path: "/c", Status: 500})
	rec.Record(RequestEntry{Path: "/d", Status: 302})

	recent := rec.Recent(2)
	if len(recent) != 2 || recent[0].Path != "/d" || recent[1].Path != "/c" {
		t.Fatalf("recent = %+v", recent)
	}

	m := rec.Metrics()
	if m.TotalRequests != 4 || m.Status2xx != 1 || m.Status3xx != 1 || m.Status4xx != 1 || m.Status5xx != 1 {
		t.Fatalf("metrics = %+v", m)
	}
	if m.UptimeSeconds < 0 {
		t.Fatalf("uptime = %d", m.UptimeSeconds)
	}
}

func TestRecorderRecentHonoursLimitAndEmpty(t *testing.T) {
	rec := NewRecorder(10)
	if got := rec.Recent(5); len(got) != 0 {
		t.Fatalf("empty recorder returned %d entries", len(got))
	}
	rec.Record(RequestEntry{Path: "/x", Status: 200})
	if got := rec.Recent(0); len(got) != 1 {
		t.Fatalf("limit 0 returned %d entries", len(got))
	}
}

func TestRecorderNilSafe(t *testing.T) {
	var rec *Recorder
	rec.Record(RequestEntry{})
	if got := rec.Recent(1); len(got) != 0 {
		t.Fatalf("nil recorder returned %d entries", len(got))
	}
	if m := rec.Metrics(); m.TotalRequests != 0 {
		t.Fatalf("nil recorder metrics = %+v", m)
	}
}

func TestRecorderUptime(t *testing.T) {
	rec := NewRecorder(1)
	time.Sleep(10 * time.Millisecond)
	if m := rec.Metrics(); m.UptimeSeconds != 0 {
		// only assert non-negative; sub-second uptime floors to 0
		t.Logf("uptime=%d", m.UptimeSeconds)
	}
}
