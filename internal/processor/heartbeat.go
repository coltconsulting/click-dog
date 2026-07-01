package processor

import (
	"fmt"
	"time"

	"github.com/coltconsulting/click-dog/internal/clicklog"
	"github.com/coltconsulting/click-dog/internal/metrics"
)

// heartbeat tracks cumulative stats across cycles and emits a periodic summary
// so operators can confirm click-dog is alive without per-cycle log noise.
//
// Counters (cycles/exported/filtered/duplicates/errors) accumulate over the
// emission window and reset after each Info line. Gauges (cb/backoff/uptime)
// are read at emit time from the injected snapshot func — same source /status
// consumes — so log-based monitors can extract structured metrics without
// scraping /metrics or /status.
type Heartbeat struct {
	cycles     int
	exported   int
	filtered   int
	duplicates int
	errors     int
	lastLog    time.Time
	interval   time.Duration // how often to emit the summary

	// snapshot is read at emission time to fold cb/backoff/uptime into the
	// heartbeat line. Nil-tolerant: tests that don't care about gauges pass
	// nil and get "n/a" placeholders without panicking.
	snapshot func() metrics.Snapshot
}

func NewHeartbeat(interval time.Duration, snapshot func() metrics.Snapshot) *Heartbeat {
	return &Heartbeat{
		lastLog:  time.Now(),
		interval: interval,
		snapshot: snapshot,
	}
}

func (h *Heartbeat) Record(exported, filtered, duplicates int, err error) {
	h.cycles++
	h.exported += exported
	h.filtered += filtered
	h.duplicates += duplicates
	if err != nil {
		h.errors++
	}

	if time.Since(h.lastLog) >= h.interval {
		clicklog.Info("%s", h.Format())
		h.cycles = 0
		h.exported = 0
		h.filtered = 0
		h.duplicates = 0
		h.errors = 0
		h.lastLog = time.Now()
	}
}

// format renders the heartbeat line. Split out from record so tests can
// assert on the exact output without wiring up a log capture.
//
// The interval= field is the live polling interval — the same value as
// the click_dog_backoff_interval_seconds gauge. It equals the base
// check_interval when the breaker is closed and the adaptive poller is
// not backed off; it grows when failures push the poller into backoff.
// "interval" rather than "backoff" because the value is reported as an
// absolute duration, not a delta over base.
func (h *Heartbeat) Format() string {
	const placeholder = "n/a"
	cb := placeholder
	interval := placeholder
	uptime := placeholder
	if h.snapshot != nil {
		s := h.snapshot()
		cb = s.CircuitBreakerState
		interval = time.Duration(s.BackoffIntervalSecs * float64(time.Second)).String()
		if !s.UpSince.IsZero() {
			uptime = time.Since(s.UpSince).Round(time.Second).String()
		}
	}
	return fmt.Sprintf("Heartbeat: cycles=%d, exported=%d, filtered=%d, duplicates=%d, errors=%d, cb=%s, interval=%s, uptime=%s (last %v)",
		h.cycles, h.exported, h.filtered, h.duplicates, h.errors, cb, interval, uptime, h.interval)
}
