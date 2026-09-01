package main

import (
	"fmt"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/export"
	"github.com/coltconsulting/click-dog/internal/model"
)

// loadConfig resolves a -config flag value and loads the file it names. It
// returns the resolved path alongside the config so callers can report which
// file was used without re-resolving; on a load failure the resolved path is
// still returned. Error presentation stays with the caller — each command has
// its own output contract.
func loadConfig(flagPath string) (*config.Config, string, error) {
	resolvedPath, err := config.ResolveConfigPath(flagPath)
	if err != nil {
		return nil, "", err
	}
	cfg, err := config.LoadConfig(resolvedPath)
	if err != nil {
		return nil, resolvedPath, err
	}
	return cfg, resolvedPath, nil
}

// builtExporter pairs a constructed span exporter with the identity strings
// commands print for it and the MultiExporter names it by.
type builtExporter struct {
	// Exporter is nil when InitErr is non-nil.
	Exporter model.SpanExporter
	// OTEL holds the concrete exporter for exporters.otel entries; the daemon
	// reuses its gRPC connection for the OTLP self-metrics push. Nil for
	// other exporter kinds.
	OTEL *export.OTELExporter
	// Label is the human-facing name used in command output: "OTEL[0]",
	// "SplunkHEC[1]".
	Label string
	// Name is the machine-facing name used by MultiExporter and metrics:
	// "otel[0]:collector:4317", "splunk_hec[1]:https://…".
	Name string
	// Endpoint is the collector address or HEC endpoint for status lines.
	Endpoint string
	// InitErr records a constructor failure for this entry.
	InitErr error
}

// buildExporters constructs every exporter configured under exporters.*, in
// OTEL-then-Splunk-HEC order — the single place exporter construction and
// naming live. Constructor failures are reported per entry via InitErr rather
// than aborting the build, because callers disagree on severity: the daemon
// treats any failure as fatal, while `check` and the test commands report the
// failed exporter and keep going.
func buildExporters(cfg *config.Config) []builtExporter {
	built := make([]builtExporter, 0, len(cfg.Exporters.OTEL)+len(cfg.Exporters.SplunkHEC))
	for i, otelCfg := range cfg.Exporters.OTEL {
		b := builtExporter{
			Label:    fmt.Sprintf("OTEL[%d]", i),
			Name:     fmt.Sprintf("otel[%d]:%s", i, otelCfg.CollectorAddress),
			Endpoint: otelCfg.CollectorAddress,
		}
		if exp, err := export.NewOTELExporter(otelCfg); err != nil {
			b.InitErr = err
		} else {
			b.Exporter = exp
			b.OTEL = exp
		}
		built = append(built, b)
	}
	for i, splunkCfg := range cfg.Exporters.SplunkHEC {
		b := builtExporter{
			Label:    fmt.Sprintf("SplunkHEC[%d]", i),
			Name:     fmt.Sprintf("splunk_hec[%d]:%s", i, splunkCfg.Endpoint),
			Endpoint: splunkCfg.Endpoint,
		}
		if exp, err := export.NewSplunkHECExporter(splunkCfg); err != nil {
			b.InitErr = err
		} else {
			b.Exporter = exp
		}
		built = append(built, b)
	}
	return built
}
