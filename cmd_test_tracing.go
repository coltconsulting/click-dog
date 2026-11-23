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

	"github.com/ClickHouse/clickhouse-go/v2"
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
	chErrUnknownTable         = 60
)

type tracingTestReader interface {
	ServerTime(ctx context.Context) (time.Time, error)
	ExecuteTracingTestQuery(ctx context.Context, spanContext oteltrace.SpanContext, queryID string) error
	FetchSpansForTraceIDs(ctx context.Context, traceIDs []string, lookbackDays int, since time.Time, limit int, blacklistOperations []string) ([]model.OpenTelemetrySpan, error)
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
		printFlagDefaults(errOut, fs)
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if err := rejectLeftoverArgs("click-dog test tracing", fs, args); err != nil {
		_, _ = fmt.Fprintf(errOut, "%v\n\n", err)
		fs.Usage()
		return 2
	}
	if *timeout <= 0 {
		_, _ = fmt.Fprintf(errOut, "click-dog test tracing: --timeout must be greater than zero\n\n")
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

	// The canary's spans are stamped by the server's clock, so the polls read
	// from the server's time before the canary rather than this host's. If it
	// cannot be read, they fall back to the whole day floor.
	serverStart, err := reader.ServerTime(ctx)
	if err != nil {
		serverStart = time.Time{}
	}
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
	spans, timedOut, err := pollTracingSpans(ctx, reader, identity.TraceID, identity.SpanID, serverStart, deps.wait, out)
	materializationElapsed := deps.now().Sub(materializationStart)
	if materializationElapsed < 0 {
		materializationElapsed = 0
	}
	if err != nil {
		_, _ = fmt.Fprintf(out, "  %-25s FAIL (%v)\n", "Span materialization:", err)
		_, _ = fmt.Fprintf(out, "  %-25s FAIL (materialization did not produce a verifiable parent)\n", "Parent propagation:")
		printTracingIDs(out, identity)
		_, _ = fmt.Fprintln(out, "Result: FAIL — enable opentelemetry_span_log or increase --timeout.")
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

	if !tracingContinuesParent(spans, identity.SpanID) {
		if timedOut {
			// The spans read so far are part of the trace; the one that
			// continues the parent may still be waiting for a flush.
			_, _ = fmt.Fprintf(out, "  %-25s FAIL (no native span referenced parent_span_id=%d within --timeout)\n", "Parent propagation:", identity.SpanID)
			printTracingIDs(out, identity)
			_, _ = fmt.Fprintln(out, "Result: FAIL — the span that continues the supplied parent context was not seen within --timeout. Increase --timeout when the span-log flush interval is long; otherwise ClickHouse did not continue the parent context.")
			return 1
		}
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
			"click_dog.source":    "test-tracing",
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
	_, _ = fmt.Fprintln(out, "Result: PASS — exporters accepted the trace; search this trace ID in the backend.")
	return 0
}

// pollTracingSpans reads the canary's trace until a native span continues the
// supplied parent. Every native span of the canary starts after serverStart,
// the server's time before the canary was sent, so each read covers only
// span-log rows that finished from a minute before it, not the whole day
// floor. The zero time reads the day floor.
//
// ClickHouse writes a span to the span log when the span finishes, and the
// span that continues the supplied parent is the query's outermost one, so it
// finishes last. A flush can land between the inner spans and that one, so a
// read can return part of the trace without the parent link. Polling goes on
// past such a partial read; because the log is written in finish order, the
// read that holds the parent link also holds every span that finished before
// it. When the deadline passes after a partial read, that read is returned
// with timedOut set and no error, so the caller judges what ClickHouse
// actually recorded and can say that time ran out.
func pollTracingSpans(ctx context.Context, reader tracingTestReader, traceID uuid.UUID, parentSpanID uint64, serverStart time.Time, wait func(context.Context, time.Duration) error, out io.Writer) (spans []model.OpenTelemetrySpan, timedOut bool, err error) {
	delay := tracingPollInitialDelay
	missingTableReported := false
	var lastMissingTable error
	var partial []model.OpenTelemetrySpan
	pollStopped := func(err error) ([]model.OpenTelemetrySpan, bool, error) {
		switch {
		case len(partial) > 0:
			return partial, true, nil
		case lastMissingTable != nil:
			return nil, false, fmt.Errorf("deadline or cancellation waiting for system.opentelemetry_span_log to materialize (last read: %v): %w", lastMissingTable, err)
		}
		return nil, false, fmt.Errorf("deadline or cancellation before native spans appeared: %w", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return pollStopped(err)
		}
		spans, err := reader.FetchSpansForTraceIDs(ctx, []string{traceID.String()}, tracingTestLookbackDays, serverStart, tracingTestSpanLimit, nil)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				// The read failed because the deadline passed during it.
				return pollStopped(ctxErr)
			}
			// ClickHouse creates this system log lazily on its first flush. Only
			// UNKNOWN_TABLE from this exact span-log read is a startup condition;
			// permission, schema, and transport errors must still fail promptly.
			var exception *clickhouse.Exception
			if !errors.As(err, &exception) || exception.Code != chErrUnknownTable {
				return nil, false, err
			}
			if !missingTableReported {
				_, _ = fmt.Fprintf(out, "  %-25s waiting for system.opentelemetry_span_log to be created (first flush)\n", "Span materialization:")
				missingTableReported = true
			}
			lastMissingTable = err
		} else {
			lastMissingTable = nil
		}
		// A read at the safety cap is judged at once: more polling cannot
		// make a truncated trace verifiable.
		if tracingContinuesParent(spans, parentSpanID) || len(spans) >= tracingTestSpanLimit {
			return spans, false, nil
		}
		if len(spans) > 0 {
			partial = spans
		}
		if err := wait(ctx, delay); err != nil {
			return pollStopped(err)
		}
		if delay < tracingPollMaxDelay {
			delay *= 2
			if delay > tracingPollMaxDelay {
				delay = tracingPollMaxDelay
			}
		}
	}
}

// tracingContinuesParent reports whether any span names parentSpanID as its
// parent.
func tracingContinuesParent(spans []model.OpenTelemetrySpan, parentSpanID uint64) bool {
	for _, span := range spans {
		if span.ParentSpanID == parentSpanID {
			return true
		}
	}
	return false
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
