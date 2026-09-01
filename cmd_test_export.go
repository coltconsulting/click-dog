package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/model"
	"github.com/coltconsulting/click-dog/internal/processor"
)

func runTestExport(args []string, out, errOut io.Writer) int {
	return runTestExportAs("test export", args, out, errOut)
}

func runTestExportAs(commandName string, args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet(commandName, flag.ContinueOnError)
	fs.SetOutput(errOut)
	configPath := fs.String("config", config.DefaultConfigFlag, "Path to configuration file")

	fs.Usage = func() {
		_, _ = fmt.Fprintf(errOut, `click-dog %s — send a synthetic test span

Sends a single test span to each configured exporter. The span carries
click_dog.test=true and click_dog.test_kind=export.

Usage:
  click-dog %s [flags]

Flags:
`, commandName, commandName)
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintf(errOut, "click-dog %s: unexpected argument %q\n\n", commandName, fs.Arg(0))
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
		_, _ = fmt.Fprintf(out, "Test span: FAIL (generate identity: %v)\n", err)
		return 1
	}

	now := time.Now()
	span := model.OpenTelemetrySpan{
		Hostname:      "click-dog-test",
		TraceID:       identity.TraceID,
		SpanID:        identity.SpanID,
		OperationName: "click-dog.test-span",
		Kind:          "INTERNAL",
		StartTimeUs:   uint64(now.Add(-100 * time.Millisecond).UnixMicro()),
		FinishTimeUs:  uint64(now.UnixMicro()),
		FinishDate:    now,
		Attributes: map[string]string{
			"click_dog.test":      "true",
			"click_dog.test_kind": "export",
			"click_dog.source":    "test-span",
			"db.system":           "clickhouse",
			"db.statement":        "SELECT 1 -- click-dog test span",
		},
	}

	_, _ = fmt.Fprintln(out, "Exporter test")
	allOK := exportTestBatch(context.Background(), cfg, buildExporters(cfg), []model.OpenTelemetrySpan{span}, out)
	_, _ = fmt.Fprintf(out, "\ntrace_id=%s\n", span.TraceID)
	if !allOK {
		_, _ = fmt.Fprintln(out, "Result: FAIL — one or more exporters did not accept the test span.")
		return 1
	}
	_, _ = fmt.Fprintln(out, "Result: PASS — all configured exporters accepted the test span.")
	return 0
}

// exportTestBatch sends one batch to each exporter independently. Constructor
// and send failures never stop later exporters from being reported.
func exportTestBatch(ctx context.Context, cfg *config.Config, exporters []builtExporter, spans []model.OpenTelemetrySpan, out io.Writer) bool {
	if len(exporters) == 0 {
		_, _ = fmt.Fprintf(out, "  %-18s FAIL (no exporters configured)\n", "Exporters:")
		return false
	}

	allOK := true
	f, filterErr := filter.NewQueryFilter(cfg.Filters)
	if filterErr != nil {
		_, _ = fmt.Fprintf(out, "  %-18s FAIL (query text policy: %v)\n", "Exporters:", filterErr)
		return false
	}
	for _, b := range exporters {
		if b.InitErr != nil {
			_, _ = fmt.Fprintf(out, "  %-18s FAIL (init: %v)\n", b.Label+":", b.InitErr)
			allOK = false
			continue
		}

		result, exportErr := processor.ExportSpansWithDeadline(ctx, cfg, b.Exporter, f, spans)

		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = b.Exporter.Close(closeCtx)
		closeCancel()

		if exportErr != nil {
			_, _ = fmt.Fprintf(out, "  %-18s FAIL (%s: %v)\n", b.Label+":", b.Endpoint, exportErr)
			allOK = false
			continue
		}
		if len(result.Accepted) != len(spans) {
			_, _ = fmt.Fprintf(out, "  %-18s FAIL (%s accepted %d/%d spans)\n", b.Label+":", b.Endpoint, len(result.Accepted), len(spans))
			allOK = false
			continue
		}
		_, _ = fmt.Fprintf(out, "  %-18s PASS (accepted %d %s at %s)\n", b.Label+":", len(spans), pluralSpan(len(spans)), b.Endpoint)
	}
	return allOK
}

func pluralSpan(n int) string {
	if n == 1 {
		return "span"
	}
	return "spans"
}
