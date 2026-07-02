package main

import (
	"strings"
	"testing"

	"github.com/coltconsulting/click-dog/internal/config"
)

// TestSelfMetricsStatusLine covers the startup one-liner that reports whether
// OTLP self-metrics (the surface the Datadog Health dashboard reads) are being
// pushed. The three cases are the ones an operator actually hits: disabled
// (the silent-blank-dashboard trap this line exists to break), enabled while
// inheriting the span connection (the paved-path default), and enabled with a
// standalone collector address.
func TestSelfMetricsStatusLine(t *testing.T) {
	tests := []struct {
		name        string
		cfg         *config.Config
		wantSubstrs []string
		wantAbsent  []string
	}{
		{
			name: "disabled points at the enable flag",
			cfg:  &config.Config{},
			wantSubstrs: []string{
				"OTLP push disabled",
				"metrics.otlp.enabled=true",
			},
			wantAbsent: []string{"→"},
		},
		{
			name: "enabled inheriting the span connection reports otel[0] endpoint",
			cfg: &config.Config{
				Exporters: config.ExportersConfig{
					OTEL: []config.OTELConfig{{CollectorAddress: "otel.example.com:4317"}},
				},
				Metrics: config.MetricsConfig{
					OTLP: config.OTLPMetricsConfig{
						Enabled:               true,
						InheritOTELConnection: true,
						IntervalSeconds:       10,
						ServiceName:           "click-dog-monitor",
						Host:                  "fsnpch201",
					},
				},
			},
			wantSubstrs: []string{
				"OTLP push → otel.example.com:4317",
				"inherit otel[0]",
				"every 10s",
				"service=click-dog-monitor",
				"host=fsnpch201",
			},
		},
		{
			name: "enabled standalone reports the explicit collector address",
			cfg: &config.Config{
				Metrics: config.MetricsConfig{
					OTLP: config.OTLPMetricsConfig{
						Enabled:          true,
						CollectorAddress: "metrics-collector:4317",
						IntervalSeconds:  15,
						ServiceName:      "click-dog-monitor",
						Host:             "node-a",
					},
				},
			},
			wantSubstrs: []string{
				"OTLP push → metrics-collector:4317",
				"standalone",
				"every 15s",
			},
			wantAbsent: []string{"inherit otel[0]"},
		},
		{
			// Unreachable from main() (LoadConfig rejects inherit with zero
			// OTEL exporters, and the enabled branch Fatals before logging),
			// but pinned so the helper's fallback stays defined if it's ever
			// reused somewhere without that guard: inherit is reported, and
			// the endpoint falls back to CollectorAddress rather than panicking
			// on Exporters.OTEL[0].
			name: "enabled inherit with no OTEL entries falls back without panicking",
			cfg: &config.Config{
				Metrics: config.MetricsConfig{
					OTLP: config.OTLPMetricsConfig{
						Enabled:               true,
						InheritOTELConnection: true,
						CollectorAddress:      "fallback:4317",
						IntervalSeconds:       10,
						ServiceName:           "click-dog-monitor",
						Host:                  "node-a",
					},
				},
			},
			wantSubstrs: []string{
				"inherit otel[0]",
				"OTLP push → fallback:4317",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := selfMetricsStatusLine(tc.cfg)
			for _, want := range tc.wantSubstrs {
				if !strings.Contains(got, want) {
					t.Errorf("selfMetricsStatusLine() = %q, missing %q", got, want)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("selfMetricsStatusLine() = %q, should not contain %q", got, absent)
				}
			}
		})
	}
}
