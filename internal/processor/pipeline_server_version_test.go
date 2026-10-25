package processor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/metrics"
	"github.com/coltconsulting/click-dog/internal/model"
	"github.com/coltconsulting/click-dog/internal/resilience"
)

// versionReader adds the optional ServerVersionReader surface to the standard
// live-reader fixture.
type versionReader struct {
	*mockLiveReader
	versions []string // returned in order; the last repeats
	err      error
	calls    int
}

func (v *versionReader) ServerVersion(_ context.Context) (string, error) {
	v.calls++
	if v.err != nil {
		return "", v.err
	}
	if len(v.versions) == 0 {
		return "", nil
	}
	i := v.calls - 1
	if i >= len(v.versions) {
		i = len(v.versions) - 1
	}
	return v.versions[i], nil
}

// versionExporter adds the optional ServerVersionStamper surface.
type versionExporter struct {
	*mockExporter
	stamped []string
}

func (v *versionExporter) SetServerVersion(version string) {
	v.stamped = append(v.stamped, version)
}

func (v *versionExporter) last() string {
	if len(v.stamped) == 0 {
		return "<never stamped>"
	}
	return v.stamped[len(v.stamped)-1]
}

func newVersionFixtures(versions ...string) (*versionReader, *versionExporter) {
	return &versionReader{mockLiveReader: &mockLiveReader{healthy: true}, versions: versions},
		&versionExporter{mockExporter: &mockExporter{}}
}

// refreshNow drives one refresh far enough past the previous attempt that the
// throttle never suppresses it, isolating the behavior under test.
func refreshNow(s *serverVersionStamp, reader, exporter any, tick int) {
	s.Refresh(context.Background(), reader, exporter, time.Now().Add(time.Duration(tick)*time.Hour))
}

func TestServerVersionStamp_StampsExporter(t *testing.T) {
	reader, exporter := newVersionFixtures("25.3.2.1")
	var stamp serverVersionStamp

	refreshNow(&stamp, reader, exporter, 0)

	if got := exporter.stamped; len(got) != 1 || got[0] != "25.3.2.1" {
		t.Fatalf("stamped = %v, want [25.3.2.1]", got)
	}
}

// A ClickHouse upgrade under a running click-dog must be picked up rather than
// held until restart.
func TestServerVersionStamp_TracksUpgrade(t *testing.T) {
	reader, exporter := newVersionFixtures("24.8.4.13", "24.8.4.13", "25.3.2.1")
	var stamp serverVersionStamp

	for tick := 0; tick < 3; tick++ {
		refreshNow(&stamp, reader, exporter, tick)
	}

	want := []string{"24.8.4.13", "24.8.4.13", "25.3.2.1"}
	if len(exporter.stamped) != len(want) {
		t.Fatalf("stamped %v, want %v", exporter.stamped, want)
	}
	for i := range want {
		if exporter.stamped[i] != want[i] {
			t.Fatalf("stamp %d = %q, want %q", i, exporter.stamped[i], want[i])
		}
	}
}

// A stale version is worse than an absent one: it attributes new telemetry to
// a build that is no longer running.
func TestServerVersionStamp_ClearsOnError(t *testing.T) {
	reader, exporter := newVersionFixtures("25.3.2.1")
	var stamp serverVersionStamp

	refreshNow(&stamp, reader, exporter, 0)
	reader.err = errors.New("connection reset")
	refreshNow(&stamp, reader, exporter, 1)

	if got := exporter.stamped; len(got) != 2 || got[1] != "" {
		t.Fatalf("stamped = %v, want the second stamp cleared", got)
	}
	if stamp.last != "" {
		t.Fatalf("stamp.last = %q, want cleared", stamp.last)
	}
}

func TestServerVersionStamp_FailureIsNotFatal(t *testing.T) {
	reader, exporter := newVersionFixtures()
	reader.err = errors.New("denied")
	var stamp serverVersionStamp

	refreshNow(&stamp, reader, exporter, 0)

	if got := exporter.stamped; len(got) != 1 || got[0] != "" {
		t.Fatalf("stamped = %v, want [\"\"]", got)
	}
}

// The version changes on the order of twice a year; re-reading it every cycle
// would add a round trip at poll frequency for nothing.
func TestServerVersionStamp_ThrottlesReads(t *testing.T) {
	reader, exporter := newVersionFixtures("25.3.2.1")
	var stamp serverVersionStamp
	start := time.Now()

	stamp.Refresh(context.Background(), reader, exporter, start)
	stamp.Refresh(context.Background(), reader, exporter, start.Add(serverVersionRefreshInterval/2))
	if reader.calls != 1 {
		t.Fatalf("reader called %d times within the interval, want 1", reader.calls)
	}

	stamp.Refresh(context.Background(), reader, exporter, start.Add(serverVersionRefreshInterval+time.Second))
	if reader.calls != 2 {
		t.Fatalf("reader called %d times after the interval, want 2", reader.calls)
	}
}

// Clear must also defeat the throttle, so recovery from a degraded cycle
// re-reads at once instead of waiting out the interval.
func TestServerVersionStamp_ClearForcesNextRead(t *testing.T) {
	reader, exporter := newVersionFixtures("24.8.4.13", "25.3.2.1")
	var stamp serverVersionStamp
	start := time.Now()

	stamp.Refresh(context.Background(), reader, exporter, start)
	stamp.Clear(exporter)
	stamp.Refresh(context.Background(), reader, exporter, start.Add(time.Second))

	if reader.calls != 2 {
		t.Fatalf("reader called %d times, want 2 (Clear must force a re-read)", reader.calls)
	}
	if got := exporter.last(); got != "25.3.2.1" {
		t.Fatalf("last stamp = %q, want 25.3.2.1", got)
	}
}

func TestServerVersionStamp_ClearDropsTheStamp(t *testing.T) {
	reader, exporter := newVersionFixtures("25.3.2.1")
	var stamp serverVersionStamp

	refreshNow(&stamp, reader, exporter, 0)
	stamp.Clear(exporter)

	if got := exporter.last(); got != "" {
		t.Fatalf("last stamp = %q, want cleared", got)
	}
}

// Both sides are optional companions. Neither missing half may panic or query.
func TestServerVersionStamp_OptionalCompanions(t *testing.T) {
	t.Run("reader cannot report a version", func(t *testing.T) {
		exporter := &versionExporter{mockExporter: &mockExporter{}}
		var stamp serverVersionStamp

		refreshNow(&stamp, &mockLiveReader{healthy: true}, exporter, 0)

		if len(exporter.stamped) != 0 {
			t.Fatalf("stamped = %v, want no stamp", exporter.stamped)
		}
	})

	t.Run("exporter has no envelope to stamp", func(t *testing.T) {
		reader, _ := newVersionFixtures("25.3.2.1")
		var stamp serverVersionStamp

		refreshNow(&stamp, reader, &mockExporter{}, 0)

		if reader.calls != 0 {
			t.Fatalf("reader queried %d times, want 0 when nothing can use the result", reader.calls)
		}
	})
}

// The wiring between a resolved version and an exported attribute is a single
// call in Process. Without this test, deleting it leaves the suite green.
func TestPipeline_ProcessStampsServerVersion(t *testing.T) {
	reader, exporter := newVersionFixtures("25.3.2.1")
	pipeline := mustLivePipeline(t, reader, exporter, &config.Config{}, nil, metrics.NewMetrics())

	if err := pipeline.Process(context.Background()); err != nil {
		t.Fatalf("Process: %v", err)
	}

	if got := exporter.last(); got != "25.3.2.1" {
		t.Fatalf("Process stamped %q, want 25.3.2.1", got)
	}
}

// A degraded cycle still exports a canary span. It must not carry a version
// this cycle could not confirm — an upgrade is the likeliest cause of the
// outage that opened the breaker.
func TestPipeline_ProcessClearsServerVersionWhenDegraded(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*Pipeline, *versionReader)
	}{
		{
			name: "circuit breaker open",
			setup: func(p *Pipeline, _ *versionReader) {
				cb := resilience.NewCircuitBreaker(config.CircuitBreakerConfig{
					Enabled:          true,
					FailureThreshold: 1,
					SuccessThreshold: 1,
					ResetTimeoutS:    3600,
				})
				cb.RecordFailure()
				p.CircuitBreaker = cb
			},
		},
		{
			name:  "health check fails",
			setup: func(_ *Pipeline, r *versionReader) { r.healthy = false },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reader, exporter := newVersionFixtures("24.8.4.13")
			pipeline := mustLivePipeline(t, reader, exporter, &config.Config{}, nil, metrics.NewMetrics())

			// A healthy cycle first, so there is a version to go stale.
			if err := pipeline.Process(context.Background()); err != nil {
				t.Fatalf("first Process: %v", err)
			}
			if got := exporter.last(); got != "24.8.4.13" {
				t.Fatalf("setup stamp = %q, want 24.8.4.13", got)
			}

			tc.setup(pipeline, reader)
			_ = pipeline.Process(context.Background())

			if got := exporter.last(); got != "" {
				t.Fatalf("degraded cycle left stamp %q, want it cleared", got)
			}
		})
	}
}

var _ model.SpanExporter = (*versionExporter)(nil)

// Inspect the stamp while the canary is being exported, not after Process
// returns: clearing after the send still leaves stale telemetry downstream.
func TestPipeline_DegradedCanaryClearsVersionBeforeExport(t *testing.T) {
	for _, degradedPath := range []string{"circuit breaker open", "elevated backoff"} {
		t.Run(degradedPath, func(t *testing.T) {
			reader, exporter := newVersionFixtures("24.8.4.13", "25.3.2.1")
			cfg := &config.Config{Monitor: config.MonitorConfig{
				CheckIntervalS: 10,
				Canary:         config.CanaryConfig{Enabled: true, ThresholdDurationMs: 60000},
			}}
			pipeline := mustLivePipeline(t, reader, exporter, cfg, nil, metrics.NewMetrics())
			if err := pipeline.Process(context.Background()); err != nil {
				t.Fatalf("healthy Process: %v", err)
			}
			if exporter.last() != "24.8.4.13" {
				t.Fatalf("setup stamp = %q", exporter.last())
			}

			querier := &mockCanaryQuerier{}
			pipeline.CanaryQuerier = querier
			if degradedPath == "circuit breaker open" {
				pipeline.CircuitBreaker = resilience.NewCircuitBreaker(config.CircuitBreakerConfig{
					Enabled: true, FailureThreshold: 1, SuccessThreshold: 1, ResetTimeoutS: 3600,
				})
				pipeline.CircuitBreaker.RecordFailure()
			} else {
				pipeline.Poller = resilience.NewAdaptivePoller(cfg.GetCheckInterval(), config.BackoffConfig{
					MaxIntervalS: 300, BackoffFactor: 3,
				})
				pipeline.Poller.RecordFailure()
			}

			canaryExports := 0
			exporter.exportSpansFunc = func(_ context.Context, spans []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
				canaryExports++
				if len(spans) != 1 || spans[0].OperationName != CanarySpanName {
					t.Fatalf("expected one canary span, got %v", spans)
				}
				if got := exporter.last(); got != "" {
					t.Errorf("canary exported with stale clickhouse.version %q", got)
				}
				return []model.SpanKey{model.KeyOf(spans[0])}, nil
			}
			if err := pipeline.Process(context.Background()); !errors.Is(err, ErrCanaryRan) {
				t.Fatalf("degraded Process = %v, want ErrCanaryRan", err)
			}
			if canaryExports != 1 || querier.calls != 1 || reader.calls != 1 {
				t.Fatalf("exports=%d canary queries=%d version reads=%d, want 1 each", canaryExports, querier.calls, reader.calls)
			}

			pipeline.CircuitBreaker = nil
			pipeline.Poller = nil
			exporter.exportSpansFunc = nil
			if err := pipeline.Process(context.Background()); err != nil {
				t.Fatalf("recovered Process: %v", err)
			}
			if reader.calls != 2 || exporter.last() != "25.3.2.1" {
				t.Fatalf("recovery reads=%d stamp=%q, want immediate read of 25.3.2.1", reader.calls, exporter.last())
			}
		})
	}
}
