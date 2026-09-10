package processor

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/metrics"
)

func TestPipeline_ProcessPassesLiveReaderOptions(t *testing.T) {
	reader := &mockLiveReader{healthy: true}
	m := metrics.NewMetrics()
	cfg := &config.Config{
		Monitor: config.MonitorConfig{
			MinTraceDurationMs: 1200,
			MaxTraceDurationMs: 9300,
			MinSpanDurationMs:  45,
			MaxSpanDurationMs:  6700,
			LookbackS:          75,
			MaxSpansPerCycle:   321,
		},
		Filters: config.FiltersConfig{
			BlacklistOperations: []string{"SYSTEM", "internal"},
		},
	}
	pipeline := mustLivePipeline(t, reader, &mockExporter{}, cfg, nil, m)

	if err := pipeline.Process(context.Background()); err != nil {
		t.Fatalf("Pipeline.Process: %v", err)
	}
	if reader.healthCalls != 1 || reader.fetchCalls != 1 {
		t.Fatalf("reader calls = health:%d fetch:%d, want 1/1", reader.healthCalls, reader.fetchCalls)
	}
	if reader.lastMinTraceMs != 1200 || reader.lastLookback != 75*time.Second || reader.lastLimit != 321 {
		t.Errorf("reader args = min:%d lookback:%v limit:%d", reader.lastMinTraceMs, reader.lastLookback, reader.lastLimit)
	}
	if reader.lastFetchOpts.MaxTraceDurationMs != 9300 ||
		reader.lastFetchOpts.MinSpanDurationMs != 45 ||
		reader.lastFetchOpts.MaxSpanDurationMs != 6700 ||
		!slices.Equal(reader.lastFetchOpts.BlacklistOperations, []string{"SYSTEM", "internal"}) {
		t.Errorf("reader options = %+v", reader.lastFetchOpts)
	}
}

func TestNewPipeline_ValidatesRequiredDependencies(t *testing.T) {
	_, err := NewPipeline(Pipeline{})
	if err == nil {
		t.Fatal("NewPipeline() error = nil, want missing dependency error")
	}

	for _, want := range []string{"Config", "Reader", "Exporter", "Filter", "SeenSpans", "Metrics"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("NewPipeline() error %q missing %q", err, want)
		}
	}
}

// TestPipeline_LeaderGateSkipsWhenNotLeader pins the cluster-mode standby path:
// a LeaderGate that denies the cycle must short-circuit before any ClickHouse
// work — Reader is nil here precisely to prove it is never touched — record a
// skipped cycle (not error, not success), and leave last-success at zero. This
// is the data-path half of the leader-gating change; the decision table itself
// is covered by main.TestNewLeaderGate.
func TestPipeline_LeaderGateSkipsWhenNotLeader(t *testing.T) {
	m := metrics.NewMetrics()
	qf, _ := filter.NewQueryFilter(config.FiltersConfig{})
	exp := &mockExporter{}

	err := (&Pipeline{
		Reader:     nil, // must stay untouched — the gate fires first
		Exporter:   exp,
		Filter:     qf,
		Config:     &config.Config{},
		Metrics:    m,
		LeaderGate: func() bool { return false },
	}).Process(context.Background())

	if !errors.Is(err, ErrLeaderStandby) {
		t.Fatalf("standby cycle should return ErrLeaderStandby (so the poller leaves backoff untouched), got %v", err)
	}
	if exp.exportSpansCalls != 0 {
		t.Errorf("standby must export nothing, got %d ExportSpans calls", exp.exportSpansCalls)
	}
	snap := m.Snapshot()
	if snap.Leader != 0 {
		t.Errorf("standby cycle should set leader gauge to 0, got %d", snap.Leader)
	}
	if snap.LastCycle.SkipReason != metrics.SkipReasonLeaderStandby {
		t.Errorf("standby skip reason = %q, want %q", snap.LastCycle.SkipReason, metrics.SkipReasonLeaderStandby)
	}

	body := scrapeMetrics(t, m)
	mustContain(t, body, "click_dog_leader 0")
	mustContain(t, body, `click_dog_last_success_timestamp_seconds{role="standby"} 0`)
	mustContain(t, body, `click_dog_cycle_results_total{result="skipped"} 1`)
	mustContain(t, body, `click_dog_cycle_results_total{result="success"} 0`)
	mustContain(t, body, `click_dog_cycle_results_total{result="error"} 0`)
	// A standby skip is not evidence of a healthy export — last-success must
	// not advance (operators alert on this gauge).
	mustContain(t, body, "click_dog_last_success_timestamp_seconds 0")
}
