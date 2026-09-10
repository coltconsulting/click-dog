package processor

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/metrics"
	"github.com/coltconsulting/click-dog/internal/model"
)

func TestResolveSpanClientAddress(t *testing.T) {
	traceA := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	traceB := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	tests := []struct {
		name        string
		traceID     uuid.UUID
		attributes  map[string]string
		queryLogMap map[string]model.QueryLog
		traceAddrs  map[uuid.UUID]string
		want        string
	}{
		{
			name:       "no attributes and no trace map returns empty",
			attributes: map[string]string{},
			want:       "",
		},
		{
			// The query log names the originating client on every row of a
			// query; a client.address attribute (not emitted by ClickHouse
			// today) would be per server, so the query-log answer wins.
			name: "query_log row wins over a client.address attribute",
			attributes: map[string]string{
				"client.address":      "10.0.0.1",
				"clickhouse.query_id": "qid-1",
			},
			queryLogMap: map[string]model.QueryLog{"qid-1": {ClientAddress: "10.9.9.9"}},
			want:        "10.9.9.9",
		},
		{
			name:        "client.address attribute is the last resort",
			attributes:  map[string]string{"client.address": "10.0.0.1"},
			queryLogMap: map[string]model.QueryLog{},
			want:        "10.0.0.1",
		},
		{
			// A trace decision, once made, applies to every span in the trace,
			// including a secondary query span whose own row names the
			// initiating server.
			name:        "trace decision overrides a span's own row",
			traceID:     traceA,
			attributes:  map[string]string{"clickhouse.query_id": "qid-secondary"},
			queryLogMap: map[string]model.QueryLog{"qid-secondary": {ClientAddress: "10.0.0.9"}},
			traceAddrs:  map[uuid.UUID]string{traceA: "192.168.4.7"},
			want:        "192.168.4.7",
		},
		{
			// The load-bearing case: ClickHouse does not write client.address
			// onto system.opentelemetry_span_log rows, so every real
			// scheduled-mode query root reaches the filter through this branch.
			name:        "falls back to query_log enrichment by query_id",
			attributes:  map[string]string{"clickhouse.query_id": "qid-1"},
			queryLogMap: map[string]model.QueryLog{"qid-1": {ClientAddress: "192.168.4.7"}},
			want:        "192.168.4.7",
		},
		{
			// Internal child spans carry neither attribute; without the trace
			// map they would resolve empty and be dropped, shredding the trace.
			name:       "child span inherits its trace's address",
			traceID:    traceA,
			attributes: map[string]string{"db.statement": "SELECT 1"},
			traceAddrs: map[uuid.UUID]string{traceA: "192.168.4.7"},
			want:       "192.168.4.7",
		},
		{
			name:       "child span of an unresolved trace stays empty",
			traceID:    traceB,
			attributes: map[string]string{"db.statement": "SELECT 1"},
			traceAddrs: map[uuid.UUID]string{traceA: "192.168.4.7"},
			want:       "",
		},
		{
			name:        "query_id present but missing from map",
			attributes:  map[string]string{"clickhouse.query_id": "qid-missing"},
			queryLogMap: map[string]model.QueryLog{"other-qid": {ClientAddress: "10.0.0.5"}},
			want:        "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			span := model.OpenTelemetrySpan{TraceID: tt.traceID, Attributes: tt.attributes}
			got := ResolveSpanClientAddress(span, tt.queryLogMap, tt.traceAddrs)
			if got != tt.want {
				t.Errorf("ResolveSpanClientAddress(...) = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestTraceClientAddresses_ScopesToTrace pins the trace-unit contract: only the
// query root resolves an address, and every span in that trace must inherit it.
func TestTraceClientAddresses_ScopesToTrace(t *testing.T) {
	traceA := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	traceB := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	traceC := uuid.MustParse("33333333-3333-3333-3333-333333333333")

	spans := []model.OpenTelemetrySpan{
		// Child listed before its root, so the map cannot depend on ordering.
		{TraceID: traceA, OperationName: "Collector", Attributes: map[string]string{}},
		{TraceID: traceA, OperationName: "query", Attributes: map[string]string{"clickhouse.query_id": "q1"}},
		{TraceID: traceB, OperationName: "query", Attributes: map[string]string{"clickhouse.query_id": "q2"}},
		// traceC has no root in this cycle and must stay unresolved.
		{TraceID: traceC, OperationName: "Formatter", Attributes: map[string]string{}},
	}
	qmap := map[string]model.QueryLog{
		"q1": {QueryID: "q1", ClientAddress: "192.168.4.7"},
		"q2": {QueryID: "q2", ClientAddress: "10.4.0.9"},
	}

	addrs := TraceClientAddresses(spans, qmap)
	if got := addrs[traceA]; got != "192.168.4.7" {
		t.Errorf("traceA = %q, want 192.168.4.7 (root appears after its child)", got)
	}
	if got := addrs[traceB]; got != "10.4.0.9" {
		t.Errorf("traceB = %q, want 10.4.0.9", got)
	}
	if got, ok := addrs[traceC]; ok {
		t.Errorf("traceC resolved to %q, want absent", got)
	}
}

// TestIPWhitelist_AdmitsWholeTrace is the F1 regression. Before the fix the
// filter read span.Attributes["client.address"] alone, saw "" for every span,
// and an active whitelist dropped 100% of the cycle. A half-fix that resolved
// per span would admit the root and drop the children; the whitelist is a
// statement about clients, so the whole trace must survive.
func TestIPWhitelist_AdmitsWholeTrace(t *testing.T) {
	traceA := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	traceB := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	spans := []model.OpenTelemetrySpan{
		{TraceID: traceA, OperationName: "query", Attributes: map[string]string{"clickhouse.query_id": "q1"}},
		{TraceID: traceA, OperationName: "Collector", Attributes: map[string]string{}},
		{TraceID: traceA, OperationName: "Formatter", Attributes: map[string]string{}},
		{TraceID: traceB, OperationName: "query", Attributes: map[string]string{"clickhouse.query_id": "q2"}},
		{TraceID: traceB, OperationName: "Collector", Attributes: map[string]string{}},
	}
	qmap := map[string]model.QueryLog{
		"q1": {QueryID: "q1", ClientAddress: "192.168.4.7"},
		"q2": {QueryID: "q2", ClientAddress: "10.4.0.9"},
	}

	f, err := filter.NewQueryFilter(config.FiltersConfig{WhitelistIPs: []string{"192.168.0.0/16"}})
	if err != nil {
		t.Fatalf("NewQueryFilter: %v", err)
	}
	if !f.HasIPWhitelist() {
		t.Fatal("HasIPWhitelist() = false, want true with a configured whitelist")
	}

	traceAddrs := TraceClientAddresses(spans, qmap)
	var passed, dropped int
	for _, span := range spans {
		addr := ResolveSpanClientAddress(span, qmap, traceAddrs)
		if f.ShouldFilter(span.OperationName, span.Attributes["db.statement"], addr) {
			dropped++
			if span.TraceID == traceA {
				t.Errorf("span %q from whitelisted trace was dropped (addr=%q)", span.OperationName, addr)
			}
			continue
		}
		passed++
		if span.TraceID == traceB {
			t.Errorf("span %q from non-whitelisted trace was exported (addr=%q)", span.OperationName, addr)
		}
	}
	if passed != 3 {
		t.Errorf("exported %d spans, want all 3 of the whitelisted trace", passed)
	}
	if dropped != 2 {
		t.Errorf("dropped %d spans, want both of the non-whitelisted trace", dropped)
	}
}

// TestHasIPWhitelist_GuardsResolution documents why the guard exists: with no
// whitelist configured the per-cycle trace map is never built.
func TestHasIPWhitelist_GuardsResolution(t *testing.T) {
	f, err := filter.NewQueryFilter(config.FiltersConfig{})
	if err != nil {
		t.Fatalf("NewQueryFilter: %v", err)
	}
	if f.HasIPWhitelist() {
		t.Error("HasIPWhitelist() = true with no whitelist configured, want false")
	}
	// An unresolved address must still pass when no whitelist is active.
	if f.ShouldFilter("", "SELECT 1", "") {
		t.Error("ShouldFilter with no whitelist filtered a span, want passed")
	}
}

// TestIPWhitelist_DistributedTraceUsesOriginatingClient pins the review
// finding on #501: a distributed query fans out as secondary queries whose
// query_log `address` is the initiating server. The whitelist must judge the
// originating client (`initial_address`) for every span of the trace, in any
// row order, so a client-scoped whitelist admits the whole trace and a
// server-subnet whitelist admits none of it.
func TestIPWhitelist_DistributedTraceUsesOriginatingClient(t *testing.T) {
	trace := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	initial := model.OpenTelemetrySpan{TraceID: trace, OperationName: "query", Attributes: map[string]string{"clickhouse.query_id": "q-initial"}}
	secondary := model.OpenTelemetrySpan{TraceID: trace, OperationName: "query", Attributes: map[string]string{"clickhouse.query_id": "q-secondary"}}
	child := model.OpenTelemetrySpan{TraceID: trace, OperationName: "MergeTreeSelect", Attributes: map[string]string{}}
	qmap := map[string]model.QueryLog{
		"q-initial":   {QueryID: "q-initial", ClientAddress: "192.168.4.7", InitialAddress: "192.168.4.7"},
		"q-secondary": {QueryID: "q-secondary", ClientAddress: "10.0.0.9", InitialAddress: "192.168.4.7"},
	}
	orders := map[string][]model.OpenTelemetrySpan{
		"initial first":   {initial, secondary, child},
		"secondary first": {secondary, child, initial},
		"child first":     {child, secondary, initial},
	}

	for _, tc := range []struct {
		name      string
		whitelist string
		wantPass  int
	}{
		{"client subnet admits the whole trace", "192.168.0.0/16", 3},
		{"server subnet admits nothing", "10.0.0.0/8", 0},
	} {
		f, err := filter.NewQueryFilter(config.FiltersConfig{WhitelistIPs: []string{tc.whitelist}})
		if err != nil {
			t.Fatalf("NewQueryFilter: %v", err)
		}
		for order, spans := range orders {
			traceAddrs := TraceClientAddresses(spans, qmap)
			if got := traceAddrs[trace]; got != "192.168.4.7" {
				t.Errorf("%s/%s: trace resolved to %q, want the originating client 192.168.4.7", tc.name, order, got)
			}
			passed := 0
			for _, span := range spans {
				addr := ResolveSpanClientAddress(span, qmap, traceAddrs)
				if addr != "192.168.4.7" {
					t.Errorf("%s/%s: span %q resolved to %q, want the trace's originating client", tc.name, order, span.OperationName, addr)
				}
				if !f.ShouldFilter(span.OperationName, "", addr) {
					passed++
				}
			}
			if passed != tc.wantPass {
				t.Errorf("%s/%s: %d of 3 spans passed, want %d", tc.name, order, passed, tc.wantPass)
			}
		}
	}
}

// TestOriginatingAddress_FallsBackToOwnAddress covers rows without a usable
// initial_address, which must keep the pre-existing single-node behavior.
func TestOriginatingAddress_FallsBackToOwnAddress(t *testing.T) {
	for _, tc := range []struct{ initial, own, want string }{
		{"192.168.4.7", "10.0.0.9", "192.168.4.7"},
		{"", "10.0.0.9", "10.0.0.9"},
		{"::", "10.0.0.9", "10.0.0.9"},
		{"0.0.0.0", "10.0.0.9", "10.0.0.9"},
	} {
		if got := OriginatingAddress(model.QueryLog{InitialAddress: tc.initial, ClientAddress: tc.own}); got != tc.want {
			t.Errorf("OriginatingAddress(initial=%q, own=%q) = %q, want %q", tc.initial, tc.own, got, tc.want)
		}
	}
}

// TestIPWhitelist_ResolvesAcrossSpanPages pins the other #501 review finding:
// the reader splits a trace across polling cycles at max_spans_per_cycle, so
// a whitelisted root on one page and its child on the next must still export
// both. The child's page has no query ID to enrich; the pipeline resolves the
// trace through the span log's record of its query IDs instead.
func TestIPWhitelist_ResolvesAcrossSpanPages(t *testing.T) {
	trace := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	root := model.OpenTelemetrySpan{TraceID: trace, SpanID: 1, OperationName: "query", Attributes: map[string]string{"clickhouse.query_id": "q-root"}}
	child := model.OpenTelemetrySpan{TraceID: trace, SpanID: 2, OperationName: "MergeTreeSelect", Attributes: map[string]string{}}

	enrich := true
	cfg := &config.Config{
		Monitor: config.MonitorConfig{Enabled: true, MinTraceDurationMs: 1000, CheckIntervalS: 30, MaxSpansPerCycle: 1, EnrichFromQueryLog: &enrich},
		Filters: config.FiltersConfig{WhitelistIPs: []string{"192.168.0.0/16"}},
	}
	reader := &mockLiveReader{
		healthy:       true,
		pages:         [][]model.OpenTelemetrySpan{{root}, {child}},
		queryLogs:     map[string]model.QueryLog{"q-root": {QueryID: "q-root", InitialAddress: "192.168.4.7", ClientAddress: "192.168.4.7"}},
		traceQueryIDs: map[uuid.UUID][]string{trace: {"q-root"}},
	}
	var exported []model.OpenTelemetrySpan
	exporter := &mockExporter{exportSpansFunc: func(_ context.Context, spans []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
		exported = append(exported, spans...)
		keys := make([]model.SpanKey, 0, len(spans))
		for _, s := range spans {
			keys = append(keys, model.KeyOf(s))
		}
		return keys, nil
	}}
	pipeline := mustLivePipeline(t, reader, exporter, cfg, nil, metrics.NewMetrics())

	for cycle := 1; cycle <= 2; cycle++ {
		if err := pipeline.Process(context.Background()); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
	}
	if len(exported) != 2 {
		t.Fatalf("exported %d spans across two pages, want both the root and its child: %+v", len(exported), exported)
	}
	if reader.traceQueryIDCalls != 1 {
		t.Errorf("span-log trace lookup ran %d times, want exactly once (only the child's page lacked a query root)", reader.traceQueryIDCalls)
	}

	// Strictness holds: when the span log has no query ID for the trace, the
	// orphaned child is dropped rather than admitted blind.
	reader = &mockLiveReader{healthy: true, pages: [][]model.OpenTelemetrySpan{{child}}, queryLogs: map[string]model.QueryLog{}}
	exported = nil
	pipeline = mustLivePipeline(t, reader, exporter, cfg, nil, metrics.NewMetrics())
	if err := pipeline.Process(context.Background()); err != nil {
		t.Fatalf("orphan cycle: %v", err)
	}
	if len(exported) != 0 {
		t.Errorf("an unresolvable orphan child was exported: %+v", exported)
	}
}
