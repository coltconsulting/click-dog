package main

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"time"

	"github.com/google/uuid"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/export"
	"github.com/coltconsulting/click-dog/internal/model"
	"github.com/coltconsulting/click-dog/internal/processor"
)

func runTestSpan(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("test-span", flag.ContinueOnError)
	fs.SetOutput(errOut)
	configPath := fs.String("config", config.DefaultConfigFlag, "Path to configuration file")

	fs.Usage = func() {
		_, _ = fmt.Fprintf(errOut, `click-dog test-span — send a synthetic test span

Sends a single test span to each configured exporter to verify the
export pipeline works end-to-end. The span is tagged with
click_dog.test=true so it can be identified and filtered out.

Usage:
  click-dog test-span [flags]

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

	resolvedPath, err := config.ResolveConfigPath(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(out, "Config: FAIL (%v)\n", err)
		return 1
	}
	cfg, err := config.LoadConfig(resolvedPath)
	if err != nil {
		_, _ = fmt.Fprintf(out, "Config: FAIL (%v)\n", err)
		return 1
	}

	now := time.Now()
	var traceID uuid.UUID
	binary.BigEndian.PutUint64(traceID[0:8], rand.Uint64())
	binary.BigEndian.PutUint64(traceID[8:16], rand.Uint64())
	testSpan := model.OpenTelemetrySpan{
		Hostname:      "click-dog-test",
		TraceID:       traceID,
		SpanID:        rand.Uint64(),
		ParentSpanID:  0,
		OperationName: "click-dog.test-span",
		Kind:          "INTERNAL",
		StartTimeUs:   uint64(now.Add(-100 * time.Millisecond).UnixMicro()),
		FinishTimeUs:  uint64(now.UnixMicro()),
		FinishDate:    now,
		Attributes: map[string]string{
			"click_dog.test":   "true",
			"click_dog.source": "test-span",
			"db.system":        "clickhouse",
			"db.statement":     "SELECT 1 -- click-dog test span",
		},
	}

	spans := []model.OpenTelemetrySpan{testSpan}
	allOK := true

	for i, otelCfg := range cfg.Exporters.OTEL {
		exp, otelErr := export.NewOTELExporter(otelCfg)
		if otelErr != nil {
			_, _ = fmt.Fprintf(out, "OTEL[%d]: FAIL (init: %v)\n", i, otelErr)
			allOK = false
			continue
		}

		// Use the same export deadline policy as scheduled/backfill paths;
		// monitor.export_timeout_s: 0 disables the client-side deadline here too.
		result, exportErr := processor.ExportSpansWithDeadline(context.Background(), cfg, exp, spans)

		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = exp.Close(closeCtx)
		closeCancel()

		if exportErr != nil {
			_, _ = fmt.Fprintf(out, "OTEL[%d]: FAIL (%s: %v)\n", i, otelCfg.CollectorAddress, exportErr)
			allOK = false
		} else if len(result.Accepted) > 0 {
			_, _ = fmt.Fprintf(out, "OTEL[%d]: ok — sent test span to %s (trace_id=%s span_id=%d)\n", i, otelCfg.CollectorAddress, result.Accepted[0].TraceID, result.Accepted[0].SpanID)
		} else {
			_, _ = fmt.Fprintf(out, "OTEL[%d]: ok — sent test span to %s\n", i, otelCfg.CollectorAddress)
		}
	}

	for i, splunkCfg := range cfg.Exporters.SplunkHEC {
		exp, splunkErr := export.NewSplunkHECExporter(splunkCfg)
		if splunkErr != nil {
			_, _ = fmt.Fprintf(out, "SplunkHEC[%d]: FAIL (init: %v)\n", i, splunkErr)
			allOK = false
			continue
		}

		// Use the same export deadline policy as scheduled/backfill paths;
		// monitor.export_timeout_s: 0 disables the client-side deadline here too.
		result, exportErr := processor.ExportSpansWithDeadline(context.Background(), cfg, exp, spans)

		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = exp.Close(closeCtx)
		closeCancel()

		if exportErr != nil {
			_, _ = fmt.Fprintf(out, "SplunkHEC[%d]: FAIL (%s: %v)\n", i, splunkCfg.Endpoint, exportErr)
			allOK = false
		} else if len(result.Accepted) > 0 {
			_, _ = fmt.Fprintf(out, "SplunkHEC[%d]: ok — sent test span to %s (trace_id=%s span_id=%d)\n", i, splunkCfg.Endpoint, result.Accepted[0].TraceID, result.Accepted[0].SpanID)
		} else {
			_, _ = fmt.Fprintf(out, "SplunkHEC[%d]: ok — sent test span to %s\n", i, splunkCfg.Endpoint)
		}
	}

	if !allOK {
		return 1
	}
	_, _ = fmt.Fprintln(out, "\nTest span sent. Look for operation_name=click-dog.test-span in your backend.")
	return 0
}
