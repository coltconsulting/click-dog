package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/export"
	"github.com/coltconsulting/click-dog/internal/model"

	"google.golang.org/grpc"

	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// MockOTELCollector implements the OTLP trace service
type MockOTELCollector struct {
	collectortracepb.UnimplementedTraceServiceServer
	mu             sync.Mutex
	receivedSpans  []*tracepb.Span
	receivedTraces []*collectortracepb.ExportTraceServiceRequest
}

func (m *MockOTELCollector) Export(ctx context.Context, req *collectortracepb.ExportTraceServiceRequest) (*collectortracepb.ExportTraceServiceResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.receivedTraces = append(m.receivedTraces, req)

	// Extract spans from the request
	for _, resourceSpan := range req.ResourceSpans {
		for _, scopeSpan := range resourceSpan.ScopeSpans {
			m.receivedSpans = append(m.receivedSpans, scopeSpan.Spans...)
		}
	}

	return &collectortracepb.ExportTraceServiceResponse{}, nil
}

func (m *MockOTELCollector) GetReceivedSpans() []*tracepb.Span {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.receivedSpans
}

func (m *MockOTELCollector) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.receivedSpans = nil
	m.receivedTraces = nil
}

func waitForOTELExporter(t *testing.T, exporter *export.OTELExporter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := exporter.CheckConnectivity(ctx); err != nil {
		t.Fatalf("OTEL test collector was not ready: %v", err)
	}
}

func waitForReceivedSpans(t *testing.T, collector *MockOTELCollector, want int) []*tracepb.Span {
	t.Helper()
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		spans := collector.GetReceivedSpans()
		if len(spans) >= want {
			return spans
		}
		select {
		case <-timeout.C:
			t.Fatalf("timed out waiting for at least %d spans, got %d", want, len(spans))
		case <-tick.C:
		}
	}
}

func assertNoReceivedSpans(t *testing.T, collector *MockOTELCollector) {
	t.Helper()
	timeout := time.NewTimer(100 * time.Millisecond)
	defer timeout.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		spans := collector.GetReceivedSpans()
		if len(spans) != 0 {
			t.Fatalf("expected no spans, got %d", len(spans))
		}
		select {
		case <-timeout.C:
			return
		case <-tick.C:
		}
	}
}

// startMockOTELServer starts a mock OTLP gRPC server and returns the address and cleanup function
func startMockOTELServer(t *testing.T) (*MockOTELCollector, string, func()) {
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("Failed to listen: %v", err)
	}

	server := grpc.NewServer()
	mockCollector := &MockOTELCollector{}
	collectortracepb.RegisterTraceServiceServer(server, mockCollector)

	go func() {
		if err := server.Serve(listener); err != nil {
			t.Logf("Server error: %v", err)
		}
	}()

	cleanup := func() {
		server.Stop()
		_ = listener.Close()
	}

	return mockCollector, listener.Addr().String(), cleanup
}

func TestOTELExporter_ExportQuery(t *testing.T) {
	// Start mock OTEL collector
	mockCollector, addr, cleanup := startMockOTELServer(t)
	defer cleanup()

	// Create OTEL exporter pointing to mock server
	exporter, err := export.NewOTELExporter(config.OTELConfig{
		CollectorAddress: addr,
		ServiceName:      "test-service",
		MaxQueryLength:   100000, // Must set this, 0 means truncate to 0 chars
	})
	if err != nil {
		t.Fatalf("Failed to create OTEL exporter: %v", err)
	}
	defer func() { _ = exporter.Close(context.Background()) }()
	waitForOTELExporter(t, exporter)

	// Create test query log
	testTime := time.Now()
	queryLog := model.QueryLog{
		QueryID:             "test-query-123",
		QueryKind:           "QueryFinish",
		EventTime:           testTime,
		QueryDurationMs:     1500,
		Query:               "SELECT * FROM users WHERE id = 1",
		NormalizedQueryHash: 424242,
		NormalizedQuery:     "SELECT * FROM users WHERE id = ?",
		User:                "test_user",
		ClientName:          "clickhouse-client",
		ClientHostname:      "test-host",
		ClientAddress:       "192.168.1.100",
		DatabasesVisited:    []string{"default", "analytics"},
		TablesVisited:       []string{"users", "events"},
		ExceptionCode:       0,
		ReadRows:            1000,
		ReadBytes:           50000,
		WrittenRows:         0,
		WrittenBytes:        0,
		ResultRows:          1,
		ResultBytes:         256,
		MemoryUsage:         1024000,
	}

	// Export the query
	ctx := context.Background()
	result, err := exporter.ExportQuery(ctx, queryLog)
	if err != nil {
		t.Fatalf("Failed to export query: %v", err)
	}
	if result.TotalSent != 1 || result.TotalAccepted != 1 {
		t.Fatalf("export result counts = sent %d accepted %d, want 1/1", result.TotalSent, result.TotalAccepted)
	}

	// Export is synchronous (direct gRPC), no flush needed

	// Verify we received the span
	spans := waitForReceivedSpans(t, mockCollector, 1)
	if len(spans) == 0 {
		t.Fatal("No spans received by mock collector")
	}

	span := spans[0]

	// Verify span name
	if span.Name != "clickhouse.query" {
		t.Errorf("Expected span name 'clickhouse.query', got '%s'", span.Name)
	}

	// Verify span timing
	expectedStartTime := testTime.Add(-1500 * time.Millisecond).UnixNano()
	expectedEndTime := testTime.UnixNano()

	if span.StartTimeUnixNano != uint64(expectedStartTime) {
		t.Errorf("Expected start time %d, got %d", expectedStartTime, span.StartTimeUnixNano)
	}
	if span.EndTimeUnixNano != uint64(expectedEndTime) {
		t.Errorf("Expected end time %d, got %d", expectedEndTime, span.EndTimeUnixNano)
	}

	// Verify span attributes
	attrs := attributesToMap(span.Attributes)

	expectedAttrs := map[string]interface{}{
		"db.system":                "clickhouse",
		"db.statement":             "SELECT * FROM users WHERE id = 1",
		"db.user":                  "test_user",
		"db.query_id":              "test-query-123",
		"db.query_kind":            "QueryFinish",
		"db.query_duration_ms":     int64(1500),
		"db.normalized_query_hash": "424242",
		"db.normalized_query":      "SELECT * FROM users WHERE id = ?",
		"client.name":              "clickhouse-client",
		"client.hostname":          "test-host",
		"client.address":           "192.168.1.100",
		"db.read_rows":             int64(1000),
		"db.read_bytes":            int64(50000),
		"db.written_rows":          int64(0),
		"db.written_bytes":         int64(0),
		"db.result_rows":           int64(1),
		"db.result_bytes":          int64(256),
		"db.memory_usage":          int64(1024000),
	}

	for key, expectedValue := range expectedAttrs {
		actualValue, exists := attrs[key]
		if !exists {
			t.Errorf("Expected attribute '%s' not found", key)
			continue
		}
		if actualValue != expectedValue {
			t.Errorf("Attribute '%s': expected %v, got %v", key, expectedValue, actualValue)
		}
	}

	// Verify array attributes
	databases := getStringArrayAttribute(span.Attributes, "db.databases")
	if len(databases) != 2 || databases[0] != "default" || databases[1] != "analytics" {
		t.Errorf("Expected databases ['default', 'analytics'], got %v", databases)
	}

	tables := getStringArrayAttribute(span.Attributes, "db.tables")
	if len(tables) != 2 || tables[0] != "users" || tables[1] != "events" {
		t.Errorf("Expected tables ['users', 'events'], got %v", tables)
	}
}

func TestOTELExporter_ExportQueryWithError(t *testing.T) {
	// Start mock OTEL collector
	mockCollector, addr, cleanup := startMockOTELServer(t)
	defer cleanup()

	exporter, err := export.NewOTELExporter(config.OTELConfig{
		CollectorAddress: addr,
		ServiceName:      "test-service",
	})
	if err != nil {
		t.Fatalf("Failed to create OTEL exporter: %v", err)
	}
	defer func() { _ = exporter.Close(context.Background()) }()
	waitForOTELExporter(t, exporter)

	// Create test query log with error
	testTime := time.Now()
	queryLog := model.QueryLog{
		QueryID:         "test-error-query",
		QueryKind:       "ExceptionWhileProcessing",
		EventTime:       testTime,
		QueryDurationMs: 500,
		Query:           "SELECT * FROM nonexistent_table",
		User:            "test_user",
		ClientAddress:   "127.0.0.1",
		ExceptionCode:   60, // UNKNOWN_TABLE
		ReadRows:        0,
		ReadBytes:       0,
	}

	ctx := context.Background()
	_, err = exporter.ExportQuery(ctx, queryLog)
	if err != nil {
		t.Fatalf("Failed to export query: %v", err)
	}

	spans := waitForReceivedSpans(t, mockCollector, 1)
	if len(spans) == 0 {
		t.Fatal("No spans received by mock collector")
	}

	span := spans[0]
	attrs := attributesToMap(span.Attributes)

	// Verify error attributes
	if errorAttr, exists := attrs["error"]; !exists || errorAttr != true {
		t.Errorf("Expected error attribute to be true, got %v", errorAttr)
	}

	if exceptionCode, exists := attrs["db.exception_code"]; !exists || exceptionCode != int64(60) {
		t.Errorf("Expected exception_code 60, got %v", exceptionCode)
	}
	if _, exists := attrs["db.normalized_query_hash"]; exists {
		t.Error("db.normalized_query_hash should be omitted when absent")
	}
	if _, exists := attrs["db.normalized_query"]; exists {
		t.Error("db.normalized_query should be omitted when absent")
	}
}

func TestOTELExporter_LongQueryTruncation(t *testing.T) {
	mockCollector, addr, cleanup := startMockOTELServer(t)
	defer cleanup()

	exporter, err := export.NewOTELExporter(config.OTELConfig{
		CollectorAddress: addr,
		ServiceName:      "test-service",
		MaxQueryLength:   1000, // Set to test truncation behavior
	})
	if err != nil {
		t.Fatalf("Failed to create OTEL exporter: %v", err)
	}
	defer func() { _ = exporter.Close(context.Background()) }()
	waitForOTELExporter(t, exporter)

	// Create a very long query
	longQuery := "SELECT * FROM users WHERE "
	for i := 0; i < 200; i++ {
		longQuery += fmt.Sprintf("id = %d OR ", i)
	}
	longQuery += "id = 999"

	queryLog := model.QueryLog{
		QueryID:             "long-query",
		QueryKind:           "QueryFinish",
		EventTime:           time.Now(),
		QueryDurationMs:     1000,
		Query:               longQuery,
		NormalizedQueryHash: 999,
		NormalizedQuery:     longQuery,
		User:                "test_user",
		ClientAddress:       "127.0.0.1",
	}

	ctx := context.Background()
	_, err = exporter.ExportQuery(ctx, queryLog)
	if err != nil {
		t.Fatalf("Failed to export query: %v", err)
	}

	spans := waitForReceivedSpans(t, mockCollector, 1)
	if len(spans) == 0 {
		t.Fatal("No spans received")
	}

	attrs := attributesToMap(spans[0].Attributes)
	statement := attrs["db.statement"].(string)

	// Verify truncation occurred
	if len(statement) > 1003 { // 1000 + "..."
		t.Errorf("Expected statement to be truncated to ~1000 chars, got %d", len(statement))
	}

	if len(statement) > 1000 && statement[len(statement)-3:] != "..." {
		t.Error("Expected truncated statement to end with '...'")
	}
	normalized := attrs["db.normalized_query"].(string)
	if len(normalized) > 1003 {
		t.Errorf("Expected normalized query to be truncated to ~1000 chars, got %d", len(normalized))
	}
	if len(normalized) > 1000 && normalized[len(normalized)-3:] != "..." {
		t.Error("Expected truncated normalized query to end with '...'")
	}
}

// Helper functions

func attributesToMap(attrs []*commonpb.KeyValue) map[string]interface{} {
	result := make(map[string]interface{})
	for _, attr := range attrs {
		switch v := attr.Value.Value.(type) {
		case *commonpb.AnyValue_StringValue:
			result[attr.Key] = v.StringValue
		case *commonpb.AnyValue_IntValue:
			result[attr.Key] = v.IntValue
		case *commonpb.AnyValue_BoolValue:
			result[attr.Key] = v.BoolValue
		case *commonpb.AnyValue_DoubleValue:
			result[attr.Key] = v.DoubleValue
		}
	}
	return result
}

func getStringArrayAttribute(attrs []*commonpb.KeyValue, key string) []string {
	for _, attr := range attrs {
		if attr.Key == key {
			if arrayValue, ok := attr.Value.Value.(*commonpb.AnyValue_ArrayValue); ok {
				var result []string
				for _, val := range arrayValue.ArrayValue.Values {
					if strVal, ok := val.Value.(*commonpb.AnyValue_StringValue); ok {
						result = append(result, strVal.StringValue)
					}
				}
				return result
			}
		}
	}
	return nil
}

func TestOTELExporter_ExportSpans(t *testing.T) {
	// Start mock OTEL collector
	mockCollector, addr, cleanup := startMockOTELServer(t)
	defer cleanup()

	exporter, err := export.NewOTELExporter(config.OTELConfig{
		CollectorAddress: addr,
		ServiceName:      "test-service",
	})
	if err != nil {
		t.Fatalf("Failed to create OTEL exporter: %v", err)
	}
	defer func() { _ = exporter.Close(context.Background()) }()
	waitForOTELExporter(t, exporter)

	// Create test OpenTelemetry span
	now := time.Now()
	startTimeUs := uint64(now.Add(-1500 * time.Millisecond).UnixMicro())
	finishTimeUs := uint64(now.UnixMicro())

	otelSpan := model.OpenTelemetrySpan{
		Hostname:      "test-host",
		TraceID:       uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
		SpanID:        12345,
		ParentSpanID:  0,
		OperationName: "SELECT users",
		Kind:          "INTERNAL",
		StartTimeUs:   startTimeUs,
		FinishTimeUs:  finishTimeUs,
		FinishDate:    now,
		Attributes: map[string]string{
			"db.statement":     "SELECT * FROM users WHERE id = 1",
			"client.address":   "192.168.1.100",
			"db.user":          "test_user",
			"custom.attribute": "test_value",
		},
	}

	// Export the span
	ctx := context.Background()
	_, err = exporter.ExportSpans(ctx, []model.OpenTelemetrySpan{otelSpan})
	if err != nil {
		t.Fatalf("Failed to export span: %v", err)
	}

	// Verify we received the span (export is synchronous, no flush needed)
	spans := waitForReceivedSpans(t, mockCollector, 1)
	if len(spans) == 0 {
		t.Fatal("No spans received by mock collector")
	}

	span := spans[0]

	// Verify span name
	expectedName := "SELECT users"
	if span.Name != expectedName {
		t.Errorf("Expected span name '%s', got '%s'", expectedName, span.Name)
	}

	// Verify span uses actual OTLP trace/span ID fields (not attributes)
	// TraceID should be 16 bytes
	if len(span.TraceId) != 16 {
		t.Errorf("Expected trace ID to be 16 bytes, got %d", len(span.TraceId))
	}

	// SpanID should be 8 bytes
	if len(span.SpanId) != 8 {
		t.Errorf("Expected span ID to be 8 bytes, got %d", len(span.SpanId))
	}

	// Verify custom attributes were preserved
	attrs := attributesToMap(span.Attributes)

	if dbStmt, exists := attrs["db.statement"]; !exists || dbStmt != "SELECT * FROM users WHERE id = 1" {
		t.Errorf("Expected db.statement attribute, got %v", dbStmt)
	}

	if customAttr, exists := attrs["custom.attribute"]; !exists || customAttr != "test_value" {
		t.Errorf("Expected custom.attribute, got %v", customAttr)
	}

	// Verify hostname
	if hostname, exists := attrs["hostname"]; !exists || hostname != "test-host" {
		t.Errorf("Expected hostname attribute, got %v", hostname)
	}
}

func TestOTELExporter_ExportSpansWithParent(t *testing.T) {
	mockCollector, addr, cleanup := startMockOTELServer(t)
	defer cleanup()

	exporter, err := export.NewOTELExporter(config.OTELConfig{
		CollectorAddress: addr,
		ServiceName:      "test-service",
	})
	if err != nil {
		t.Fatalf("Failed to create OTEL exporter: %v", err)
	}
	defer func() { _ = exporter.Close(context.Background()) }()
	waitForOTELExporter(t, exporter)

	now := time.Now()

	// Create span with parent
	otelSpan := model.OpenTelemetrySpan{
		Hostname:      "test-host",
		TraceID:       uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
		SpanID:        67890,
		ParentSpanID:  12345, // Has parent
		OperationName: "SELECT orders",
		Kind:          "CLIENT",
		StartTimeUs:   uint64(now.Add(-500 * time.Millisecond).UnixMicro()),
		FinishTimeUs:  uint64(now.UnixMicro()),
		FinishDate:    now,
		Attributes:    map[string]string{},
	}

	ctx := context.Background()
	_, err = exporter.ExportSpans(ctx, []model.OpenTelemetrySpan{otelSpan})
	if err != nil {
		t.Fatalf("Failed to export span: %v", err)
	}

	spans := waitForReceivedSpans(t, mockCollector, 1)
	if len(spans) == 0 {
		t.Fatal("No spans received")
	}

	// Verify parent span ID is set in the OTLP parent span ID field
	parentSpanIDBytes := spans[0].ParentSpanId
	if len(parentSpanIDBytes) != 8 {
		t.Fatalf("Expected parent span ID to be 8 bytes, got %d", len(parentSpanIDBytes))
	}

	// Convert bytes back to uint64 and verify
	parentSpanID := binary.BigEndian.Uint64(parentSpanIDBytes)
	if parentSpanID != 12345 {
		t.Errorf("Expected parent span ID 12345, got %d", parentSpanID)
	}
}

// TestOTELExporter_TraceIDBytePassthrough verifies that the 16 raw bytes of
// the TraceID flow unchanged from the model into the OTLP Span.TraceId field.
// Covers the well-formed and all-zero cases called out in issue #62. A mismatch
// here indicates a regression in the trace_id pipeline.
func TestOTELExporter_TraceIDBytePassthrough(t *testing.T) {
	cases := []struct {
		name    string
		traceID uuid.UUID
	}{
		{name: "well-formed UUID", traceID: uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")},
		{name: "all-zero UUID", traceID: uuid.Nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mockCollector, addr, cleanup := startMockOTELServer(t)
			defer cleanup()

			exporter, err := export.NewOTELExporter(config.OTELConfig{
				CollectorAddress: addr,
				ServiceName:      "trace-id-passthrough",
			})
			if err != nil {
				t.Fatalf("Failed to create OTEL exporter: %v", err)
			}
			defer func() { _ = exporter.Close(context.Background()) }()
			waitForOTELExporter(t, exporter)

			now := time.Now()
			otelSpan := model.OpenTelemetrySpan{
				TraceID:       tc.traceID,
				SpanID:        1,
				OperationName: "trace-id-check",
				Kind:          "INTERNAL",
				StartTimeUs:   uint64(now.Add(-1 * time.Second).UnixMicro()),
				FinishTimeUs:  uint64(now.UnixMicro()),
				FinishDate:    now,
				Attributes:    map[string]string{},
			}

			if _, err := exporter.ExportSpans(context.Background(), []model.OpenTelemetrySpan{otelSpan}); err != nil {
				t.Fatalf("ExportSpans failed: %v", err)
			}

			received := waitForReceivedSpans(t, mockCollector, 1)
			if len(received) != 1 {
				t.Fatalf("expected 1 span, got %d", len(received))
			}
			got := received[0].TraceId
			want := tc.traceID[:]
			if !bytes.Equal(got, want) {
				t.Errorf("TraceId bytes mismatch:\n got  %x\n want %x", got, want)
			}
		})
	}
}
