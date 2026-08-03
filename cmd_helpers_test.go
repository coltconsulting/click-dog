package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/model"
)

// minimal loadable config: LoadConfig validates that at least one exporter
// and the ClickHouse connection are configured.
const helperTestConfig = `clickhouse:
  host: localhost
  port: 9000

exporters:
  otel:
    - collector_address: localhost:4317
      service_name: helper-test

monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 30
`

func TestLoadConfig_ValidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "click-dog.yaml")
	if err := os.WriteFile(path, []byte(helperTestConfig), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, resolvedPath, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if resolvedPath != path {
		t.Errorf("resolvedPath = %q, want %q", resolvedPath, path)
	}
	if cfg == nil || cfg.ClickHouse.Host != "localhost" {
		t.Errorf("config not loaded: %+v", cfg)
	}
}

func TestLoadConfig_MissingExplicitPath(t *testing.T) {
	_, _, err := loadConfig(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("expected error for missing explicit config path")
	}
}

// A file that resolves but fails to load must still return the resolved path,
// so callers can name the file they rejected.
func TestLoadConfig_LoadErrorReturnsResolvedPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.yaml")
	if err := os.WriteFile(path, []byte(":\tnot yaml"), 0600); err != nil {
		t.Fatal(err)
	}

	_, resolvedPath, err := loadConfig(path)
	if err == nil {
		t.Fatal("expected error for unparseable config")
	}
	if resolvedPath != path {
		t.Errorf("resolvedPath = %q, want %q even on load failure", resolvedPath, path)
	}
}

func TestBuildExporters_LabelsNamesAndOrder(t *testing.T) {
	cfg := &config.Config{}
	cfg.Exporters.OTEL = []config.OTELConfig{
		{CollectorAddress: "collector:4317", ServiceName: "svc"},
	}
	cfg.Exporters.SplunkHEC = []config.SplunkHECConfig{
		{Endpoint: "https://splunk.example.com:8088", Token: "tok"},
	}

	built := buildExporters(cfg)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		for _, b := range built {
			if b.Exporter != nil {
				_ = b.Exporter.Close(ctx)
			}
		}
	})

	if len(built) != 2 {
		t.Fatalf("built %d exporters, want 2", len(built))
	}

	tests := []struct {
		idx      int
		label    string
		name     string
		endpoint string
		wantOTEL bool
	}{
		{0, "OTEL[0]", "otel[0]:collector:4317", "collector:4317", true},
		{1, "SplunkHEC[0]", "splunk_hec[0]:https://splunk.example.com:8088", "https://splunk.example.com:8088", false},
	}
	for _, tc := range tests {
		b := built[tc.idx]
		if b.InitErr != nil {
			t.Fatalf("built[%d] InitErr = %v, want nil", tc.idx, b.InitErr)
		}
		if b.Exporter == nil {
			t.Fatalf("built[%d] Exporter is nil", tc.idx)
		}
		if b.Label != tc.label {
			t.Errorf("built[%d] Label = %q, want %q", tc.idx, b.Label, tc.label)
		}
		if b.Name != tc.name {
			t.Errorf("built[%d] Name = %q, want %q", tc.idx, b.Name, tc.name)
		}
		if b.Endpoint != tc.endpoint {
			t.Errorf("built[%d] Endpoint = %q, want %q", tc.idx, b.Endpoint, tc.endpoint)
		}
		if (b.OTEL != nil) != tc.wantOTEL {
			t.Errorf("built[%d] OTEL non-nil = %v, want %v", tc.idx, b.OTEL != nil, tc.wantOTEL)
		}
		// Both shipped exporter kinds support active probing; `check` relies
		// on the type assertion, so pin it here.
		if _, ok := b.Exporter.(model.ConnectivityChecker); !ok {
			t.Errorf("built[%d] (%s) does not implement model.ConnectivityChecker", tc.idx, b.Label)
		}
	}
}

// A constructor failure must land in InitErr for that entry — not abort the
// build — so `check` and `test-span` can report the broken exporter and keep
// probing the rest.
func TestBuildExporters_InitErrorDoesNotAbortBuild(t *testing.T) {
	cfg := &config.Config{}
	cfg.Exporters.SplunkHEC = []config.SplunkHECConfig{
		{Endpoint: "https://splunk.example.com:8088"}, // no token → constructor error
		{Endpoint: "https://splunk2.example.com:8088", Token: "tok"},
	}

	built := buildExporters(cfg)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		for _, b := range built {
			if b.Exporter != nil {
				_ = b.Exporter.Close(ctx)
			}
		}
	})

	if len(built) != 2 {
		t.Fatalf("built %d exporters, want 2", len(built))
	}
	if built[0].InitErr == nil || built[0].Exporter != nil {
		t.Errorf("built[0] = {InitErr: %v, Exporter: %v}, want init error and nil exporter", built[0].InitErr, built[0].Exporter)
	}
	if !strings.Contains(built[0].Label, "SplunkHEC[0]") {
		t.Errorf("built[0] Label = %q, want SplunkHEC[0]", built[0].Label)
	}
	if built[1].InitErr != nil || built[1].Exporter == nil {
		t.Errorf("built[1] = {InitErr: %v}, want working exporter after a failed one", built[1].InitErr)
	}
}
