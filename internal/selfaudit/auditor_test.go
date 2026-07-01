package selfaudit

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/clicklog"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/metrics"
)

// fakeClock is a settable clock for the WARN throttle.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

// --- fakes -----------------------------------------------------------------

type fakeReader struct {
	count int
	err   error

	// noRows simulates an empty query_log for the blindness sanity probe; the
	// zero value means "rows exist" so tests that aren't about blindness stay
	// silent. rowsErr fails the probe; sanityCalls counts its invocations.
	noRows      bool
	rowsErr     error
	sanityCalls int
}

func (f *fakeReader) CountDistinctClusterReaders(_ context.Context, _, _ time.Duration) (int, error) {
	return f.count, f.err
}

func (f *fakeReader) HasRecentQueryLogRows(_ context.Context, _ time.Duration) (bool, error) {
	f.sanityCalls++
	if f.rowsErr != nil {
		return false, f.rowsErr
	}
	return !f.noRows, nil
}

type fakeSink struct {
	state map[string]bool
}

func newFakeSink() *fakeSink { return &fakeSink{state: map[string]bool{}} }

func (s *fakeSink) SetTopologyWarning(reason string, on bool) { s.state[reason] = on }

const (
	loopbackHost    = "127.0.0.1"
	nonLoopbackHost = "ch-prod-0.internal"
)

func newAuditor(host string, debounce int, reader clusterReaderProber) (*Auditor, *fakeSink) {
	sink := newFakeSink()
	a := New(Options{
		Host:          host,
		Interval:      time.Hour, // Run() is not used; tests drive evaluate() directly.
		Debounce:      debounce,
		Lookback:      15 * time.Minute,
		CheckInterval: 30 * time.Second,
		Reader:        reader,
		Sink:          sink,
	})
	return a, sink
}

// tick runs n evaluate() passes.
func tick(a *Auditor, n int) {
	for i := 0; i < n; i++ {
		a.evaluate(context.Background())
	}
}

// --- the count triggers + debounce -----------------------------------------

func TestAuditor_QueryLogTrigger_FlipsAfterDebounce(t *testing.T) {
	a, sink := newAuditor(loopbackHost, 2, &fakeReader{count: 2})

	a.evaluate(context.Background())
	if sink.state[metrics.TopologyReasonSidecar] {
		t.Fatal("flipped on the first positive tick; debounce_count=2 should require two")
	}
	a.evaluate(context.Background())
	if !sink.state[metrics.TopologyReasonSidecar] {
		t.Fatal("did not flip after debounce_count positive ticks")
	}
	// Loopback host ⇒ sidecar reason; the multi reason must stay clear.
	if sink.state[metrics.TopologyReasonMultiInstance] {
		t.Error("multi_instance reason set on a loopback host (should be sidecar only)")
	}
}

func TestAuditor_ReasonFromHost(t *testing.T) {
	cases := []struct {
		host string
		want string
	}{
		{"127.0.0.1", metrics.TopologyReasonSidecar},
		{"localhost", metrics.TopologyReasonSidecar},
		{"::1", metrics.TopologyReasonSidecar},
		{nonLoopbackHost, metrics.TopologyReasonMultiInstance},
		{"10.0.0.5", metrics.TopologyReasonMultiInstance},
		{"", metrics.TopologyReasonMultiInstance},
	}
	for _, tc := range cases {
		a, sink := newAuditor(tc.host, 1, &fakeReader{count: 2})
		a.evaluate(context.Background())
		if !sink.state[tc.want] {
			t.Errorf("host %q: reason %q not set; gauge=%v", tc.host, tc.want, sink.state)
		}
	}
}

// --- lone instance / loopback-only never trips -----------------------------

func TestAuditor_LoneInstanceNeverTrips(t *testing.T) {
	// count==1, even on loopback: the harmless n=1 case.
	a, sink := newAuditor(loopbackHost, 1, &fakeReader{count: 1})
	tick(a, 5)
	if sink.state[metrics.TopologyReasonSidecar] || sink.state[metrics.TopologyReasonMultiInstance] {
		t.Errorf("a lone instance tripped: %v", sink.state)
	}
}

func TestAuditor_LoopbackSmellAloneDoesNotTrip(t *testing.T) {
	// Loopback host but only one reader: the config smell is a reason
	// discriminator, never a standalone trigger.
	a, sink := newAuditor(loopbackHost, 1, &fakeReader{count: 1})
	tick(a, 3)
	if sink.state[metrics.TopologyReasonSidecar] {
		t.Error("loopback config smell tripped without a count trigger")
	}
}

// --- clear-on-clean-tick ----------------------------------------------------

func TestAuditor_ClearsImmediatelyOnCleanTick(t *testing.T) {
	reader := &fakeReader{count: 2}
	a, sink := newAuditor(loopbackHost, 1, reader)

	a.evaluate(context.Background())
	if !sink.state[metrics.TopologyReasonSidecar] {
		t.Fatal("did not trip")
	}
	// Topology corrected: a single clean tick clears immediately (no debounce).
	reader.count = 1
	a.evaluate(context.Background())
	if sink.state[metrics.TopologyReasonSidecar] {
		t.Error("did not clear on the first clean tick")
	}
	if a.counters[metrics.TopologyReasonSidecar] != 0 {
		t.Errorf("counter not reset on clear: %d", a.counters[metrics.TopologyReasonSidecar])
	}
}

// --- fail-open on probe error ----------------------------------------------

func TestAuditor_ProbeError_IsFullNoOp(t *testing.T) {
	reader := &fakeReader{err: errors.New("clickhouse down")}
	a, sink := newAuditor(loopbackHost, 1, reader)

	a.evaluate(context.Background())
	// Gauge untouched (no write at all this tick) and counter not advanced.
	if _, written := sink.state[metrics.TopologyReasonSidecar]; written {
		t.Error("gauge was written on an errored tick (should be a full no-op)")
	}
	if a.counters[metrics.TopologyReasonSidecar] != 0 {
		t.Errorf("counter advanced on an errored tick: %d", a.counters[metrics.TopologyReasonSidecar])
	}
}

func TestAuditor_ErroredTickDoesNotAdvanceDebounce(t *testing.T) {
	reader := &fakeReader{count: 2}
	a, sink := newAuditor(loopbackHost, 2, reader)

	a.evaluate(context.Background())     // counter 1
	reader.err = errors.New("transient") // no-op tick
	a.evaluate(context.Background())
	if a.counters[metrics.TopologyReasonSidecar] != 1 {
		t.Fatalf("errored tick changed the counter: %d, want 1", a.counters[metrics.TopologyReasonSidecar])
	}
	if sink.state[metrics.TopologyReasonSidecar] {
		t.Fatal("an errored tick must not push the counter to the threshold")
	}
	reader.err = nil
	a.evaluate(context.Background()) // counter 2 → detected
	if !sink.state[metrics.TopologyReasonSidecar] {
		t.Error("did not flip after two genuine positive ticks straddling an errored tick")
	}
}

func TestAuditor_ErroredTickDoesNotClearDetection(t *testing.T) {
	reader := &fakeReader{count: 2}
	a, sink := newAuditor(loopbackHost, 1, reader)

	a.evaluate(context.Background())
	if !sink.state[metrics.TopologyReasonSidecar] {
		t.Fatal("did not trip")
	}
	// The P1 contract: a query_log scrape outage must NOT clear a real detection
	// — an errored observation is not a clean tick, so the warning survives a
	// transient probe failure (fail-open).
	reader.err = errors.New("scrape timeout")
	a.evaluate(context.Background())
	if !sink.state[metrics.TopologyReasonSidecar] {
		t.Error("an errored tick cleared a real detection (must be fail-open)")
	}
	if !a.detected[metrics.TopologyReasonSidecar] {
		t.Error("detection state lost on an errored tick")
	}
}

// --- active window derivation: the temporal false-positive fix (issue #223) -

// The active window replaced the warm-up gate: startup-era and post-failover
// departed readers are filtered out at the query (HAVING max(event_time)) rather
// than by suppressing the flip for one lookback. activeWindowFor must keep the
// window in (checkInterval, interval): above the poll cadence so a genuine
// concurrent reader is still caught, well below the audit interval so a host that
// read once can't survive into two consecutive ticks and trip the debounce.
func TestActiveWindowFor(t *testing.T) {
	const minTickGapFraction = 1 - jitterFraction // consecutive ticks are ≥ interval×this apart
	cases := []struct {
		name     string
		check    time.Duration
		interval time.Duration
		want     time.Duration
	}{
		{"defaults: 3×30s poll, 5m audit", 30 * time.Second, 5 * time.Minute, 90 * time.Second},
		{"tiny poll clamps up to the 1m floor", 5 * time.Second, 5 * time.Minute, time.Minute},
		{"slow poll clamps down to interval/2", 4 * time.Minute, 5 * time.Minute, 150 * time.Second},
		{"zero check falls back to the floor", 0, 5 * time.Minute, time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := activeWindowFor(tc.check, tc.interval)
			if got != tc.want {
				t.Fatalf("activeWindowFor(%v, %v) = %v, want %v", tc.check, tc.interval, got, tc.want)
			}
			// No-false-positive invariant: a departed reader (fixed last read) must
			// age out before two consecutive ticks could both count it.
			if minGap := time.Duration(float64(tc.interval) * minTickGapFraction); got >= minGap {
				t.Errorf("active window %v ≥ min tick gap %v — a departed reader could span two ticks and trip the debounce", got, minGap)
			}
		})
	}
}

// --- the no-op gate (use_cluster_queries off) ------------------------------

func TestShouldAudit(t *testing.T) {
	mk := func(clusterQueries, enabled bool) *config.Config {
		c := &config.Config{}
		c.ClickHouse.UseClusterQueries = clusterQueries
		c.Monitor.TopologyAudit.Enabled = enabled
		return c
	}
	cases := []struct {
		name           string
		clusterQueries bool
		enabled        bool
		want           bool
	}{
		{"cluster off, enabled → no-op", false, true, false},
		{"cluster on, enabled → run", true, true, true},
		{"cluster on, disabled → no-op", true, false, false},
		{"cluster off, disabled → no-op", false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShouldAudit(mk(tc.clusterQueries, tc.enabled)); got != tc.want {
				t.Errorf("ShouldAudit = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAuditor_PersistWarnIsThrottled exercises the auditor-level WARN path that
// the throttle unit tests cover only in isolation: a detection logs once on
// transition, persisting ticks within persistWarnInterval are throttled (no new
// WARN), and a tick past the window logs again. clicklog routes WARN through the
// stdlib logger, so we capture log output and count the two message shapes.
// --- query_log blindness warning (#238) ------------------------------------

// captureWarnLog redirects clicklog output to a buffer at WARN level and
// returns a counter of emitted lines containing substr. It mutates
// process-global logger state (clicklog.InitLogger, log.SetOutput), so tests
// using it must not call t.Parallel().
func captureWarnLog(t *testing.T, substr string) func() int {
	t.Helper()
	if err := clicklog.InitLogger("warn", "", "text", 0, 0); err != nil {
		t.Fatalf("InitLogger: %v", err)
	}
	t.Cleanup(clicklog.CloseLogger)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return func() int {
		n := 0
		for _, line := range strings.Split(buf.String(), "\n") {
			if strings.Contains(line, substr) {
				n++
			}
		}
		return n
	}
}

// TestAuditor_QueryLogBlindness_WarnsOnceWhenEmpty pins the logged-once caveat:
// a successful zero-reader scan over an empty query_log is the detector's blind
// spot, so the first such tick WARNs exactly once and the sanity probe is never
// re-run.
func TestAuditor_QueryLogBlindness_WarnsOnceWhenEmpty(t *testing.T) {
	warns := captureWarnLog(t, "query logging appears disabled")
	reader := &fakeReader{count: 0, noRows: true}
	a, _ := newAuditor(loopbackHost, 2, reader)

	tick(a, 3)
	if got := warns(); got != 1 {
		t.Errorf("blindness WARN count = %d, want exactly 1", got)
	}
	if reader.sanityCalls != 1 {
		t.Errorf("sanity probe ran %d times, want 1 (latched after first success)", reader.sanityCalls)
	}
}

// TestAuditor_QueryLogBlindness_SilentWhenRowsExist: a zero-reader scan with a
// populated query_log is the genuine all-clear — checked once, no WARN.
func TestAuditor_QueryLogBlindness_SilentWhenRowsExist(t *testing.T) {
	warns := captureWarnLog(t, "query logging appears disabled")
	reader := &fakeReader{count: 0}
	a, _ := newAuditor(loopbackHost, 2, reader)

	tick(a, 2)
	if got := warns(); got != 0 {
		t.Errorf("blindness WARN count = %d, want 0 when query_log has rows", got)
	}
	if reader.sanityCalls != 1 {
		t.Errorf("sanity probe ran %d times, want 1", reader.sanityCalls)
	}
}

// TestAuditor_QueryLogBlindness_SkippedWhenReadersSeen: a non-zero count proves
// query_log is populated, so the extra probe must not run at all (zero cost on
// a healthy fleet).
func TestAuditor_QueryLogBlindness_SkippedWhenReadersSeen(t *testing.T) {
	reader := &fakeReader{count: 2}
	a, _ := newAuditor(loopbackHost, 2, reader)

	tick(a, 2)
	if reader.sanityCalls != 0 {
		t.Errorf("sanity probe ran %d times, want 0 when the scan sees readers", reader.sanityCalls)
	}
}

// TestAuditor_QueryLogBlindness_ProbeErrorRetries: an errored sanity probe must
// not consume the once-per-process latch — the check retries on the next
// zero-count tick and still WARNs.
func TestAuditor_QueryLogBlindness_ProbeErrorRetries(t *testing.T) {
	warns := captureWarnLog(t, "query logging appears disabled")
	reader := &fakeReader{count: 0, rowsErr: errors.New("timeout")}
	a, _ := newAuditor(loopbackHost, 2, reader)

	tick(a, 1)
	if got := warns(); got != 0 {
		t.Fatalf("errored sanity probe must not WARN, got %d", got)
	}

	reader.rowsErr = nil
	reader.noRows = true
	tick(a, 1)
	if got := warns(); got != 1 {
		t.Errorf("blindness WARN count after retry = %d, want 1", got)
	}
	if reader.sanityCalls != 2 {
		t.Errorf("sanity probe ran %d times, want 2 (one failure, one retry)", reader.sanityCalls)
	}
}

func TestAuditor_PersistWarnIsThrottled(t *testing.T) {
	if err := clicklog.InitLogger("warn", "", "text", 0, 0); err != nil {
		t.Fatalf("InitLogger: %v", err)
	}
	t.Cleanup(clicklog.CloseLogger)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	a := New(Options{
		Host:     nonLoopbackHost,
		Interval: time.Hour,
		Debounce: 1,
		Lookback: 15 * time.Minute,
		Reader:   &fakeReader{count: 3},
		Sink:     newFakeSink(),
		now:      clk.now,
	})

	counts := func() (detected, persist int) {
		for _, line := range strings.Split(buf.String(), "\n") {
			if strings.Contains(line, "DETECTED") {
				detected++
			}
			if strings.Contains(line, "still detected") {
				persist++
			}
		}
		return
	}

	a.evaluate(context.Background()) // transition → DETECTED
	if d, p := counts(); d != 1 || p != 0 {
		t.Fatalf("on detection: detected=%d persist=%d, want 1/0", d, p)
	}

	a.evaluate(context.Background()) // within window → throttled, no new WARN
	if d, p := counts(); d != 1 || p != 0 {
		t.Fatalf("persist within window must be throttled: detected=%d persist=%d, want 1/0", d, p)
	}

	clk.t = clk.t.Add(persistWarnInterval + time.Second) // past the window
	a.evaluate(context.Background())
	if d, p := counts(); d != 1 || p != 1 {
		t.Fatalf("persist past window must WARN again: detected=%d persist=%d, want 1/1", d, p)
	}
}
