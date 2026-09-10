package export

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/coltconsulting/click-dog/internal/clicklog"
)

// stubMetricExporter returns whatever error the test sets, so the wrapper's
// logging policy can be exercised without a collector.
type stubMetricExporter struct {
	sdkmetric.Exporter
	err error
}

func (s *stubMetricExporter) Export(context.Context, *metricdata.ResourceMetrics) error {
	return s.err
}

func captureSelfMetricsLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	// Mutates clicklog/stdlog process globals; keep callers non-parallel and
	// restore the exact prior stdlog state on cleanup.
	prevWriter := log.Writer()
	prevFlags := log.Flags()
	prevPrefix := log.Prefix()
	t.Cleanup(func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
		log.SetPrefix(prevPrefix)
	})

	if err := clicklog.InitLogger("info", "", "text", 0, 0); err != nil {
		t.Fatalf("InitLogger failed: %v", err)
	}
	var buf bytes.Buffer
	log.SetOutput(&buf)
	return &buf
}

// TestSelfMetricsExporter_SuppressesRepeatedErrors pins the log-volume
// regression. Self-metrics push every 10s by default and the usual failure —
// a collector with no metrics pipeline — never resolves on its own, so logging
// each dropped batch produced an unbounded ERROR stream that buried real
// errors while span export was healthy.
func TestSelfMetricsExporter_SuppressesRepeatedErrors(t *testing.T) {
	buf := captureSelfMetricsLog(t)
	stub := &stubMetricExporter{err: errors.New("unknown service MetricsService")}
	exp := &logAndDropMetricExporter{Exporter: stub}

	for i := 0; i < 20; i++ {
		if err := exp.Export(context.Background(), nil); err != nil {
			t.Fatalf("Export returned %v, want nil (errors are logged and dropped)", err)
		}
	}

	if got := strings.Count(buf.String(), "self-metrics export failed"); got != 1 {
		t.Errorf("logged %d failure lines for 20 identical failures, want 1", got)
	}
}

func TestSelfMetricsExporter_LogsEachDistinctError(t *testing.T) {
	buf := captureSelfMetricsLog(t)
	stub := &stubMetricExporter{err: errors.New("connection refused")}
	exp := &logAndDropMetricExporter{Exporter: stub}

	_ = exp.Export(context.Background(), nil)
	stub.err = errors.New("unknown service MetricsService")
	_ = exp.Export(context.Background(), nil)
	_ = exp.Export(context.Background(), nil)

	out := buf.String()
	if !strings.Contains(out, "connection refused") {
		t.Error("first distinct error was not logged")
	}
	if !strings.Contains(out, "unknown service MetricsService") {
		t.Error("second distinct error was not logged")
	}
	if got := strings.Count(out, "self-metrics export failed"); got != 2 {
		t.Errorf("logged %d failure lines for 2 distinct errors, want 2", got)
	}
}

// A persistent failure must still surface periodically — suppression bounds
// the rate, it does not silence the condition.
func TestSelfMetricsExporter_RepeatsAfterInterval(t *testing.T) {
	buf := captureSelfMetricsLog(t)
	stub := &stubMetricExporter{err: errors.New("unknown service MetricsService")}
	exp := &logAndDropMetricExporter{Exporter: stub}

	_ = exp.Export(context.Background(), nil)
	for i := 0; i < 5; i++ {
		_ = exp.Export(context.Background(), nil)
	}
	// Age the last-logged stamp past the repeat window rather than sleeping.
	exp.mu.Lock()
	exp.lastLogged = exp.lastLogged.Add(-selfMetricsErrorRepeat - time.Second)
	exp.mu.Unlock()
	_ = exp.Export(context.Background(), nil)

	out := buf.String()
	if !strings.Contains(out, "still failing") {
		t.Errorf("no repeat line after the interval elapsed; got %q", out)
	}
	if !strings.Contains(out, "batch(es) dropped since last message") {
		t.Error("repeat line does not report how many batches were dropped")
	}
}

// A repeat line resets only the since-last-message counter. Recovery reports
// the whole outage, so the batch that triggered the repeat must not be counted
// twice.
func TestSelfMetricsExporter_RecoveryCountSurvivesARepeat(t *testing.T) {
	buf := captureSelfMetricsLog(t)
	stub := &stubMetricExporter{err: errors.New("unknown service MetricsService")}
	exp := &logAndDropMetricExporter{Exporter: stub}

	_ = exp.Export(context.Background(), nil) // 1: logged
	_ = exp.Export(context.Background(), nil) // 2: suppressed
	exp.mu.Lock()
	exp.lastLogged = exp.lastLogged.Add(-selfMetricsErrorRepeat - time.Second)
	exp.mu.Unlock()
	_ = exp.Export(context.Background(), nil) // 3: repeat line, resets sinceLogged
	_ = exp.Export(context.Background(), nil) // 4: suppressed again

	buf.Reset()
	stub.err = nil
	_ = exp.Export(context.Background(), nil)

	if !strings.Contains(buf.String(), "recovered after 4 dropped batch(es)") {
		t.Errorf("recovery miscounted the outage; got %q, want 4 dropped batches", buf.String())
	}
}

func TestSelfMetricsExporter_LogsRecovery(t *testing.T) {
	buf := captureSelfMetricsLog(t)
	stub := &stubMetricExporter{err: errors.New("unknown service MetricsService")}
	exp := &logAndDropMetricExporter{Exporter: stub}

	_ = exp.Export(context.Background(), nil)
	_ = exp.Export(context.Background(), nil)
	stub.err = nil
	_ = exp.Export(context.Background(), nil)

	if !strings.Contains(buf.String(), "self-metrics export recovered") {
		t.Errorf("recovery was not logged; got %q", buf.String())
	}

	// A later failure after recovery logs again rather than being suppressed
	// as a repeat of the pre-recovery error.
	buf.Reset()
	stub.err = errors.New("unknown service MetricsService")
	_ = exp.Export(context.Background(), nil)
	if !strings.Contains(buf.String(), "self-metrics export failed") {
		t.Error("failure after recovery was suppressed, want logged")
	}
}
