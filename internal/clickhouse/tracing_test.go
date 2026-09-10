package clickhouse

import (
	"context"
	"errors"
	"strings"
	"testing"

	oteltrace "go.opentelemetry.io/otel/trace"
)

func TestExecuteTracingTestQuery_UsesConstantCanary(t *testing.T) {
	conn := &fakeConn{row: fakeRow{count: 1}}
	reader := &ClickHouseReader{conn: conn}
	traceID, _ := oteltrace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	spanID, _ := oteltrace.SpanIDFromHex("0123456789abcdef")
	spanContext := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: oteltrace.FlagsSampled,
	})

	if err := reader.ExecuteTracingTestQuery(context.Background(), spanContext, "click-dog-test-fixed"); err != nil {
		t.Fatalf("ExecuteTracingTestQuery: %v", err)
	}
	if conn.lastQueryRow != "SELECT 1 /* click-dog test tracing */" {
		t.Errorf("query = %q, want exact tracing canary", conn.lastQueryRow)
	}
}

func TestExecuteTracingTestQuery_ReportsQueryAndResultFailures(t *testing.T) {
	tests := []struct {
		name string
		row  fakeRow
		want string
	}{
		{name: "query error", row: fakeRow{err: errors.New("ACCESS_DENIED")}, want: "ACCESS_DENIED"},
		{name: "unexpected result", row: fakeRow{count: 2}, want: "returned 2, want 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := &ClickHouseReader{conn: &fakeConn{row: tt.row}}
			err := reader.ExecuteTracingTestQuery(context.Background(), oteltrace.SpanContext{}, "click-dog-test-fixed")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}
