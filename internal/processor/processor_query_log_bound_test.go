package processor

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/metrics"
	"github.com/coltconsulting/click-dog/internal/model"
)

func TestEarliestQueryStartUs_IgnoresSpansWithoutQueryID(t *testing.T) {
	spans := []model.OpenTelemetrySpan{
		{StartTimeUs: 1_000, Attributes: map[string]string{}},
		{StartTimeUs: 3_000, Attributes: map[string]string{"clickhouse.query_id": "q-1"}},
		{StartTimeUs: 2_000, Attributes: map[string]string{"clickhouse.query_id": "q-2"}},
		{StartTimeUs: 500, Attributes: map[string]string{"clickhouse.query_id": ""}},
	}
	if got := EarliestQueryStartUs(spans); got != 2_000 {
		t.Errorf("EarliestQueryStartUs = %d, want 2000 (spans without a query ID ignored)", got)
	}
	if got := EarliestQueryStartUs(spans[:1]); got != 0 {
		t.Errorf("EarliestQueryStartUs with no query span = %d, want 0", got)
	}
}

func TestEarliestStartUs_OnlyTheGivenTraces(t *testing.T) {
	a := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	b := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	spans := []model.OpenTelemetrySpan{
		{TraceID: a, StartTimeUs: 1_000},
		{TraceID: b, StartTimeUs: 3_000},
		{TraceID: b, StartTimeUs: 2_000},
	}
	if got := EarliestStartUs(spans, []uuid.UUID{b}); got != 2_000 {
		t.Errorf("EarliestStartUs(b) = %d, want 2000", got)
	}
	if got := EarliestStartUs(spans, []uuid.UUID{uuid.New()}); got != 0 {
		t.Errorf("EarliestStartUs(absent trace) = %d, want 0", got)
	}
}

// TestLiveCycle_BoundsQueryLogReadsBySpanStart pins what each query_log read
// in a cycle is bounded by: enrichment by the earliest start of the page's
// query spans, and the trace filters' lookups of the traces' queries and
// their origins by the earliest start of the page's spans of those traces.
func TestLiveCycle_BoundsQueryLogReadsBySpanStart(t *testing.T) {
	traceA := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	traceB := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	page := []model.OpenTelemetrySpan{
		{TraceID: traceA, SpanID: 1, OperationName: "query", StartTimeUs: 2_000_000_000, FinishTimeUs: 2_003_000_000,
			Attributes: map[string]string{"clickhouse.query_id": "q-a"}},
		{TraceID: traceA, SpanID: 2, ParentSpanID: 1, OperationName: "MergeTreeSelect", StartTimeUs: 2_000_500_000, FinishTimeUs: 2_002_000_000,
			Attributes: map[string]string{}},
		// Trace B's query span landed in another page.
		{TraceID: traceB, SpanID: 3, ParentSpanID: 9, OperationName: "MergeTreeSelect", StartTimeUs: 1_800_000_000, FinishTimeUs: 1_802_000_000,
			Attributes: map[string]string{}},
	}
	enrich := true
	cfg := &config.Config{
		Monitor: config.MonitorConfig{Enabled: true, MinTraceDurationMs: 1000, CheckIntervalS: 30, EnrichFromQueryLog: &enrich},
		Filters: config.FiltersConfig{WhitelistIPs: []string{"192.168.0.0/16"}},
	}
	reader := &mockLiveReader{
		healthy: true,
		spans:   page,
		queryLogs: map[string]model.QueryLog{
			"q-a": {QueryID: "q-a", InitialAddress: "192.168.4.7"},
			"q-b": {QueryID: "q-b", InitialAddress: "192.168.4.8"},
		},
		traceQueryIDs: map[uuid.UUID][]string{traceA: {"q-a"}, traceB: {"q-b"}},
	}
	pipeline := mustLivePipeline(t, reader, &mockExporter{}, cfg, nil, metrics.NewMetrics())
	if err := pipeline.Process(context.Background()); err != nil {
		t.Fatalf("Process: %v", err)
	}

	if want := []uint64{2_000_000_000}; !reflect.DeepEqual(reader.queryLogStartUs, want) {
		t.Errorf("enrichment read starts = %v, want %v (the page's earliest query span)", reader.queryLogStartUs, want)
	}
	if reader.lastTraceQueryStartUs != 1_800_000_000 || reader.lastOriginsStartUs != 1_800_000_000 {
		t.Errorf("trace filter reads start at %d (span log) and %d (query_log), want 1800000000 for both (trace B's earliest span)",
			reader.lastTraceQueryStartUs, reader.lastOriginsStartUs)
	}
}
