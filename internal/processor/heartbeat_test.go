package processor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/metrics"
)

func TestHeartbeat_FormatIncludesAllFields(t *testing.T) {
	// UpSince is offset by exactly 2h so that time.Since(UpSince).Round(time.Second)
	// renders deterministically as "2h0m0s" — the only way the rounded value
	// could land elsewhere is a unit-test scheduling stall of 500ms+ between
	// the snap closure firing and format() returning, which doesn't happen
	// in practice. Keeps the assertion exact instead of substring-prefix matching.
	upSince := time.Now().Add(-2 * time.Hour)
	snap := func() metrics.Snapshot {
		return metrics.Snapshot{
			CircuitBreakerState: "half_open",
			BackoffIntervalSecs: 30,
			UpSince:             upSince,
		}
	}
	hb := NewHeartbeat(5*time.Minute, snap)
	hb.cycles = 12
	hb.exported = 340
	hb.filtered = 7
	hb.duplicates = 4
	hb.errors = 1

	got := hb.Format()

	for _, want := range []string{
		"cycles=12",
		"exported=340",
		"filtered=7",
		"duplicates=4",
		"errors=1",
		"cb=half_open",
		"interval=30s",
		"uptime=2h0m0s",
		"(last 5m0s)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("format output missing %q\nfull line: %s", want, got)
		}
	}
}

func TestHeartbeat_FormatNilSnapshotIsSafe(t *testing.T) {
	hb := NewHeartbeat(time.Minute, nil)
	hb.cycles = 1
	got := hb.Format()
	for _, want := range []string{"cb=n/a", "interval=n/a", "uptime=n/a"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q placeholder when snapshot is nil, got: %s", want, got)
		}
	}
}

func TestHeartbeat_RecordResetsCountersOnEmission(t *testing.T) {
	snap := func() metrics.Snapshot {
		return metrics.Snapshot{
			CircuitBreakerState: "closed",
			BackoffIntervalSecs: 10,
			UpSince:             time.Now().Add(-10 * time.Second),
		}
	}
	// interval=0 forces an emission on every record call.
	hb := NewHeartbeat(0, snap)
	preLog := hb.lastLog

	hb.Record(5, 1, 2, nil)

	if hb.cycles != 0 || hb.exported != 0 || hb.filtered != 0 || hb.duplicates != 0 || hb.errors != 0 {
		t.Errorf("counters not reset after emission: %+v", hb)
	}
	if !hb.lastLog.After(preLog) {
		t.Errorf("lastLog should advance after emission; pre=%v post=%v", preLog, hb.lastLog)
	}
}

func TestHeartbeat_RecordAccumulatesUntilInterval(t *testing.T) {
	hb := NewHeartbeat(time.Hour, nil) // interval well in the future
	preLog := hb.lastLog

	hb.Record(2, 0, 1, nil)
	hb.Record(3, 1, 0, errors.New("boom"))
	hb.Record(0, 0, 0, nil)

	if !hb.lastLog.Equal(preLog) {
		t.Errorf("expected no emission (lastLog unchanged) before interval elapsed; pre=%v post=%v", preLog, hb.lastLog)
	}
	if hb.cycles != 3 {
		t.Errorf("cycles: want 3, got %d", hb.cycles)
	}
	if hb.exported != 5 {
		t.Errorf("exported: want 5, got %d", hb.exported)
	}
	if hb.filtered != 1 {
		t.Errorf("filtered: want 1, got %d", hb.filtered)
	}
	if hb.duplicates != 1 {
		t.Errorf("duplicates: want 1, got %d", hb.duplicates)
	}
	if hb.errors != 1 {
		t.Errorf("errors: want 1, got %d", hb.errors)
	}
}

func TestHeartbeat_FormatIntervalRendersAsDuration(t *testing.T) {
	snap := func() metrics.Snapshot {
		return metrics.Snapshot{
			CircuitBreakerState: "open",
			BackoffIntervalSecs: 7.5, // fractional seconds — confirms float→Duration path
			UpSince:             time.Now(),
		}
	}
	hb := NewHeartbeat(time.Minute, snap)
	got := hb.Format()
	if !strings.Contains(got, "interval=7.5s") {
		t.Errorf("expected interval=7.5s, got: %s", got)
	}
	if !strings.Contains(got, "cb=open") {
		t.Errorf("expected cb=open, got: %s", got)
	}
}
