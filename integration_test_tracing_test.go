//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	chreader "github.com/coltconsulting/click-dog/internal/clickhouse"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/model"
)

type tracingIntegrationExporter struct {
	spans  []model.OpenTelemetrySpan
	closed bool
}

func (e *tracingIntegrationExporter) ExportSpans(_ context.Context, spans []model.OpenTelemetrySpan) (model.ExportResult, error) {
	e.spans = append([]model.OpenTelemetrySpan(nil), spans...)
	keys := make([]model.SpanKey, len(spans))
	for i := range spans {
		keys[i] = model.KeyOf(spans[i])
	}
	return model.ExportResult{Accepted: keys, TotalSent: len(spans), TotalAccepted: len(spans)}, nil
}

func (e *tracingIntegrationExporter) ExportQuery(context.Context, model.QueryLog) (model.ExportResult, error) {
	return model.ExportResult{}, nil
}

func (e *tracingIntegrationExporter) Close(context.Context) error {
	e.closed = true
	return nil
}

func TestTracingCommand_NativeProtocolPropagationIntegration(t *testing.T) {
	probeConn := waitForClickHouse(t, 1, 30*time.Second)
	defer func() { _ = probeConn.Close() }()

	var spanLogTables uint64
	if err := probeConn.QueryRow(context.Background(), `
		SELECT count()
		FROM system.tables
		WHERE database = 'system' AND name = 'opentelemetry_span_log'
	`).Scan(&spanLogTables); err != nil {
		t.Fatalf("probe system.opentelemetry_span_log: %v", err)
	}
	if spanLogTables == 0 {
		t.Skip("integration environment does not expose system.opentelemetry_span_log")
	}

	cfg := &config.Config{
		ClickHouse: config.ClickHouseConfig{
			Host:           integrationCHHost(1),
			Port:           integrationCHPort(1),
			Database:       "default",
			Username:       "default",
			MaxOpenConns:   2,
			MaxIdleConns:   1,
			QueryTimeoutS:  30,
			MaxMemoryUsage: 104857600,
		},
		Monitor: config.MonitorConfig{ExportTimeoutS: 5},
	}
	identity, err := generateTracingIdentity()
	if err != nil {
		t.Fatal(err)
	}
	recorder := &tracingIntegrationExporter{}
	deps := productionTracingTestDependencies()
	deps.newReader = func(ctx context.Context, cfg *config.Config) (tracingTestReader, error) {
		return chreader.NewTracingTestReader(ctx, cfg.ClickHouse)
	}
	deps.buildExporters = func(*config.Config) []builtExporter {
		return []builtExporter{{Label: "recording[0]", Endpoint: "memory", Exporter: recorder}}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out bytes.Buffer
	if code := executeTracingTest(ctx, cfg, identity, deps, &out); code != 0 {
		t.Fatalf("tracing command exit = %d:\n%s", code, out.String())
	}
	if len(recorder.spans) < 2 {
		t.Fatalf("exported %d spans, want a local parent and at least one native child", len(recorder.spans))
	}
	parent := recorder.spans[0]
	if parent.TraceID != identity.TraceID || parent.SpanID != identity.SpanID {
		t.Fatalf("local parent identity changed: got %s/%d want %s/%d", parent.TraceID, parent.SpanID, identity.TraceID, identity.SpanID)
	}
	linkedNative := false
	for i, span := range recorder.spans[1:] {
		if span.TraceID != identity.TraceID {
			t.Errorf("native span %d trace_id=%s, want %s", i, span.TraceID, identity.TraceID)
		}
		if span.ParentSpanID == identity.SpanID {
			linkedNative = true
		}
		if span.Attributes["click_dog.source"] != "span_log" {
			t.Errorf("native span %d source=%q, want span_log", i, span.Attributes["click_dog.source"])
		}
	}
	if !linkedNative {
		t.Fatalf("no exported native span retained parent_span_id=%d", identity.SpanID)
	}
	if !recorder.closed {
		t.Error("recording exporter was not closed")
	}
	for _, want := range []string{
		fmt.Sprintf("trace_id=%s", identity.TraceID),
		"Parent propagation:       PASS",
		"Result: PASS",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}
