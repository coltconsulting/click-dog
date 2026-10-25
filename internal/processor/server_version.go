package processor

import (
	"context"
	"time"

	"github.com/coltconsulting/click-dog/internal/clicklog"
	"github.com/coltconsulting/click-dog/internal/model"
)

// serverVersionRefreshInterval bounds how often the ClickHouse server version
// is re-read. The value changes on the order of twice a year, so polling it
// every cycle would add a round trip at poll frequency — and a system.query_log
// row on the very server being monitored — to learn nothing. Five minutes is
// far below any useful dashboard resolution while costing almost nothing.
//
// Detected degradation clears the stamp and forces an immediate refresh on
// recovery. An upgrade that completes between polls without a detected failure
// remains subject to this interval.
const serverVersionRefreshInterval = 5 * time.Minute

// serverVersionStamp keeps an exporter's clickhouse.version attribute in step
// with the server actually being read.
//
// Healthy cycles reuse the last successful lookup within the refresh interval.
// A failed lookup or detected degraded cycle clears that cached value.
type serverVersionStamp struct {
	// last is the version confirmed by the most recent successful read, or
	// empty when the stamp is cleared or unresolved. Kept to log transitions.
	last string

	// attemptedAt throttles reads. Zero means never attempted, so the first
	// call always reads.
	attemptedAt time.Time

	// force skips the throttle once. Clear sets it so recovery from a degraded
	// cycle re-reads immediately rather than waiting out the interval.
	force bool

	// failureLogged keeps a permanently failing read from logging every cycle.
	failureLogged bool
}

// Refresh re-reads the server version and stamps it on the exporter, subject to
// the refresh interval.
//
// reader and exporter are taken as any because both sides are optional
// companions discovered by type assertion: a reader that cannot report a
// version, or an exporter with no envelope to stamp (Splunk HEC), simply opts
// out. The exporter is checked first so the reader is never queried when
// nothing can use the result.
func (s *serverVersionStamp) Refresh(ctx context.Context, reader, exporter any, now time.Time) {
	stamper, ok := exporter.(model.ServerVersionStamper)
	if !ok {
		return
	}
	versionReader, ok := reader.(model.ServerVersionReader)
	if !ok {
		return
	}
	if !s.force && !s.attemptedAt.IsZero() && now.Sub(s.attemptedAt) < serverVersionRefreshInterval {
		return
	}
	s.attemptedAt = now
	s.force = false

	version, err := versionReader.ServerVersion(ctx)
	if err != nil {
		if s.last != "" {
			clicklog.Warn("ClickHouse server version unavailable, clearing exported clickhouse.version: %v", err)
		} else if !s.failureLogged {
			clicklog.Debug("ClickHouse server version unavailable: %v", err)
		}
		s.failureLogged = true
		s.last = ""
		stamper.SetServerVersion("")
		return
	}
	s.failureLogged = false

	if version != s.last {
		if s.last == "" {
			clicklog.Info("ClickHouse server version: %s", version)
		} else {
			clicklog.Info("ClickHouse server version changed: %s -> %s", s.last, version)
		}
		s.last = version
	}
	stamper.SetServerVersion(version)
}

// Clear drops the stamp for a cycle that could not confirm the version, and
// forces the next Refresh to re-read rather than wait out the interval.
//
// Degraded cycles still export — the canary span goes out while the breaker is
// open — and a ClickHouse upgrade is the most likely cause of the outage that
// opened it. Without this, those spans would carry the pre-upgrade version for
// the whole degraded window.
func (s *serverVersionStamp) Clear(exporter any) {
	s.force = true
	stamper, ok := exporter.(model.ServerVersionStamper)
	if !ok {
		return
	}
	if s.last != "" {
		clicklog.Debug("ClickHouse unreachable this cycle, clearing exported clickhouse.version (was %s)", s.last)
	}
	s.last = ""
	stamper.SetServerVersion("")
}

// StampServerVersionOnce resolves the server version and stamps it on the
// exporter a single time. Backfill is a one-shot historical export with no
// cycle to refresh on, so the attribute describes the server observed at the
// start of the run, not the version that originally produced historical rows.
func StampServerVersionOnce(ctx context.Context, reader, exporter any) {
	stamp := &serverVersionStamp{}
	stamp.Refresh(ctx, reader, exporter, time.Now())
}
