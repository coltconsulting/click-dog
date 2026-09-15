package config

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// LoadConfig: default values applied to minimal config
// ---------------------------------------------------------------------------

func TestLoadConfig_ValidMinimal(t *testing.T) {
	cfg, err := LoadConfig(writeConfigFile(t, validMinimalYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Spot-check defaults are applied
	if cfg.Exporters.OTEL[0].ServiceName != "click-dog-monitor" {
		t.Errorf("service_name default = %q, want click-dog-monitor", cfg.Exporters.OTEL[0].ServiceName)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("log_level default = %q, want info", cfg.LogLevel)
	}
	if cfg.ClickHouse.MaxOpenConns != 2 {
		t.Errorf("max_open_conns default = %d, want 2", cfg.ClickHouse.MaxOpenConns)
	}
	if cfg.ClickHouse.MaxIdleConns != 1 {
		t.Errorf("max_idle_conns default = %d, want 1", cfg.ClickHouse.MaxIdleConns)
	}
	if cfg.ClickHouse.QueryTimeoutS != 30 {
		t.Errorf("query_timeout_s default = %d, want 30", cfg.ClickHouse.QueryTimeoutS)
	}
	if cfg.ClickHouse.Port != 9000 {
		t.Errorf("port default = %d, want 9000", cfg.ClickHouse.Port)
	}
	if cfg.ClickHouse.MaxMemoryUsage != 104857600 {
		t.Errorf("max_memory_usage default = %d, want 104857600", cfg.ClickHouse.MaxMemoryUsage)
	}
	if cfg.Monitor.MaxSpansPerCycle != 1000 {
		t.Errorf("max_spans_per_cycle default = %d, want 1000", cfg.Monitor.MaxSpansPerCycle)
	}
	if cfg.Monitor.DedupCacheSize != 10000 {
		t.Errorf("dedup_cache_size default = %d, want 10000", cfg.Monitor.DedupCacheSize)
	}
	if cfg.Monitor.ExportTimeoutS != DefaultExportTimeoutS {
		t.Errorf("export_timeout_s default = %d, want %d", cfg.Monitor.ExportTimeoutS, DefaultExportTimeoutS)
	}
	if cfg.Monitor.LookbackBufferS != 10 {
		t.Errorf("lookback_buffer_s default = %d, want 10", cfg.Monitor.LookbackBufferS)
	}
	if cfg.Monitor.LookbackS != 40 { // 30 + 10
		t.Errorf("lookback_s default = %d, want 40 (check_interval 30 + buffer 10)", cfg.Monitor.LookbackS)
	}
	if cfg.Monitor.Backoff.MaxIntervalS != 300 {
		t.Errorf("backoff.max_interval_s default = %d, want 300", cfg.Monitor.Backoff.MaxIntervalS)
	}
	if cfg.Monitor.Backoff.BackoffFactor != 2.0 {
		t.Errorf("backoff.backoff_factor default = %f, want 2.0", cfg.Monitor.Backoff.BackoffFactor)
	}
	if cfg.Filters.QueryTextMode != QueryTextModeRaw {
		t.Errorf("filters.query_text_mode default = %q, want raw", cfg.Filters.QueryTextMode)
	}
}

func TestLoadConfig_ClickHousePortDefault(t *testing.T) {
	yaml := `
clickhouse:
  host: localhost
  database: default
exporters:
  otel:
    - collector_address: localhost:4317
monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 30
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ClickHouse.Port != 9000 {
		t.Errorf("clickhouse.port default = %d, want 9000", cfg.ClickHouse.Port)
	}
}

func TestLoadConfig_LegacyRedactionRulesSelectRedactedMode(t *testing.T) {
	yaml := validMinimalYAML + `
filters:
  redact_queries:
    - pattern: "'[^']*'"
      replacement: "?"
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Filters.QueryTextMode != QueryTextModeRedacted {
		t.Fatalf("query_text_mode = %q, want redacted for legacy redact_queries config", cfg.Filters.QueryTextMode)
	}
	if len(cfg.ValidationWarnings) == 0 {
		t.Fatal("expected a migration warning for implicit legacy redaction")
	}
	warnings := strings.Join(cfg.ValidationWarnings, "\n")
	if !strings.Contains(warnings, "statements that match no rule are now omitted") {
		t.Fatalf("migration warning does not disclose stricter unmatched-query behavior: %v", cfg.ValidationWarnings)
	}
}

func TestLoadConfig_QueryTextModeWarnings(t *testing.T) {
	tests := []struct {
		name        string
		yaml        string
		wantWarning string
	}{
		{
			name:        "normalized only without enrichment",
			yaml:        strings.Replace(validMinimalYAML, "monitor:\n", "filters:\n  query_text_mode: normalized_only\nmonitor:\n  enrich_from_query_log: false\n", 1),
			wantWarning: "scheduled/native spans cannot receive query_log normalized previews",
		},
		{
			name: "none ignores stale redaction rules",
			yaml: validMinimalYAML + `
filters:
  query_text_mode: none
  redact_queries:
    - pattern: "[invalid("
`,
			wantWarning: "redact_queries is ignored when filters.query_text_mode is none",
		},
		{
			name: "normalized only ignores stale redaction rules",
			yaml: validMinimalYAML + `
filters:
  query_text_mode: normalized_only
  redact_queries:
    - pattern: "[invalid("
`,
			wantWarning: "redact_queries is ignored when filters.query_text_mode is normalized_only",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := LoadConfig(writeConfigFile(t, tt.yaml))
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if warnings := strings.Join(cfg.ValidationWarnings, "\n"); !strings.Contains(warnings, tt.wantWarning) {
				t.Fatalf("ValidationWarnings = %v, want substring %q", cfg.ValidationWarnings, tt.wantWarning)
			}
		})
	}
}

func TestLoadConfig_ExplicitEmptyQueryTextModeRejected(t *testing.T) {
	yaml := validMinimalYAML + `
filters:
  query_text_mode: ""
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil || !strings.Contains(err.Error(), "query_text_mode") {
		t.Fatalf("LoadConfig error = %v, want explicit empty query_text_mode rejection", err)
	}
}

// ---------------------------------------------------------------------------
// LoadConfig: lookback defaults
// ---------------------------------------------------------------------------

func TestLoadConfig_LookbackDefaults(t *testing.T) {
	tests := []struct {
		name         string
		yaml         string
		wantLookback int
		wantBuffer   int
	}{
		{
			name: "default buffer and lookback",
			yaml: `
clickhouse:
  host: localhost
  port: 9000
  database: default
exporters:
  otel:
    - collector_address: localhost:4317
monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 20
`,
			wantLookback: 30, // 20 + 10
			wantBuffer:   10,
		},
		{
			name: "custom buffer",
			yaml: `
clickhouse:
  host: localhost
  port: 9000
  database: default
exporters:
  otel:
    - collector_address: localhost:4317
monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 20
  lookback_buffer_s: 30
`,
			wantLookback: 50, // 20 + 30
			wantBuffer:   30,
		},
		{
			name: "explicit lookback overrides formula",
			yaml: `
clickhouse:
  host: localhost
  port: 9000
  database: default
exporters:
  otel:
    - collector_address: localhost:4317
monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 20
  lookback_s: 100
`,
			wantLookback: 100,
			wantBuffer:   10, // default still applied
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := LoadConfig(writeConfigFile(t, tt.yaml))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.Monitor.LookbackS != tt.wantLookback {
				t.Errorf("lookback_s = %d, want %d", cfg.Monitor.LookbackS, tt.wantLookback)
			}
			if cfg.Monitor.LookbackBufferS != tt.wantBuffer {
				t.Errorf("lookback_buffer_s = %d, want %d", cfg.Monitor.LookbackBufferS, tt.wantBuffer)
			}
		})
	}
}

func TestLoadConfig_ExportTimeoutExplicitZeroDisablesDeadline(t *testing.T) {
	yaml := `
clickhouse:
  host: localhost
  port: 9000
  database: default
exporters:
  otel:
    - collector_address: localhost:4317
monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 30
  export_timeout_s: 0
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Monitor.ExportTimeoutS != 0 {
		t.Errorf("export_timeout_s = %d, want 0 for disabled deadline", cfg.Monitor.ExportTimeoutS)
	}
}

// ---------------------------------------------------------------------------
// ShouldEnrichFromQueryLog: pointer-based default
// ---------------------------------------------------------------------------

func TestShouldEnrichFromQueryLog(t *testing.T) {
	t.Run("nil defaults to true", func(t *testing.T) {
		m := MonitorConfig{EnrichFromQueryLog: nil}
		if !m.ShouldEnrichFromQueryLog() {
			t.Error("expected true when nil")
		}
	})
	t.Run("explicit true", func(t *testing.T) {
		v := true
		m := MonitorConfig{EnrichFromQueryLog: &v}
		if !m.ShouldEnrichFromQueryLog() {
			t.Error("expected true")
		}
	})
	t.Run("explicit false", func(t *testing.T) {
		v := false
		m := MonitorConfig{EnrichFromQueryLog: &v}
		if m.ShouldEnrichFromQueryLog() {
			t.Error("expected false")
		}
	})
}

// ---------------------------------------------------------------------------
// LoadConfig: exporter defaults
// ---------------------------------------------------------------------------

func TestLoadConfig_ExporterDefaults(t *testing.T) {
	yaml := `
clickhouse:
  host: localhost
  port: 9000
  database: default
exporters:
  otel:
    - collector_address: localhost:4317
  splunk_hec:
    - endpoint: https://splunk:8088
      token: test
monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 30
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// OTEL defaults
	if cfg.Exporters.OTEL[0].ServiceName != "click-dog-monitor" {
		t.Errorf("OTEL service_name default = %q", cfg.Exporters.OTEL[0].ServiceName)
	}
	if cfg.Exporters.OTEL[0].MaxQueryLength != 100000 {
		t.Errorf("OTEL max_query_length default = %d", cfg.Exporters.OTEL[0].MaxQueryLength)
	}
	// Splunk HEC defaults
	if cfg.Exporters.SplunkHEC[0].MaxQueryLength != 100000 {
		t.Errorf("SplunkHEC max_query_length default = %d", cfg.Exporters.SplunkHEC[0].MaxQueryLength)
	}
}

// ---------------------------------------------------------------------------
// LoadConfig: additional defaults (table-driven)
// ---------------------------------------------------------------------------

func TestLoadConfig_AdditionalDefaults(t *testing.T) {
	cfg, err := LoadConfig(writeConfigFile(t, validMinimalYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tests := []struct {
		name string
		got  int
		want int
	}{
		{"monitor.max_query_length", cfg.Monitor.MaxQueryLength, 100000},
		{"canary.threshold_duration_ms", cfg.Monitor.Canary.ThresholdDurationMs, 60000},
		{"circuit_breaker.failure_threshold", cfg.Monitor.CircuitBreaker.FailureThreshold, 3},
		{"circuit_breaker.success_threshold", cfg.Monitor.CircuitBreaker.SuccessThreshold, 1},
		{"circuit_breaker.reset_timeout_s", cfg.Monitor.CircuitBreaker.ResetTimeoutS, 60},
		{"log_rotation.max_size_mb", cfg.LogRotation.MaxSizeMB, 100},
		{"log_rotation.max_files", cfg.LogRotation.MaxFiles, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("%s default = %d, want %d", tt.name, tt.got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// LoadConfig: topology audit defaults
// ---------------------------------------------------------------------------

func TestLoadConfig_TopologyAuditDefaults(t *testing.T) {
	cfg, err := LoadConfig(writeConfigFile(t, validMinimalYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Monitor.TopologyAudit.Enabled {
		t.Error("topology_audit should be enabled by default")
	}
	if cfg.Monitor.TopologyAudit.IntervalS != 300 {
		t.Errorf("topology_audit.interval_s default = %d, want 300", cfg.Monitor.TopologyAudit.IntervalS)
	}
	if cfg.Monitor.TopologyAudit.DebounceCount != 2 {
		t.Errorf("topology_audit.debounce_count default = %d, want 2", cfg.Monitor.TopologyAudit.DebounceCount)
	}
	if cfg.Monitor.TopologyAudit.QueryLogLookbackMinutes != 15 {
		t.Errorf("topology_audit.query_log_lookback_minutes default = %d, want 15", cfg.Monitor.TopologyAudit.QueryLogLookbackMinutes)
	}
}

func TestLoadConfig_TopologyAuditExplicitDisablePreserved(t *testing.T) {
	yaml := `
clickhouse:
  host: localhost
  port: 9000
  database: default
exporters:
  otel:
    - collector_address: localhost:4317
monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 30
  topology_audit:
    enabled: false
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Monitor.TopologyAudit.Enabled {
		t.Error("explicit topology_audit.enabled: false was overridden by the default")
	}
}

// ---------------------------------------------------------------------------
// LoadConfig: canary defaults
// ---------------------------------------------------------------------------

func TestLoadConfig_CanaryDefaults(t *testing.T) {
	cfg, err := LoadConfig(writeConfigFile(t, validMinimalYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Monitor.Canary.Enabled {
		t.Error("canary should be disabled by default")
	}
}

// ---------------------------------------------------------------------------
// LoadConfig: health defaults
// ---------------------------------------------------------------------------

func TestLoadConfig_HealthDefaults(t *testing.T) {
	// Minimal config has health.enabled unset (false). The listen address
	// default is deliberately NOT applied when health is disabled — that
	// way -validate output doesn't print a port the process won't open.
	cfg, err := LoadConfig(writeConfigFile(t, validMinimalYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Health.Enabled {
		t.Error("health should be disabled by default")
	}
	if cfg.Health.ListenAddress != "" {
		t.Errorf("health.listen_address = %q; default should not be applied when health is disabled", cfg.Health.ListenAddress)
	}
}

func TestLoadConfig_HealthDefaultsAppliedWhenEnabled(t *testing.T) {
	yaml := validMinimalYAML + `
health:
  enabled: true
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Health.ListenAddress != ":8686" {
		t.Errorf("health.listen_address = %q, want :8686 when enabled without explicit address", cfg.Health.ListenAddress)
	}
}

func TestLoadConfig_HealthClusterDefaultPeerTimeout(t *testing.T) {
	yaml := validMinimalYAML + `
health:
  enabled: true
  cluster:
    enabled: true
    self: "node-0:8686"
    peers:
      - "node-1:8686"
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Health.Cluster.PeerTimeoutMs != 3000 {
		t.Errorf("peer_timeout_ms = %d, want 3000 default when omitted", cfg.Health.Cluster.PeerTimeoutMs)
	}
	if cfg.Health.Cluster.Self != "node-0:8686" {
		t.Errorf("self = %q, want node-0:8686 (round-tripped from YAML)", cfg.Health.Cluster.Self)
	}
}

func TestLoadConfig_HealthCluster_NormalizesWhitespace(t *testing.T) {
	// Surrounding whitespace on `self` or `peers` entries would silently
	// break the runtime self-skip (`p == cs.self` compares strings
	// byte-for-byte). LoadConfig trims so the canonical form propagates
	// into Validate(), the cluster source, and the fanout. YAML rarely
	// carries surrounding spaces, but a copy-paste like
	// `self: " node-0:8686 "` shouldn't quietly cause a self-loop.
	yaml := validMinimalYAML + `
health:
  enabled: true
  cluster:
    enabled: true
    self: "  node-0:8686  "
    peers:
      - "  node-0:8686  "
      - " node-1:8686"
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Health.Cluster.Self != "node-0:8686" {
		t.Errorf("self = %q, want trimmed %q", cfg.Health.Cluster.Self, "node-0:8686")
	}
	wantPeers := []string{"node-0:8686", "node-1:8686"}
	for i, want := range wantPeers {
		if cfg.Health.Cluster.Peers[i] != want {
			t.Errorf("peers[%d] = %q, want trimmed %q", i, cfg.Health.Cluster.Peers[i], want)
		}
	}
}

func TestLoadConfig_HealthClusterDefaultsNotAppliedWhenDisabled(t *testing.T) {
	// Mirror of the listen-address default behavior: if health.cluster.enabled
	// is false, default values are not silently materialised — operators
	// reading the parsed config can tell the feature wasn't turned on.
	cfg, err := LoadConfig(writeConfigFile(t, validMinimalYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Health.Cluster.Enabled {
		t.Error("cluster should be disabled by default")
	}
	if cfg.Health.Cluster.PeerTimeoutMs != 0 {
		t.Errorf("peer_timeout_ms = %d, want 0 (no default applied when disabled)", cfg.Health.Cluster.PeerTimeoutMs)
	}
}

// ---------------------------------------------------------------------------
// LoadConfig: metrics defaults
// ---------------------------------------------------------------------------

// TestLoadConfig_MetricsDefaults locks in the scrape + admin defaults so the
// admin listener never silently regresses to a non-loopback bind. The admin
// default is 127.0.0.1:9091 — a wildcard or all-interfaces default would
// undo the whole point of the two-listener split (issue #63).
func TestLoadConfig_MetricsDefaults(t *testing.T) {
	cfg, err := LoadConfig(writeConfigFile(t, validMinimalYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Metrics.ListenAddress != ":9090" {
		t.Errorf("metrics.listen_address = %q, want :9090", cfg.Metrics.ListenAddress)
	}
	if cfg.Metrics.AdminListenAddress != "127.0.0.1:9091" {
		t.Errorf("metrics.admin_listen_address = %q, want 127.0.0.1:9091", cfg.Metrics.AdminListenAddress)
	}
}

func TestLoadConfig_OTLPMetricsDefaults(t *testing.T) {
	yaml := `
clickhouse:
  host: localhost
  port: 9000
  database: default
exporters:
  otel:
    - collector_address: localhost:4317
      service_name: custom-service
monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 30
metrics:
  otlp:
    enabled: true
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Metrics.OTLP.InheritOTELConnection {
		t.Error("metrics.otlp.inherit_otel_connection default = false, want true")
	}
	if cfg.Metrics.OTLP.IntervalSeconds != 10 {
		t.Errorf("metrics.otlp.interval_seconds default = %d, want 10", cfg.Metrics.OTLP.IntervalSeconds)
	}
	if cfg.Metrics.OTLP.Host == "" {
		t.Error("metrics.otlp.host default should use os.Hostname")
	}
	if cfg.Metrics.OTLP.ServiceName != "custom-service" {
		t.Errorf("metrics.otlp.service_name default = %q, want exporter service_name", cfg.Metrics.OTLP.ServiceName)
	}
}

func TestLoadConfig_OTLPMetricsExplicitStandalone(t *testing.T) {
	t.Setenv("OTLP_HOST", "node-a")
	yaml := `
clickhouse:
  host: localhost
  port: 9000
  database: default
exporters:
  otel:
    - collector_address: localhost:4317
monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 30
metrics:
  otlp:
    enabled: true
    inherit_otel_connection: false
    collector_address: metrics-collector:4317
    secure: true
    host: ${OTLP_HOST}
    service_name: metrics-service
    interval_seconds: 15
    rename:
      spans_exported: myco.clickdog.spans_exported
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Metrics.OTLP.InheritOTELConnection {
		t.Error("metrics.otlp.inherit_otel_connection = true, want explicit false")
	}
	if cfg.Metrics.OTLP.CollectorAddress != "metrics-collector:4317" {
		t.Errorf("collector_address = %q", cfg.Metrics.OTLP.CollectorAddress)
	}
	if !cfg.Metrics.OTLP.Secure {
		t.Error("secure = false, want true")
	}
	if cfg.Metrics.OTLP.Host != "node-a" {
		t.Errorf("host = %q, want env-expanded node-a", cfg.Metrics.OTLP.Host)
	}
	if cfg.Metrics.OTLP.ServiceName != "metrics-service" {
		t.Errorf("service_name = %q, want metrics-service", cfg.Metrics.OTLP.ServiceName)
	}
	if cfg.Metrics.OTLP.IntervalSeconds != 15 {
		t.Errorf("interval_seconds = %d, want 15", cfg.Metrics.OTLP.IntervalSeconds)
	}
	if got := cfg.Metrics.OTLP.Rename["spans_exported"]; got != "myco.clickdog.spans_exported" {
		t.Errorf("rename[spans_exported] = %q", got)
	}
}
