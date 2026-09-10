package processor

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/coltconsulting/click-dog/internal/clickhouse"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/metrics"
	"github.com/coltconsulting/click-dog/internal/model"
)

type mockLiveReader struct {
	healthy bool

	spans []model.OpenTelemetrySpan
	// pages, when set, is served one slice per fetch in order (the last page
	// repeats), standing in for the reader's keyset paging across cycles.
	pages    [][]model.OpenTelemetrySpan
	fetchErr error

	queryLogs    map[string]model.QueryLog
	queryLogsErr error

	traceQueryIDs    map[uuid.UUID][]string
	traceQueryIDsErr error

	healthCalls       int
	fetchCalls        int
	queryLogCalls     int
	traceQueryIDCalls int
	lastMinTraceMs    int
	lastLookback      time.Duration
	lastLimit         int
	lastFetchOpts     clickhouse.FetchOpts
	lastQueryIDs      []string
	lastQueryDays     int
}

var _ LiveReader = (*mockLiveReader)(nil)

func mustLivePipeline(
	t *testing.T,
	reader LiveReader,
	exporter model.SpanExporter,
	cfg *config.Config,
	seenSpans *lru.Cache[model.SpanKey, bool],
	m *metrics.Metrics,
) *Pipeline {
	t.Helper()

	qf, err := filter.NewQueryFilter(cfg.Filters)
	if err != nil {
		t.Fatalf("NewQueryFilter: %v", err)
	}
	if seenSpans == nil {
		seenSpans, err = lru.New[model.SpanKey, bool](1024)
		if err != nil {
			t.Fatalf("lru.New: %v", err)
		}
	}
	pipeline, err := NewPipeline(Pipeline{
		Reader:    reader,
		Exporter:  exporter,
		Filter:    qf,
		Config:    cfg,
		SeenSpans: seenSpans,
		Metrics:   m,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return pipeline
}

func (r *mockLiveReader) IsHealthy(context.Context) bool {
	r.healthCalls++
	return r.healthy
}

func (r *mockLiveReader) FetchOpenTelemetrySpansWithOpts(
	_ context.Context,
	minTraceDurationMs int,
	lookback time.Duration,
	limit int,
	opts clickhouse.FetchOpts,
) ([]model.OpenTelemetrySpan, error) {
	r.fetchCalls++
	r.lastMinTraceMs = minTraceDurationMs
	r.lastLookback = lookback
	r.lastLimit = limit
	r.lastFetchOpts = opts
	if len(r.pages) > 0 {
		i := r.fetchCalls - 1
		if i >= len(r.pages) {
			i = len(r.pages) - 1
		}
		return r.pages[i], r.fetchErr
	}
	return r.spans, r.fetchErr
}

func (r *mockLiveReader) FetchQueryLogByQueryIDs(
	_ context.Context,
	queryIDs []string,
	lookbackDays int,
) (map[string]model.QueryLog, error) {
	r.queryLogCalls++
	r.lastQueryIDs = append(r.lastQueryIDs[:0], queryIDs...)
	r.lastQueryDays = lookbackDays
	return r.queryLogs, r.queryLogsErr
}

func (r *mockLiveReader) FetchTraceQueryIDs(
	_ context.Context,
	traceIDs []uuid.UUID,
	_ int,
) (map[uuid.UUID][]string, error) {
	r.traceQueryIDCalls++
	if r.traceQueryIDsErr != nil {
		return nil, r.traceQueryIDsErr
	}
	out := make(map[uuid.UUID][]string, len(traceIDs))
	for _, id := range traceIDs {
		if qids, ok := r.traceQueryIDs[id]; ok {
			out[id] = qids
		}
	}
	return out, nil
}
