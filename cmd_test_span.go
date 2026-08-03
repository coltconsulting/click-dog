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

	cfg, _, err := loadConfig(*configPath)
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

	for _, b := range buildExporters(cfg) {
		if b.InitErr != nil {
			_, _ = fmt.Fprintf(out, "%s: FAIL (init: %v)\n", b.Label, b.InitErr)
			allOK = false
			continue
		}

		// Use the same export deadline policy as scheduled/backfill paths;
		// monitor.export_timeout_s: 0 disables the client-side deadline here too.
		result, exportErr := processor.ExportSpansWithDeadline(context.Background(), cfg, b.Exporter, spans)

		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = b.Exporter.Close(closeCtx)
		closeCancel()

		if exportErr != nil {
			_, _ = fmt.Fprintf(out, "%s: FAIL (%s: %v)\n", b.Label, b.Endpoint, exportErr)
			allOK = false
		} else if len(result.Accepted) > 0 {
			_, _ = fmt.Fprintf(out, "%s: ok — sent test span to %s (trace_id=%s span_id=%d)\n", b.Label, b.Endpoint, result.Accepted[0].TraceID, result.Accepted[0].SpanID)
		} else {
			_, _ = fmt.Fprintf(out, "%s: ok — sent test span to %s\n", b.Label, b.Endpoint)
		}
	}

	if !allOK {
		return 1
	}
	_, _ = fmt.Fprintln(out, "\nTest span sent. Look for operation_name=click-dog.test-span in your backend.")
	return 0
}
