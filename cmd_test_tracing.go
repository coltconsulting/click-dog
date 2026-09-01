package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	oteltrace "go.opentelemetry.io/otel/trace"

	chreader "github.com/coltconsulting/click-dog/internal/clickhouse"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/model"
)

const (
	defaultTracingTestTimeout = 30 * time.Second
	tracingTestSpanLimit      = 1000
	tracingTestLookbackDays   = 1
	tracingPollInitialDelay   = 100 * time.Millisecond
	tracingPollMaxDelay       = time.Second
)

type tracingTestReader interface {
	ExecuteTracingTestQuery(ctx context.Context, spanContext oteltrace.SpanContext, queryID string) error
	FetchSpansForTraceIDs(ctx context.Context, traceIDs []string, lookbackDays, limit int, blacklistOperations []string) ([]model.OpenTelemetrySpan, error)
	Close() error
}

type tracingIdentity struct {
	TraceID     uuid.UUID
	SpanID      uint64
	SpanContext oteltrace.SpanContext
	QueryID     string
}

type tracingTestDependencies struct {
	newReader      func(context.Context, *config.Config) (tracingTestReader, error)
	buildExporters func(*config.Config) []builtExporter
	wait           func(context.Context, time.Duration) error
	now            func() time.Time
}

func productionTracingTestDependencies() tracingTestDependencies {
	return tracingTestDependencies{
		newReader: func(ctx context.Context, cfg *config.Config) (tracingTestReader, error) {
			return chreader.NewTracingTestReader(ctx, cfg.ClickHouse)
		},
		buildExporters: buildExporters,
		wait:           waitForTracingPoll,
		now:            time.Now,
	}
}

func runTestTracing(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("test tracing", flag.ContinueOnError)
	fs.SetOutput(errOut)
	configPath := fs.String("config", config.DefaultConfigFlag, "Path to configuration file")
	timeout := fs.Duration("timeout", defaultTracingTestTimeout, "Deadline for connection, query, polling, fetch, and export")

	fs.Usage = func() {
		_, _ = fmt.Fprintf(errOut, `click-dog test tracing — verify native ClickHouse trace propagation

Runs one traced SELECT 1 query, waits for its native ClickHouse spans,
verifies the generated parent relationship, and asks every configured
exporter to accept the complete trace.

Usage:
  click-dog test tracing [flags]

Flags:
`)
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintf(errOut, "click-dog test tracing: unexpected argument %q\n\n", fs.Arg(0))
		fs.Usage()
		return 2
	}
	if *timeout <= 0 {
		_, _ = fmt.Fprintf(errOut, "click-dog test tracing: -timeout must be greater than zero\n\n")
		fs.Usage()
		return 2
	}

	cfg, _, err := loadConfig(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(out, "Config: FAIL (%v)\n", err)
		return 1
	}
	identity, err := generateTracingIdentity()
	if err != nil {
		_, _ = fmt.Fprintf(out, "Tracing test: FAIL (generate identity: %v)\n", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	return executeTracingTest(ctx, cfg, identity, productionTracingTestDependencies(), out)
}

func generateTracingIdentity() (tracingIdentity, error) {
	var traceID oteltrace.TraceID
	for !traceID.IsValid() {
		if _, err := io.ReadFull(rand.Reader, traceID[:]); err != nil {
			return tracingIdentity{}, err
		}
	}

	var spanID oteltrace.SpanID
	for !spanID.IsValid() {
		if _, err := io.ReadFull(rand.Reader, spanID[:]); err != nil {
			return tracingIdentity{}, err
		}
	}

	var suffix [12]byte
	if _, err := io.ReadFull(rand.Reader, suffix[:]); err != nil {
		return tracingIdentity{}, err
	}

	var modelTraceID uuid.UUID
	copy(modelTraceID[:], traceID[:])
	spanContext := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: oteltrace.FlagsSampled,
	})
	return tracingIdentity{
		TraceID:     modelTraceID,
		SpanID:      binary.BigEndian.Uint64(spanID[:]),
		SpanContext: spanContext,
		QueryID:     "click-dog-test-" + hex.EncodeToString(suffix[:]),
	}, nil
}

func executeTracingTest(ctx context.Context, cfg *config.Config, identity tracingIdentity, deps tracingTestDependencies, out io.Writer) int {
	_, _ = fmt.Fprintln(out, "Tracing test")

	reader, err := deps.newReader(ctx, cfg)
	if err != nil {
		_, _ = fmt.Fprintf(out, "  %-25s FAIL (connect: %v)\n", "ClickHouse query:", err)
		printTracingIDs(out, identity)
		_, _ = fmt.Fprintln(out, "Result: FAIL — check ClickHouse connectivity, TLS, authentication, and SELECT grants.")
		return 1
	}
	defer func() { _ = reader.Close() }()

	queryStart := deps.now()
	queryErr := reader.ExecuteTracingTestQuery(ctx, identity.SpanContext, identity.QueryID)
	queryFinish := deps.now()
	if queryErr != nil {
		_, _ = fmt.Fprintf(out, "  %-25s FAIL (%v)\n", "ClickHouse query:", queryErr)
		printTracingIDs(out, identity)
		_, _ = fmt.Fprintln(out, "Result: FAIL — check SELECT grants and the configured ClickHouse node.")
		return 1
	}
	_, _ = fmt.Fprintf(out, "  %-25s PASS (query_id=%s)\n", "ClickHouse query:", identity.QueryID)

	materializationStart := deps.now()
	spans, err := pollTracingSpans(ctx, reader, identity.TraceID, deps.wait)
	materializationElapsed := deps.now().Sub(materializationStart)
	if materializationElapsed < 0 {
		materializationElapsed = 0
	}
	if err != nil {
		_, _ = fmt.Fprintf(out, "  %-25s FAIL (%v)\n", "Span materialization:", err)
		_, _ = fmt.Fprintf(out, "  %-25s FAIL (materialization did not produce a verifiable parent)\n", "Parent propagation:")
		printTracingIDs(out, identity)
		_, _ = fmt.Fprintln(out, "Result: FAIL — enable opentelemetry_span_log or increase -timeout.")
		return 1
	}

	for _, span := range spans {
		if span.TraceID != identity.TraceID {
			_, _ = fmt.Fprintf(out, "  %-25s FAIL (returned trace_id=%s, want %s)\n", "Span materialization:", span.TraceID, identity.TraceID)
			_, _ = fmt.Fprintf(out, "  %-25s FAIL (wrong-trace row cannot prove propagation)\n", "Parent propagation:")
			printTracingIDs(out, identity)
			_, _ = fmt.Fprintln(out, "Result: FAIL — exact trace-ID recovery returned an unexpected row.")
			return 1
		}
	}
	if len(spans) >= tracingTestSpanLimit {
		_, _ = fmt.Fprintf(out, "  %-25s FAIL (reached the %d-span safety cap)\n", "Span materialization:", tracingTestSpanLimit)
		_, _ = fmt.Fprintf(out, "  %-25s FAIL (the complete trace cannot be verified)\n", "Parent propagation:")
		printTracingIDs(out, identity)
		_, _ = fmt.Fprintln(out, "Result: FAIL — narrow unexpected trace fan-out before retrying.")
		return 1
	}
	_, _ = fmt.Fprintf(out, "  %-25s PASS (%d %s, %s)\n", "Span materialization:", len(spans), pluralSpan(len(spans)), materializationElapsed.Round(time.Millisecond))

	parentFound := false
	for _, span := range spans {
		if span.ParentSpanID == identity.SpanID {
			parentFound = true
			break
		}
	}
	if !parentFound {
		_, _ = fmt.Fprintf(out, "  %-25s FAIL (no native span references parent_span_id=%d)\n", "Parent propagation:", identity.SpanID)
		printTracingIDs(out, identity)
		_, _ = fmt.Fprintln(out, "Result: FAIL — ClickHouse recorded rows but did not continue the supplied parent context.")
		return 1
	}
	_, _ = fmt.Fprintf(out, "  %-25s PASS\n", "Parent propagation:")

	parent := model.OpenTelemetrySpan{
		Hostname:      "click-dog-test",
		TraceID:       identity.TraceID,
		SpanID:        identity.SpanID,
		OperationName: "click-dog.test.tracing",
		Kind:          "CLIENT",
		StartTimeUs:   uint64(queryStart.UnixMicro()),
		FinishTimeUs:  uint64(queryFinish.UnixMicro()),
		FinishDate:    queryFinish,
		Attributes: map[string]string{
			"click_dog.source":    "test-span",
			"click_dog.test":      "true",
			"click_dog.test_kind": "tracing",
			"db.system":           "clickhouse",
			"db.statement":        "SELECT 1",
		},
	}

	batch := make([]model.OpenTelemetrySpan, 1, len(spans)+1)
	batch[0] = parent
	for _, span := range spans {
		child := span
		child.Attributes = cloneStringMap(span.Attributes)
		child.Attributes["click_dog.source"] = "span_log"
		child.Attributes["click_dog.test"] = "true"
		child.Attributes["click_dog.test_kind"] = "tracing"
		batch = append(batch, child)
	}

	allOK := exportTestBatch(ctx, cfg, deps.buildExporters(cfg), batch, out)
	printTracingIDs(out, identity)
	if !allOK {
		_, _ = fmt.Fprintln(out, "Result: FAIL — one or more exporters did not accept the complete trace.")
		return 1
	}
	_, _ = fmt.Fprintln(out, "Result: PASS — exporters accepted the trace; search this trace ID in your backend.")
	return 0
}

func pollTracingSpans(ctx context.Context, reader tracingTestReader, traceID uuid.UUID, wait func(context.Context, time.Duration) error) ([]model.OpenTelemetrySpan, error) {
	delay := tracingPollInitialDelay
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("deadline or cancellation before native spans appeared: %w", err)
		}
		spans, err := reader.FetchSpansForTraceIDs(ctx, []string{traceID.String()}, tracingTestLookbackDays, tracingTestSpanLimit, nil)
		if err != nil {
			return nil, err
		}
		if len(spans) > 0 {
			return spans, nil
		}
		if err := wait(ctx, delay); err != nil {
			return nil, fmt.Errorf("deadline or cancellation before native spans appeared: %w", err)
		}
		if delay < tracingPollMaxDelay {
			delay *= 2
			if delay > tracingPollMaxDelay {
				delay = tracingPollMaxDelay
			}
		}
	}
}

func waitForTracingPoll(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func cloneStringMap(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src)+3)
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func printTracingIDs(out io.Writer, identity tracingIdentity) {
	_, _ = fmt.Fprintf(out, "\ntrace_id=%s\nquery_id=%s\n", identity.TraceID, identity.QueryID)
}
