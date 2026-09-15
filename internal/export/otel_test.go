package export

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	collectorpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/coltconsulting/click-dog/internal/model"
)

// mockTraceClient records Export calls and can return configurable errors.
type mockTraceClient struct {
	calls   []*collectorpb.ExportTraceServiceRequest
	errFunc func(req *collectorpb.ExportTraceServiceRequest) error
}

func (m *mockTraceClient) Export(ctx context.Context, in *collectorpb.ExportTraceServiceRequest, opts ...grpc.CallOption) (*collectorpb.ExportTraceServiceResponse, error) {
	m.calls = append(m.calls, in)
	if m.errFunc != nil {
		if err := m.errFunc(in); err != nil {
			return nil, err
		}
	}
	return &collectorpb.ExportTraceServiceResponse{}, nil
}

// makeSpans creates n test spans with a payload attribute to control size.
func makeSpans(n int, payloadSize int) []*tracepb.Span {
	spans := make([]*tracepb.Span, n)
	payload := make([]byte, payloadSize)
	for i := range payload {
		payload[i] = 'x'
	}
	for i := range spans {
		spans[i] = &tracepb.Span{
			TraceId: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
			SpanId:  []byte{1, 2, 3, 4, 5, 6, 7, 8},
			Name:    "test-span",
			Attributes: []*commonpb.KeyValue{
				{
					Key:   "payload",
					Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: string(payload)}},
				},
			},
		}
	}
	return spans
}

func newTestExporter(client *mockTraceClient) *OTELExporter {
	exp := &OTELExporter{
		grpcClient:  client,
		serviceName: "test-service",
	}
	exp.wrapperBytes = proto.Size(exp.buildExportRequest(nil))
	return exp
}

func TestExportOTLPSpans_SmallBatch(t *testing.T) {
	mock := &mockTraceClient{}
	exp := newTestExporter(mock)

	spans := makeSpans(5, 10)
	if err := exp.exportOTLPSpans(context.Background(), spans); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(mock.calls))
	}
	got := len(mock.calls[0].ResourceSpans[0].ScopeSpans[0].Spans)
	if got != 5 {
		t.Fatalf("expected 5 spans in call, got %d", got)
	}
}

func TestExportOTLPSpans_LargeBatchChunks(t *testing.T) {
	mock := &mockTraceClient{}
	exp := newTestExporter(mock)

	// Create spans large enough to exceed grpcMaxBytes.
	// Each span ~1KB payload → 4000 spans ≈ 4MB > 3.5MB limit.
	spans := makeSpans(4000, 1000)

	// Verify our test data is actually over the limit
	req := exp.buildExportRequest(spans)
	if proto.Size(req) <= grpcMaxBytes {
		t.Fatal("test spans should exceed grpcMaxBytes")
	}

	if err := exp.exportOTLPSpans(context.Background(), spans); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.calls) < 2 {
		t.Fatalf("expected multiple calls for large batch, got %d", len(mock.calls))
	}

	// All spans should be accounted for
	total := 0
	for _, call := range mock.calls {
		total += len(call.ResourceSpans[0].ScopeSpans[0].Spans)
	}
	if total != 4000 {
		t.Fatalf("expected 4000 total spans across chunks, got %d", total)
	}
}

func TestExportOTLPSpans_BisectsOnResourceExhausted(t *testing.T) {
	exhaustedOnce := true
	mock := &mockTraceClient{
		errFunc: func(req *collectorpb.ExportTraceServiceRequest) error {
			spanCount := len(req.ResourceSpans[0].ScopeSpans[0].Spans)
			// Fail the first call that has more than 5 spans
			if spanCount > 5 && exhaustedOnce {
				exhaustedOnce = false
				return status.Error(codes.ResourceExhausted, "message too large")
			}
			return nil
		},
	}
	exp := newTestExporter(mock)

	// 10 small spans — fits in one message by size, but mock rejects it
	spans := makeSpans(10, 10)
	if err := exp.exportOTLPSpans(context.Background(), spans); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have: 1 failed attempt + 2 bisected halves = 3 calls
	if len(mock.calls) != 3 {
		t.Fatalf("expected 3 calls (1 rejected + 2 halves), got %d", len(mock.calls))
	}

	// Halves should contain all 10 spans
	total := 0
	for _, call := range mock.calls[1:] { // skip the rejected call
		total += len(call.ResourceSpans[0].ScopeSpans[0].Spans)
	}
	if total != 10 {
		t.Fatalf("expected 10 spans in successful calls, got %d", total)
	}
}

func TestExportOTLPSpans_NonResourceExhaustedFails(t *testing.T) {
	mock := &mockTraceClient{
		errFunc: func(req *collectorpb.ExportTraceServiceRequest) error {
			return status.Error(codes.Unavailable, "server down")
		},
	}
	exp := newTestExporter(mock)

	spans := makeSpans(10, 10)
	err := exp.exportOTLPSpans(context.Background(), spans)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	// Should not retry — only 1 call
	if len(mock.calls) != 1 {
		t.Fatalf("expected 1 call (no retry for non-ResourceExhausted), got %d", len(mock.calls))
	}
}

func TestExportOTLPSpans_SingleSpanResourceExhaustedFails(t *testing.T) {
	mock := &mockTraceClient{
		errFunc: func(req *collectorpb.ExportTraceServiceRequest) error {
			return status.Error(codes.ResourceExhausted, "message too large")
		},
	}
	exp := newTestExporter(mock)

	// Single span can't be bisected — should fail
	spans := makeSpans(1, 10)
	err := exp.exportOTLPSpans(context.Background(), spans)
	if err == nil {
		t.Fatal("expected error for single span that's too large")
	}

	if len(mock.calls) != 1 {
		t.Fatalf("expected 1 call (can't bisect single span), got %d", len(mock.calls))
	}
}

// makeModelSpans builds n model.OpenTelemetrySpan values whose Attributes map
// holds a single payload string of the given size, so we can drive the public
// ExportSpans path with predictable per-span proto sizes.
func makeModelSpans(n int, payloadSize int) []model.OpenTelemetrySpan {
	payload := strings.Repeat("x", payloadSize)
	spans := make([]model.OpenTelemetrySpan, n)
	for i := range spans {
		spans[i] = model.OpenTelemetrySpan{
			Hostname:      "test-host",
			TraceID:       uuid.UUID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, byte(i)},
			SpanID:        uint64(i + 1),
			OperationName: "test.op",
			Kind:          "INTERNAL",
			StartTimeUs:   1_000_000,
			FinishTimeUs:  2_000_000,
			Attributes:    map[string]string{"payload": payload},
		}
	}
	return spans
}

func firstExportedSpan(t *testing.T, mock *mockTraceClient) *tracepb.Span {
	t.Helper()
	if len(mock.calls) != 1 {
		t.Fatalf("Export calls = %d, want 1", len(mock.calls))
	}
	spans := mock.calls[0].ResourceSpans[0].ScopeSpans[0].Spans
	if len(spans) != 1 {
		t.Fatalf("exported spans = %d, want 1", len(spans))
	}
	return spans[0]
}

func attrByKey(attrs []*commonpb.KeyValue, key string) *commonpb.AnyValue {
	for _, attr := range attrs {
		if attr.Key == key {
			return attr.Value
		}
	}
	return nil
}

func attrCount(attrs []*commonpb.KeyValue, key string) int {
	count := 0
	for _, attr := range attrs {
		if attr.Key == key {
			count++
		}
	}
	return count
}

func stringAttrValue(t *testing.T, attrs []*commonpb.KeyValue, key string) string {
	t.Helper()
	value := attrByKey(attrs, key)
	if value == nil {
		t.Fatalf("missing attribute %q", key)
	}
	got, ok := value.Value.(*commonpb.AnyValue_StringValue)
	if !ok {
		t.Fatalf("%s has type %T, want string", key, value.Value)
	}
	return got.StringValue
}

func stringSliceAttrValues(t *testing.T, attrs []*commonpb.KeyValue, key string) []string {
	t.Helper()
	value := attrByKey(attrs, key)
	if value == nil {
		t.Fatalf("missing attribute %q", key)
	}
	got, ok := value.Value.(*commonpb.AnyValue_ArrayValue)
	if !ok {
		t.Fatalf("%s has type %T, want array", key, value.Value)
	}
	values := got.ArrayValue.GetValues()
	out := make([]string, len(values))
	for i, value := range values {
		stringValue, ok := value.Value.(*commonpb.AnyValue_StringValue)
		if !ok {
			t.Fatalf("%s[%d] has type %T, want string", key, i, value.Value)
		}
		out[i] = stringValue.StringValue
	}
	return out
}

func TestExportSpans_LiveSourceAttribute(t *testing.T) {
	tests := []struct {
		name       string
		attributes map[string]string
		wantSource string
	}{
		{
			name: "supplied source is authoritative",
			attributes: map[string]string{
				liveSpanSourceKey: "test-span",
				"custom":          "preserved",
			},
			wantSource: "test-span",
		},
		{
			name:       "absent source defaults to span log",
			attributes: map[string]string{"custom": "preserved"},
			wantSource: liveSpanDefaultSource,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockTraceClient{}
			exp := newTestExporter(mock)
			span := makeModelSpans(1, 1)[0]
			span.Attributes = tt.attributes

			if _, err := exp.ExportSpans(context.Background(), []model.OpenTelemetrySpan{span}); err != nil {
				t.Fatalf("ExportSpans returned err: %v", err)
			}

			attrs := firstExportedSpan(t, mock).Attributes
			if got := attrCount(attrs, liveSpanSourceKey); got != 1 {
				t.Fatalf("%s count = %d, want exactly 1", liveSpanSourceKey, got)
			}
			if got := stringAttrValue(t, attrs, liveSpanSourceKey); got != tt.wantSource {
				t.Fatalf("%s = %q, want %q", liveSpanSourceKey, got, tt.wantSource)
			}
			if got := stringAttrValue(t, attrs, "custom"); got != "preserved" {
				t.Fatalf("custom = %q, want preserved", got)
			}
		})
	}
}

func TestExportSpans_LiveDBStatementTruncation(t *testing.T) {
	tests := []struct {
		name           string
		maxQueryLength int
		query          string
		want           string
	}{
		{name: "boundary", maxQueryLength: 8, query: "SELECT 1", want: "SELECT 1"},
		{name: "over limit", maxQueryLength: 8, query: "SELECT 12", want: "SELECT 1..."},
		{name: "no limit", maxQueryLength: 0, query: "SELECT 12", want: "SELECT 12"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockTraceClient{}
			exp := newTestExporter(mock)
			exp.maxQueryLength = tt.maxQueryLength
			span := makeModelSpans(1, 1)[0]
			span.Attributes = map[string]string{
				liveSpanQueryKey: tt.query,
				"custom":         "unchanged-long-value",
			}

			if _, err := exp.ExportSpans(context.Background(), []model.OpenTelemetrySpan{span}); err != nil {
				t.Fatalf("ExportSpans returned err: %v", err)
			}

			attrs := firstExportedSpan(t, mock).Attributes
			if got := stringAttrValue(t, attrs, liveSpanQueryKey); got != tt.want {
				t.Fatalf("%s = %q, want %q", liveSpanQueryKey, got, tt.want)
			}
			if got := stringAttrValue(t, attrs, "custom"); got != "unchanged-long-value" {
				t.Fatalf("custom = %q, want unchanged-long-value", got)
			}
			if got := span.Attributes[liveSpanQueryKey]; got != tt.query {
				t.Fatalf("input %s mutated to %q, want %q", liveSpanQueryKey, got, tt.query)
			}
		})
	}
}

func TestExportSpans_LargeBatchChunks(t *testing.T) {
	mock := &mockTraceClient{}
	exp := newTestExporter(mock)

	// ~4MB of payload spread across 4000 spans → exceeds grpcMaxBytes (3.5MB),
	// so the streaming chunker should flush at least twice.
	spans := makeModelSpans(4000, 1000)

	result, err := exp.ExportSpans(context.Background(), spans)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Accepted) != len(spans) {
		t.Fatalf("expected %d keys, got %d", len(spans), len(result.Accepted))
	}
	if result.TotalSent != len(spans) || result.TotalAccepted != len(spans) {
		t.Fatalf("result counts = sent %d accepted %d, want %d/%d", result.TotalSent, result.TotalAccepted, len(spans), len(spans))
	}
	if len(result.Sinks) != 1 {
		t.Fatalf("len(Sinks) = %d, want 1", len(result.Sinks))
	}
	if result.Sinks[0].Name != "otel" || result.Sinks[0].Sent != len(spans) || result.Sinks[0].Accepted != len(spans) || result.Sinks[0].Error != nil {
		t.Fatalf("sink status = %+v, want otel sent/accepted=%d with nil error", result.Sinks[0], len(spans))
	}
	if len(mock.calls) < 2 {
		t.Fatalf("expected multiple gRPC calls for oversized batch, got %d", len(mock.calls))
	}

	total := 0
	for _, call := range mock.calls {
		spanCount := len(call.ResourceSpans[0].ScopeSpans[0].Spans)
		total += spanCount
		// Verify the streaming estimator kept each chunk under the wire limit
		// (allowing the small per-span overhead estimate to be conservative).
		if size := proto.Size(call); size > grpcMaxBytes {
			t.Fatalf("chunk size %d exceeded grpcMaxBytes %d (%d spans)", size, grpcMaxBytes, spanCount)
		}
	}
	if total != len(spans) {
		t.Fatalf("expected %d total spans across chunks, got %d", len(spans), total)
	}

	// Keys should preserve order and identity from the input.
	for i, k := range result.Accepted {
		want := model.KeyOf(spans[i])
		if k != want {
			t.Fatalf("key[%d] = %+v, want %+v", i, k, want)
		}
	}
}

func TestExportSpans_EmitsStringSliceAttributes(t *testing.T) {
	mock := &mockTraceClient{}
	exp := newTestExporter(mock)

	spans := []model.OpenTelemetrySpan{{
		Hostname:      "test-host",
		TraceID:       uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		SpanID:        1,
		OperationName: "query",
		Kind:          "CLIENT",
		StartTimeUs:   1_000_000,
		FinishTimeUs:  2_000_000,
		Attributes: map[string]string{
			"query_log.tables_csv":    "default.events,system.numbers",
			"query_log.databases_csv": "default,system",
		},
		StringSliceAttributes: map[string][]string{
			"query_log.tables":    {"default.events", "system.numbers"},
			"query_log.databases": {"default", "system"},
		},
	}}

	result, err := exp.ExportSpans(context.Background(), spans)
	if err != nil {
		t.Fatalf("ExportSpans returned err: %v", err)
	}
	if result.TotalAccepted != 1 {
		t.Fatalf("TotalAccepted = %d, want 1", result.TotalAccepted)
	}

	attrs := firstExportedSpan(t, mock).Attributes
	if got := stringSliceAttrValues(t, attrs, "query_log.tables"); !slices.Equal(got, []string{"default.events", "system.numbers"}) {
		t.Fatalf("query_log.tables = %v, want [default.events system.numbers]", got)
	}
	if got := stringSliceAttrValues(t, attrs, "query_log.databases"); !slices.Equal(got, []string{"default", "system"}) {
		t.Fatalf("query_log.databases = %v, want [default system]", got)
	}
	if got := stringAttrValue(t, attrs, "query_log.tables_csv"); got != "default.events,system.numbers" {
		t.Fatalf("query_log.tables_csv = %q, want default.events,system.numbers", got)
	}
}

func TestExportSpans_FailsFastOnSendError(t *testing.T) {
	mock := &mockTraceClient{
		errFunc: func(req *collectorpb.ExportTraceServiceRequest) error {
			return status.Error(codes.Unavailable, "server down")
		},
	}
	exp := newTestExporter(mock)

	result, err := exp.ExportSpans(context.Background(), makeModelSpans(10, 10))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if result.Accepted != nil {
		t.Fatalf("expected nil keys on error, got %d", len(result.Accepted))
	}
	if result.TotalSent != 10 || result.TotalAccepted != 0 {
		t.Fatalf("result counts = sent %d accepted %d, want 10/0", result.TotalSent, result.TotalAccepted)
	}
	if len(result.Sinks) != 1 {
		t.Fatalf("len(Sinks) = %d, want 1", len(result.Sinks))
	}
	if result.Sinks[0].Name != "otel" || result.Sinks[0].Sent != 10 || result.Sinks[0].Accepted != 0 || result.Sinks[0].Error == nil {
		t.Fatalf("sink status = %+v, want otel sent=10 accepted=0 with error", result.Sinks[0])
	}
}

func TestExportQuery_ResultOnSendError(t *testing.T) {
	mock := &mockTraceClient{
		errFunc: func(req *collectorpb.ExportTraceServiceRequest) error {
			return status.Error(codes.Unavailable, "server down")
		},
	}
	exp := newTestExporter(mock)

	result, err := exp.ExportQuery(context.Background(), model.QueryLog{EventTime: time.Now()})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if result.TotalSent != 1 || result.TotalAccepted != 0 {
		t.Fatalf("result counts = sent %d accepted %d, want 1/0", result.TotalSent, result.TotalAccepted)
	}
	if len(result.Sinks) != 1 {
		t.Fatalf("len(Sinks) = %d, want 1", len(result.Sinks))
	}
	if result.Sinks[0].Name != "otel" || result.Sinks[0].Sent != 1 || result.Sinks[0].Accepted != 0 || result.Sinks[0].Error == nil {
		t.Fatalf("sink status = %+v, want otel sent=1 accepted=0 with error", result.Sinks[0])
	}
}

func TestExportQuery_OmitsAbsentRawStatement(t *testing.T) {
	mock := &mockTraceClient{}
	exp := newTestExporter(mock)
	result, err := exp.ExportQuery(context.Background(), model.QueryLog{
		QueryID:             "q-normalized-only",
		EventTime:           time.Now(),
		NormalizedQueryHash: 123,
		NormalizedQuery:     "SELECT ?",
		ExceptionCode:       62,
	})
	if err != nil {
		t.Fatalf("ExportQuery: %v", err)
	}
	if result.TotalAccepted != 1 {
		t.Fatalf("result = %+v, want one accepted", result)
	}
	span := firstExportedSpan(t, mock)
	if attrByKey(span.Attributes, "db.statement") != nil {
		t.Fatal("OTLP exporter emitted db.statement for a shaped query without raw text")
	}
	if got := stringAttrValue(t, span.Attributes, "db.normalized_query"); got != "SELECT ?" {
		t.Fatalf("db.normalized_query = %q, want SELECT ?", got)
	}
	if attrByKey(span.Attributes, "db.exception_code") == nil {
		t.Fatal("error metadata was lost when raw query text was omitted")
	}
}

// Multi-chunk batch where the first send fails: the streaming chunker must
// stop after the first flush rather than push the rest of the chunks.
// Locks down "stops on error" for the chunked path (the small-batch test
// above only exercises a single-chunk send).
func TestExportSpans_StopsAfterFirstChunkError(t *testing.T) {
	mock := &mockTraceClient{
		errFunc: func(req *collectorpb.ExportTraceServiceRequest) error {
			return status.Error(codes.Unavailable, "server down")
		},
	}
	exp := newTestExporter(mock)

	// Same shape as TestExportSpans_LargeBatchChunks — exceeds grpcMaxBytes,
	// so on the success path it would flush at least twice.
	result, err := exp.ExportSpans(context.Background(), makeModelSpans(4000, 1000))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if result.Accepted != nil {
		t.Fatalf("expected nil keys on error, got %d", len(result.Accepted))
	}
	if len(mock.calls) != 1 {
		t.Fatalf("expected exactly 1 send (fail-fast on first chunk), got %d", len(mock.calls))
	}
}

func TestExportSpans_LiveQueryLogAttributeTypes(t *testing.T) {
	mock := &mockTraceClient{}
	exp := newTestExporter(mock)

	spans := []model.OpenTelemetrySpan{
		{
			Hostname:      "test-host",
			TraceID:       uuid.UUID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
			SpanID:        1,
			OperationName: "query",
			Kind:          "CLIENT",
			StartTimeUs:   1_000_000,
			FinishTimeUs:  2_500_000,
			Attributes: map[string]string{
				"query_log.query_duration_ms":     "1500",
				"query_log.read_rows":             "10000",
				"query_log.read_bytes":            "512000",
				"query_log.written_rows":          "0",
				"query_log.written_bytes":         "0",
				"query_log.result_rows":           "100",
				"query_log.result_bytes":          "4096",
				"query_log.memory_usage":          "2048000",
				"query_log.exception_code":        "62",
				"query_log.user":                  "default",
				"query_log.client_name":           "clickhouse-go",
				"query_log.normalized_query_hash": "123456789",
				"query_log.normalized_query":      "SELECT * FROM events WHERE user_id = ?",
			},
			StringSliceAttributes: map[string][]string{
				"query_log.tables":    {"default.events", "system.numbers"},
				"query_log.databases": {"default", "system"},
			},
		},
	}

	if _, err := exp.ExportSpans(context.Background(), spans); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.calls) != 1 {
		t.Fatalf("expected 1 export call, got %d", len(mock.calls))
	}

	exported := mock.calls[0].ResourceSpans[0].ScopeSpans[0].Spans[0]
	requireIntAttr(t, exported, "query_log.query_duration_ms", 1500)
	requireIntAttr(t, exported, "query_log.read_rows", 10000)
	requireIntAttr(t, exported, "query_log.read_bytes", 512000)
	requireIntAttr(t, exported, "query_log.written_rows", 0)
	requireIntAttr(t, exported, "query_log.written_bytes", 0)
	requireIntAttr(t, exported, "query_log.result_rows", 100)
	requireIntAttr(t, exported, "query_log.result_bytes", 4096)
	requireIntAttr(t, exported, "query_log.memory_usage", 2048000)
	requireIntAttr(t, exported, "query_log.exception_code", 62)

	requireStringAttr(t, exported, "query_log.user", "default")
	requireStringAttr(t, exported, "query_log.client_name", "clickhouse-go")
	requireStringAttr(t, exported, "query_log.normalized_query_hash", "123456789")
	requireStringAttr(t, exported, "query_log.normalized_query", "SELECT * FROM events WHERE user_id = ?")
	if got := stringSliceAttrValues(t, exported.Attributes, "query_log.tables"); !slices.Equal(got, []string{"default.events", "system.numbers"}) {
		t.Fatalf("query_log.tables = %v, want [default.events system.numbers]", got)
	}
	if got := stringSliceAttrValues(t, exported.Attributes, "query_log.databases"); !slices.Equal(got, []string{"default", "system"}) {
		t.Fatalf("query_log.databases = %v, want [default system]", got)
	}
}

func TestExportSpans_LiveQueryLogIntAttrFallback(t *testing.T) {
	mock := &mockTraceClient{}
	exp := newTestExporter(mock)

	spans := []model.OpenTelemetrySpan{
		{
			Hostname:      "test-host",
			TraceID:       uuid.UUID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 17},
			SpanID:        2,
			OperationName: "query",
			Kind:          "CLIENT",
			StartTimeUs:   1_000_000,
			FinishTimeUs:  2_500_000,
			Attributes: map[string]string{
				"query_log.read_rows":    "9223372036854775808",
				"query_log.memory_usage": "not-a-number",
			},
		},
	}

	if _, err := exp.ExportSpans(context.Background(), spans); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	exported := mock.calls[0].ResourceSpans[0].ScopeSpans[0].Spans[0]
	requireStringAttr(t, exported, "query_log.read_rows", "9223372036854775808")
	requireStringAttr(t, exported, "query_log.memory_usage", "not-a-number")
}

func requireAttr(t *testing.T, span *tracepb.Span, key string) *commonpb.AnyValue {
	t.Helper()
	for _, attr := range span.Attributes {
		if attr.Key == key {
			return attr.Value
		}
	}
	t.Fatalf("attribute %q not found", key)
	return nil
}

func requireIntAttr(t *testing.T, span *tracepb.Span, key string, want int64) {
	t.Helper()
	value := requireAttr(t, span, key)
	got, ok := value.Value.(*commonpb.AnyValue_IntValue)
	if !ok {
		t.Fatalf("%s type = %T, want IntValue", key, value.Value)
	}
	if got.IntValue != want {
		t.Fatalf("%s = %d, want %d", key, got.IntValue, want)
	}
}

func requireStringAttr(t *testing.T, span *tracepb.Span, key, want string) {
	t.Helper()
	value := requireAttr(t, span, key)
	got, ok := value.Value.(*commonpb.AnyValue_StringValue)
	if !ok {
		t.Fatalf("%s type = %T, want StringValue", key, value.Value)
	}
	if got.StringValue != want {
		t.Fatalf("%s = %q, want %q", key, got.StringValue, want)
	}
}

func BenchmarkExportSpans_LargeBatch(b *testing.B) {
	mock := &mockTraceClient{}
	exp := newTestExporter(mock)
	spans := makeModelSpans(4000, 1000)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Drop the slice (not just reslice to len 0) so the previous
		// iteration's *ExportTraceServiceRequest pointers can be GC'd
		// before we accumulate the next batch — keeps the alloc numbers
		// honest by not holding prior-iter proto trees live across iters.
		mock.calls = nil
		if _, err := exp.ExportSpans(context.Background(), spans); err != nil {
			b.Fatal(err)
		}
	}
}

func TestDeterministicQueryIDs(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	row := model.QueryLog{QueryID: "q-abc-123", EventTime: base, QueryKind: "Select"}

	// Re-exporting the same row must yield identical, correctly-sized, non-zero
	// IDs so the retry carries the same identifiable (trace_id, span_id).
	tid1, sid1 := deterministicQueryIDs(row)
	tid2, sid2 := deterministicQueryIDs(row)
	if len(tid1) != 16 || len(sid1) != 8 {
		t.Fatalf("wrong ID sizes: trace=%d span=%d (want 16/8)", len(tid1), len(sid1))
	}
	if string(tid1) != string(tid2) || string(sid1) != string(sid2) {
		t.Error("same row produced different IDs — retries would not dedup")
	}
	if isAllZero(tid1) || isAllZero(sid1) {
		t.Error("OTLP IDs must be non-zero")
	}

	// A reused query_id at a different event_time is a DISTINCT execution and
	// must NOT collide — keying on query_id alone would dedup the later
	// historical row away in backfill.
	reused := model.QueryLog{QueryID: "q-abc-123", EventTime: base.Add(time.Hour), QueryKind: "Select"}
	tid3, sid3 := deterministicQueryIDs(reused)
	if string(tid3) == string(tid1) && string(sid3) == string(sid1) {
		t.Error("reused query_id at a different event_time collided — distinct rows would be deduped away")
	}

	// A different query_id at the same time likewise differs.
	other := model.QueryLog{QueryID: "q-different", EventTime: base, QueryKind: "Select"}
	tid4, _ := deterministicQueryIDs(other)
	if string(tid4) == string(tid1) {
		t.Error("distinct query_ids collided onto the same IDs")
	}
}

func isAllZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}
