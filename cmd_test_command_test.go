package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/model"
)

type testCommandExporter struct {
	err           error
	acceptedCount int
	spans         []model.OpenTelemetrySpan
	closed        bool
}

func (e *testCommandExporter) ExportSpans(_ context.Context, spans []model.OpenTelemetrySpan) (model.ExportResult, error) {
	e.spans = append([]model.OpenTelemetrySpan(nil), spans...)
	if e.err != nil {
		return model.ExportResult{}, e.err
	}
	acceptedCount := e.acceptedCount
	if acceptedCount < 0 || acceptedCount > len(spans) {
		acceptedCount = len(spans)
	}
	accepted := make([]model.SpanKey, acceptedCount)
	for i := range accepted {
		accepted[i] = model.KeyOf(spans[i])
	}
	return model.ExportResult{Accepted: accepted, TotalSent: len(spans), TotalAccepted: len(accepted)}, nil
}

func (e *testCommandExporter) ExportQuery(context.Context, model.QueryLog) (model.ExportResult, error) {
	return model.ExportResult{}, nil
}

func (e *testCommandExporter) Close(context.Context) error {
	e.closed = true
	return nil
}

type fakeTracingTestReader struct {
	queryErr       error
	fetchErr       error
	fetchResponses [][]model.OpenTelemetrySpan
	fetchCalls     int
	traceIDs       [][]string
	lookbackDays   []int
	limits         []int
	blacklists     [][]string
	gotSpanContext oteltrace.SpanContext
	gotQueryID     string
	closed         bool
}

func (r *fakeTracingTestReader) ExecuteTracingTestQuery(_ context.Context, spanContext oteltrace.SpanContext, queryID string) error {
	r.gotSpanContext = spanContext
	r.gotQueryID = queryID
	return r.queryErr
}

func (r *fakeTracingTestReader) FetchSpansForTraceIDs(_ context.Context, traceIDs []string, lookbackDays, limit int, blacklist []string) ([]model.OpenTelemetrySpan, error) {
	r.fetchCalls++
	r.traceIDs = append(r.traceIDs, append([]string(nil), traceIDs...))
	r.lookbackDays = append(r.lookbackDays, lookbackDays)
	r.limits = append(r.limits, limit)
	r.blacklists = append(r.blacklists, append([]string(nil), blacklist...))
	if r.fetchErr != nil {
		return nil, r.fetchErr
	}
	if len(r.fetchResponses) == 0 {
		return nil, nil
	}
	idx := r.fetchCalls - 1
	if idx >= len(r.fetchResponses) {
		idx = len(r.fetchResponses) - 1
	}
	return r.fetchResponses[idx], nil
}

func (r *fakeTracingTestReader) Close() error {
	r.closed = true
	return nil
}

func fixedTracingIdentity() tracingIdentity {
	traceID := uuid.MustParse("01234567-89ab-cdef-0123-456789abcdef")
	var otelTraceID oteltrace.TraceID
	copy(otelTraceID[:], traceID[:])
	spanID := oteltrace.SpanID{0, 0, 0, 0, 0, 0, 0, 42}
	return tracingIdentity{
		TraceID: traceID,
		SpanID:  42,
		SpanContext: oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
			TraceID:    otelTraceID,
			SpanID:     spanID,
			TraceFlags: oteltrace.FlagsSampled,
		}),
		QueryID: "click-dog-test-fixed",
	}
}

func tracingTestDeps(reader tracingTestReader, exporters []builtExporter) tracingTestDependencies {
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	nowCalls := 0
	return tracingTestDependencies{
		newReader: func(context.Context, *config.Config) (tracingTestReader, error) {
			return reader, nil
		},
		buildExporters: func(*config.Config) []builtExporter { return exporters },
		wait:           func(context.Context, time.Duration) error { return nil },
		now: func() time.Time {
			t := base.Add(time.Duration(nowCalls) * 100 * time.Millisecond)
			nowCalls++
			return t
		},
	}
}

func nativeTracingChild(identity tracingIdentity) model.OpenTelemetrySpan {
	return model.OpenTelemetrySpan{
		Hostname:      "clickhouse-1",
		TraceID:       identity.TraceID,
		SpanID:        9001,
		ParentSpanID:  identity.SpanID,
		OperationName: "Query",
		Kind:          "SERVER",
		StartTimeUs:   100,
		FinishTimeUs:  200,
		FinishDate:    time.Date(2026, 8, 4, 12, 0, 1, 0, time.UTC),
		Attributes:    map[string]string{"clickhouse.query_id": identity.QueryID, "native": "kept"},
		StringSliceAttributes: map[string][]string{
			"db.namespace": {"default"},
		},
	}
}

func TestRunTest_DispatchHelpAndInvalidUsage(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
		wantErr  string
	}{
		{name: "parent help", wantCode: 0, wantOut: "Subcommands:"},
		{name: "explicit help", args: []string{"-h"}, wantCode: 0, wantOut: "tracing    Run a traced ClickHouse query"},
		{name: "unknown", args: []string{"wat"}, wantCode: 2, wantErr: `unknown command "wat"`},
		{name: "export invalid flag", args: []string{"export", "-timeout", "1s"}, wantCode: 2, wantErr: "flag provided but not defined"},
		{name: "tracing invalid timeout", args: []string{"tracing", "-timeout", "0s"}, wantCode: 2, wantErr: "must be greater than zero"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := runTest(tt.args, &out, &errOut); code != tt.wantCode {
				t.Fatalf("exit = %d, want %d; out=%q err=%q", code, tt.wantCode, out.String(), errOut.String())
			}
			if tt.wantOut != "" && !strings.Contains(out.String(), tt.wantOut) {
				t.Errorf("stdout = %q, want %q", out.String(), tt.wantOut)
			}
			if tt.wantErr != "" && !strings.Contains(errOut.String(), tt.wantErr) {
				t.Errorf("stderr = %q, want %q", errOut.String(), tt.wantErr)
			}
		})
	}
}

func TestRootHelpPromotesTestCommandFamily(t *testing.T) {
	var out bytes.Buffer
	printUsage(&out)
	for _, want := range []string{"click-dog test <subcommand>", "Deprecated alias for 'click-dog test export'", "click-dog test tracing -config"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("root help missing %q:\n%s", want, out.String())
		}
	}
}

func TestTestSpanAlias_ParityAndDeprecationNotice(t *testing.T) {
	_, addr, cleanup := startMockOTELServer(t)
	defer cleanup()

	configPath := filepath.Join(t.TempDir(), "click-dog.yaml")
	configBody := fmt.Sprintf(`clickhouse:
  host: localhost
  port: 9000
exporters:
  otel:
    - collector_address: %s
      service_name: test-command
monitor:
  enabled: true
  min_trace_duration_ms: 1000
  export_timeout_s: 5
`, addr)
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}

	var canonicalOut, canonicalErr bytes.Buffer
	canonicalCode := runTestExport([]string{"-config", configPath}, &canonicalOut, &canonicalErr)
	var aliasOut, aliasErr bytes.Buffer
	aliasCode := runTestSpan([]string{"-config", configPath}, &aliasOut, &aliasErr)

	if canonicalCode != 0 || aliasCode != canonicalCode {
		t.Fatalf("canonical/alias exits = %d/%d; canonical=%s alias=%s", canonicalCode, aliasCode, canonicalOut.String(), aliasOut.String())
	}
	for _, output := range []string{canonicalOut.String(), aliasOut.String()} {
		for _, want := range []string{"Exporter test", "PASS (accepted 1 span", "Result: PASS"} {
			if !strings.Contains(output, want) {
				t.Errorf("output missing %q:\n%s", want, output)
			}
		}
	}
	if canonicalErr.Len() != 0 {
		t.Errorf("canonical stderr = %q, want empty", canonicalErr.String())
	}
	if !strings.Contains(aliasErr.String(), "deprecated") || !strings.Contains(aliasErr.String(), "click-dog test export") {
		t.Errorf("alias stderr missing replacement notice: %q", aliasErr.String())
	}
}

func TestExportTestBatch_ReportsPartialFailureAndCleansUp(t *testing.T) {
	good := &testCommandExporter{acceptedCount: -1}
	bad := &testCommandExporter{err: errors.New("rejected")}
	partial := &testCommandExporter{acceptedCount: 0}
	initErr := errors.New("bad token")
	exporters := []builtExporter{
		{Label: "OTEL[0]", Endpoint: "good", Exporter: good},
		{Label: "OTEL[1]", Endpoint: "bad", Exporter: bad},
		{Label: "OTEL[2]", Endpoint: "partial", Exporter: partial},
		{Label: "SplunkHEC[0]", Endpoint: "init", InitErr: initErr},
	}
	span := model.OpenTelemetrySpan{TraceID: uuid.New(), SpanID: 1}
	var out bytes.Buffer
	if exportTestBatch(context.Background(), &config.Config{}, exporters, []model.OpenTelemetrySpan{span}, &out) {
		t.Fatal("partial exporter failure must fail the batch")
	}
	for _, want := range []string{"OTEL[0]:", "OTEL[1]:", "OTEL[2]:", "SplunkHEC[0]:", "PASS", "rejected", "accepted 0/1", "bad token"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
	if !good.closed || !bad.closed || !partial.closed {
		t.Errorf("initialized exporters were not all closed: good=%v bad=%v partial=%v", good.closed, bad.closed, partial.closed)
	}
}

func TestExportTestBatch_NoExportersFails(t *testing.T) {
	var out bytes.Buffer
	if exportTestBatch(context.Background(), &config.Config{}, nil, []model.OpenTelemetrySpan{{TraceID: uuid.New(), SpanID: 1}}, &out) {
		t.Fatal("zero exporters must not report a successful batch")
	}
	if !strings.Contains(out.String(), "FAIL (no exporters configured)") {
		t.Errorf("output missing zero-exporter failure:\n%s", out.String())
	}
}

func TestExecuteTracingTest_SuccessPreservesIdentityAndProvenance(t *testing.T) {
	identity := fixedTracingIdentity()
	child := nativeTracingChild(identity)
	originalAttrs := cloneStringMap(child.Attributes)
	reader := &fakeTracingTestReader{fetchResponses: [][]model.OpenTelemetrySpan{{}, {child}}}
	exporter := &testCommandExporter{acceptedCount: -1}
	deps := tracingTestDeps(reader, []builtExporter{{Label: "OTEL[0]", Endpoint: "collector:4317", Exporter: exporter}})
	var waits []time.Duration
	deps.wait = func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	}

	var out bytes.Buffer
	if code := executeTracingTest(context.Background(), &config.Config{}, identity, deps, &out); code != 0 {
		t.Fatalf("exit = %d, want 0:\n%s", code, out.String())
	}
	if reader.gotQueryID != identity.QueryID || reader.gotSpanContext.TraceID() != identity.SpanContext.TraceID() || !reader.gotSpanContext.IsSampled() {
		t.Errorf("query identity not preserved: query=%q span=%v", reader.gotQueryID, reader.gotSpanContext)
	}
	if reader.fetchCalls != 2 || len(waits) != 1 || waits[0] != tracingPollInitialDelay {
		t.Errorf("poll calls/waits = %d/%v, want 2/[100ms]", reader.fetchCalls, waits)
	}
	for i := range reader.traceIDs {
		if len(reader.traceIDs[i]) != 1 || reader.traceIDs[i][0] != identity.TraceID.String() || reader.lookbackDays[i] != 1 || reader.limits[i] != 1000 || len(reader.blacklists[i]) != 0 {
			t.Errorf("fetch[%d] = ids=%v days=%d limit=%d blacklist=%v", i, reader.traceIDs[i], reader.lookbackDays[i], reader.limits[i], reader.blacklists[i])
		}
	}
	if len(exporter.spans) != 2 {
		t.Fatalf("exported %d spans, want parent + child", len(exporter.spans))
	}
	parent, gotChild := exporter.spans[0], exporter.spans[1]
	if parent.OperationName != "click-dog.test.tracing" || parent.TraceID != identity.TraceID || parent.SpanID != identity.SpanID || parent.Attributes["click_dog.source"] != "test-span" || parent.Attributes["click_dog.test_kind"] != "tracing" || parent.Attributes["db.statement"] != "SELECT 1" {
		t.Errorf("local parent contract not preserved: %+v", parent)
	}
	if gotChild.TraceID != child.TraceID || gotChild.SpanID != child.SpanID || gotChild.ParentSpanID != child.ParentSpanID || gotChild.OperationName != child.OperationName || gotChild.Kind != child.Kind || gotChild.Hostname != child.Hostname || gotChild.StartTimeUs != child.StartTimeUs || gotChild.FinishTimeUs != child.FinishTimeUs {
		t.Errorf("native identity changed: got=%+v want=%+v", gotChild, child)
	}
	// The fake reader supplies a string-slice attribute to prove this command's
	// transformation preserves any values already present. The production exact
	// span-log fetch does not run query-log enrichment or create these values.
	if gotChild.Attributes["native"] != "kept" || gotChild.Attributes["click_dog.source"] != "span_log" || gotChild.Attributes["click_dog.test"] != "true" || gotChild.Attributes["click_dog.test_kind"] != "tracing" || gotChild.StringSliceAttributes["db.namespace"][0] != "default" {
		t.Errorf("native provenance/attributes missing: %+v", gotChild)
	}
	if fmt.Sprint(child.Attributes) != fmt.Sprint(originalAttrs) {
		t.Errorf("source child attributes were mutated: got=%v want=%v", child.Attributes, originalAttrs)
	}
	if !reader.closed || !exporter.closed {
		t.Errorf("resources not closed: reader=%v exporter=%v", reader.closed, exporter.closed)
	}
	for _, want := range []string{"ClickHouse query:", "Span materialization:", "Parent propagation:", "PASS", "accepted 2 spans", "trace_id=" + identity.TraceID.String(), "query_id=" + identity.QueryID, "Result: PASS"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestExecuteTracingTest_FailuresRetainIDs(t *testing.T) {
	identity := fixedTracingIdentity()
	wrongTrace := nativeTracingChild(identity)
	wrongTrace.TraceID = uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	missingParent := nativeTracingChild(identity)
	missingParent.ParentSpanID = 999
	capped := make([]model.OpenTelemetrySpan, tracingTestSpanLimit)
	for i := range capped {
		capped[i] = nativeTracingChild(identity)
		capped[i].SpanID = uint64(i + 1)
	}

	tests := []struct {
		name      string
		reader    *fakeTracingTestReader
		customize func(*tracingTestDependencies)
		want      string
	}{
		{name: "query", reader: &fakeTracingTestReader{queryErr: errors.New("ACCESS_DENIED")}, want: "ACCESS_DENIED"},
		{name: "fetch", reader: &fakeTracingTestReader{fetchErr: errors.New("UNKNOWN_TABLE")}, want: "UNKNOWN_TABLE"},
		{name: "no spans timeout", reader: &fakeTracingTestReader{}, customize: func(d *tracingTestDependencies) {
			d.wait = func(context.Context, time.Duration) error { return context.DeadlineExceeded }
		}, want: "deadline or cancellation"},
		{name: "wrong trace", reader: &fakeTracingTestReader{fetchResponses: [][]model.OpenTelemetrySpan{{wrongTrace}}}, want: "unexpected row"},
		{name: "missing parent", reader: &fakeTracingTestReader{fetchResponses: [][]model.OpenTelemetrySpan{{missingParent}}}, want: "did not continue"},
		{name: "cap", reader: &fakeTracingTestReader{fetchResponses: [][]model.OpenTelemetrySpan{capped}}, want: "1000-span safety cap"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := tracingTestDeps(tt.reader, nil)
			if tt.customize != nil {
				tt.customize(&deps)
			}
			var out bytes.Buffer
			if code := executeTracingTest(context.Background(), &config.Config{}, identity, deps, &out); code != 1 {
				t.Fatalf("exit = %d, want 1:\n%s", code, out.String())
			}
			for _, want := range []string{tt.want, "trace_id=" + identity.TraceID.String(), "query_id=" + identity.QueryID, "Result: FAIL"} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output missing %q:\n%s", want, out.String())
				}
			}
		})
	}
}

func TestExecuteTracingTest_CancellationAndConnectionFailure(t *testing.T) {
	identity := fixedTracingIdentity()

	t.Run("cancellation stops polling", func(t *testing.T) {
		reader := &fakeTracingTestReader{}
		deps := tracingTestDeps(reader, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var out bytes.Buffer
		if code := executeTracingTest(ctx, &config.Config{}, identity, deps, &out); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if reader.fetchCalls != 0 {
			t.Errorf("canceled context issued %d fetches, want 0", reader.fetchCalls)
		}
		if !strings.Contains(out.String(), "context canceled") {
			t.Errorf("output missing cancellation: %s", out.String())
		}
	})

	t.Run("connection failure retains IDs", func(t *testing.T) {
		deps := tracingTestDeps(&fakeTracingTestReader{}, nil)
		deps.newReader = func(context.Context, *config.Config) (tracingTestReader, error) {
			return nil, errors.New("dial refused")
		}
		var out bytes.Buffer
		if code := executeTracingTest(context.Background(), &config.Config{}, identity, deps, &out); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		for _, want := range []string{"dial refused", "trace_id=" + identity.TraceID.String(), "query_id=" + identity.QueryID} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("output missing %q: %s", want, out.String())
			}
		}
	})
}

func TestExecuteTracingTest_ReportsEveryExporterFailure(t *testing.T) {
	identity := fixedTracingIdentity()
	reader := &fakeTracingTestReader{fetchResponses: [][]model.OpenTelemetrySpan{{nativeTracingChild(identity)}}}
	failed := &testCommandExporter{err: errors.New("send failed")}
	good := &testCommandExporter{acceptedCount: -1}
	deps := tracingTestDeps(reader, []builtExporter{
		{Label: "OTEL[0]", Endpoint: "bad", Exporter: failed},
		{Label: "SplunkHEC[0]", Endpoint: "missing", InitErr: errors.New("init failed")},
		{Label: "OTEL[1]", Endpoint: "good", Exporter: good},
	})
	var out bytes.Buffer
	if code := executeTracingTest(context.Background(), &config.Config{}, identity, deps, &out); code != 1 {
		t.Fatalf("exit = %d, want 1:\n%s", code, out.String())
	}
	for _, want := range []string{"OTEL[0]:", "SplunkHEC[0]:", "OTEL[1]:", "send failed", "init failed", "PASS (accepted 2 spans", "Result: FAIL"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
	if !failed.closed || !good.closed {
		t.Errorf("initialized exporters were not cleaned up: failed=%v good=%v", failed.closed, good.closed)
	}
}

func TestGenerateTracingIdentity_ValidSampledAndUnique(t *testing.T) {
	first, err := generateTracingIdentity()
	if err != nil {
		t.Fatal(err)
	}
	second, err := generateTracingIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if first.TraceID == uuid.Nil || first.SpanID == 0 || !first.SpanContext.IsValid() || !first.SpanContext.IsSampled() {
		t.Errorf("invalid generated identity: %+v", first)
	}
	if !strings.HasPrefix(first.QueryID, "click-dog-test-") {
		t.Errorf("query ID = %q, want click-dog-test- prefix", first.QueryID)
	}
	if first.TraceID == second.TraceID || first.SpanID == second.SpanID || first.QueryID == second.QueryID {
		t.Errorf("two generated identities collided: first=%+v second=%+v", first, second)
	}
}
