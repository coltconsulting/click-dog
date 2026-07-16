package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// expandEnvVars
// ---------------------------------------------------------------------------

func TestExpandEnvVars(t *testing.T) {
	// Set up test environment variables
	t.Setenv("TEST_VAR", "test_value")
	t.Setenv("TEST_HOST", "myhost.example.com")

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "braced format",
			input:    "${TEST_VAR}",
			expected: "test_value",
		},
		{
			name:     "dollar format",
			input:    "$TEST_VAR",
			expected: "test_value",
		},
		{
			name:     "mixed with text",
			input:    "prefix_${TEST_VAR}_suffix",
			expected: "prefix_test_value_suffix",
		},
		{
			name:     "multiple vars",
			input:    "${TEST_HOST}:${TEST_VAR}",
			expected: "myhost.example.com:test_value",
		},
		{
			name:     "unset variable returns empty",
			input:    "${UNSET_VAR}",
			expected: "",
		},
		{
			name:     "no variables",
			input:    "plain_string",
			expected: "plain_string",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "partial match with unset",
			input:    "user:${UNSET_PASSWORD}@host",
			expected: "user:@host",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := expandEnvVars(tt.input)
			if result != tt.expected {
				t.Errorf("expandEnvVars(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// writeConfigFile writes YAML content to a temp file and returns its path.
func writeConfigFile(t *testing.T, yaml string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	return path
}

// validMinimalYAML is the smallest config that should pass LoadConfig.
const validMinimalYAML = `
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
`

// validFullYAML exercises every field LoadConfig knows about.
const validFullYAML = `
clickhouse:
  host: ch.example.com
  port: 9440
  database: analytics
  username: reader
  password: s3cret
  secure: true
  insecure_skip_verify: false
  ca_cert: /etc/ssl/ca.pem
  cluster: prod
  use_cluster_queries: true
  max_open_conns: 4
  max_idle_conns: 2
  query_timeout_s: 60
  max_memory_usage: 209715200
exporters:
  otel:
    - collector_address: otel.example.com:4317
      service_name: my-service
      max_query_length: 50000
      secure: true
      insecure_skip_verify: false
      ca_cert: /etc/ssl/otel-ca.pem
      client_cert: /etc/ssl/client.pem
      client_key: /etc/ssl/client-key.pem
monitor:
  enabled: true
  min_trace_duration_ms: 500
  min_span_duration_ms: 100
  max_trace_duration_ms: 60000
  max_span_duration_ms: 30000
  max_query_length: 50000
  check_interval_s: 15
  lookback_buffer_s: 20
  lookback_s: 35
  max_spans_per_cycle: 5000
  batch_size: 200
  batch_delay_ms: 50
  export_timeout_s: 45
  dedup_cache_size: 50000
  circuit_breaker:
    enabled: true
    failure_threshold: 5
    success_threshold: 2
    reset_timeout_s: 120
  backoff:
    enabled: true
    max_interval_s: 600
    backoff_factor: 3.0
filters:
  whitelist_operations:
    - "DB::Interpreter*::execute()"
  blacklist_operations:
    - "MergeTreeIndex"
    - "VFSWrite"
  whitelist_ips:
    - "10.0.0.0/8"
    - "192.168.1.100"
  blacklist_queries:
    - "^SELECT 1$"
ha:
  keeper:
    hosts:
      - "keeper-01:9181"
      - "keeper-02:9181"
    session_timeout_s: 10
    base_path: "/click-dog/election"
log_level: debug
log_file: /var/log/click-dog.log
`

// ---------------------------------------------------------------------------
// LoadConfig: valid configs (full config parsing)
// ---------------------------------------------------------------------------

func TestLoadConfig_ValidFull(t *testing.T) {
	cfg, err := LoadConfig(writeConfigFile(t, validFullYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Spot-check explicitly-set values are not overwritten by defaults
	if cfg.Exporters.OTEL[0].ServiceName != "my-service" {
		t.Errorf("service_name = %q, want my-service", cfg.Exporters.OTEL[0].ServiceName)
	}
	if cfg.Monitor.LookbackS != 35 {
		t.Errorf("lookback_s = %d, want 35 (explicit override)", cfg.Monitor.LookbackS)
	}
	if cfg.Monitor.LookbackBufferS != 20 {
		t.Errorf("lookback_buffer_s = %d, want 20", cfg.Monitor.LookbackBufferS)
	}
	if cfg.ClickHouse.Port != 9440 {
		t.Errorf("port = %d, want 9440", cfg.ClickHouse.Port)
	}
	if cfg.Monitor.Backoff.BackoffFactor != 3.0 {
		t.Errorf("backoff_factor = %f, want 3.0", cfg.Monitor.Backoff.BackoffFactor)
	}
	if cfg.Monitor.ExportTimeoutS != 45 {
		t.Errorf("export_timeout_s = %d, want 45", cfg.Monitor.ExportTimeoutS)
	}
	if len(cfg.Filters.BlacklistOperations) != 2 {
		t.Errorf("blacklist_operations = %d items, want 2", len(cfg.Filters.BlacklistOperations))
	}
}

// ---------------------------------------------------------------------------
// LoadConfig: env var expansion through LoadConfig
// ---------------------------------------------------------------------------

func TestLoadConfig_EnvVarExpansion(t *testing.T) {
	t.Setenv("CD_TEST_HOST", "env-host.example.com")
	t.Setenv("CD_TEST_PASS", "supersecret")
	t.Setenv("CD_TEST_USER", "admin")
	t.Setenv("CD_TEST_SERVICE", "env-service")

	yaml := `
clickhouse:
  host: ${CD_TEST_HOST}
  port: 9000
  database: default
  username: $CD_TEST_USER
  password: ${CD_TEST_PASS}
exporters:
  otel:
    - collector_address: localhost:4317
      service_name: ${CD_TEST_SERVICE}
monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 30
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ClickHouse.Host != "env-host.example.com" {
		t.Errorf("host = %q, want env-host.example.com", cfg.ClickHouse.Host)
	}
	if cfg.ClickHouse.Username != "admin" {
		t.Errorf("username = %q, want admin", cfg.ClickHouse.Username)
	}
	if cfg.ClickHouse.Password != "supersecret" {
		t.Errorf("password = %q, want supersecret", cfg.ClickHouse.Password)
	}
	if cfg.Exporters.OTEL[0].ServiceName != "env-service" {
		t.Errorf("service_name = %q, want env-service", cfg.Exporters.OTEL[0].ServiceName)
	}
}

func TestLoadConfig_UnsetEnvVarBecomesEmpty(t *testing.T) {
	_ = os.Unsetenv("CD_NEVER_SET_VAR")
	yaml := `
clickhouse:
  host: localhost
  port: 9000
  database: default
  password: ${CD_NEVER_SET_VAR}
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
	if cfg.ClickHouse.Password != "" {
		t.Errorf("password = %q, want empty string for unset env var", cfg.ClickHouse.Password)
	}
}

// ---------------------------------------------------------------------------
// LoadConfig: YAML parse errors
// ---------------------------------------------------------------------------

func TestLoadConfig_InvalidYAML(t *testing.T) {
	_, err := LoadConfig(writeConfigFile(t, "{{{{not yaml"))
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
	if !strings.Contains(err.Error(), "failed to parse config") {
		t.Errorf("error = %q, want it to contain 'failed to parse config'", err.Error())
	}
}

func TestLoadConfig_MissingFile(t *testing.T) {
	_, err := LoadConfig("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), "failed to read config file") {
		t.Errorf("error = %q, want it to contain 'failed to read config file'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Exporters config: backward compat, multi-sink, Splunk HEC env expansion
// ---------------------------------------------------------------------------

func TestLoadConfig_MultiSink(t *testing.T) {
	yaml := `
clickhouse:
  host: localhost
  port: 9000
  database: default
exporters:
  otel:
    - collector_address: otel1:4317
      service_name: svc1
    - collector_address: otel2:4317
      service_name: svc2
  splunk_hec:
    - endpoint: https://splunk:8088
      token: my-token
monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 30
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Exporters.OTEL) != 2 {
		t.Fatalf("expected 2 OTEL exporters, got %d", len(cfg.Exporters.OTEL))
	}
	if len(cfg.Exporters.SplunkHEC) != 1 {
		t.Fatalf("expected 1 Splunk HEC exporter, got %d", len(cfg.Exporters.SplunkHEC))
	}
	if cfg.Exporters.OTEL[0].CollectorAddress != "otel1:4317" {
		t.Errorf("first exporter = %q, want otel1:4317", cfg.Exporters.OTEL[0].CollectorAddress)
	}
}

func TestLoadConfig_SplunkHEC_EnvExpansion(t *testing.T) {
	t.Setenv("SPLUNK_TOKEN", "env-secret-token")
	t.Setenv("SPLUNK_URL", "https://splunk.internal:8088")

	yaml := `
clickhouse:
  host: localhost
  port: 9000
  database: default
exporters:
  splunk_hec:
    - endpoint: ${SPLUNK_URL}
      token: ${SPLUNK_TOKEN}
monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 30
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Exporters.SplunkHEC[0].Token != "env-secret-token" {
		t.Errorf("token = %q, want env-secret-token", cfg.Exporters.SplunkHEC[0].Token)
	}
	if cfg.Exporters.SplunkHEC[0].Endpoint != "https://splunk.internal:8088" {
		t.Errorf("endpoint = %q, want https://splunk.internal:8088", cfg.Exporters.SplunkHEC[0].Endpoint)
	}
}

// ---------------------------------------------------------------------------
// LoadConfig: redact_queries parsing
// ---------------------------------------------------------------------------

func TestLoadConfig_RedactQueries(t *testing.T) {
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
filters:
  redact_queries:
    - pattern: "(?i)identified\\s+by\\s+'[^']*'"
      replacement: "IDENTIFIED BY '[REDACTED]'"
    - pattern: "(?i)password\\s*=\\s*'[^']*'"
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Filters.RedactQueries) != 2 {
		t.Fatalf("expected 2 redaction rules, got %d", len(cfg.Filters.RedactQueries))
	}
	if cfg.Filters.RedactQueries[0].Replacement != "IDENTIFIED BY '[REDACTED]'" {
		t.Errorf("first rule replacement = %q", cfg.Filters.RedactQueries[0].Replacement)
	}
	if cfg.Filters.RedactQueries[1].Replacement != "" {
		t.Errorf("second rule replacement should be empty (defaults at filter init), got %q", cfg.Filters.RedactQueries[1].Replacement)
	}
}

// ---------------------------------------------------------------------------
// LoadConfig: canary explicit config
// ---------------------------------------------------------------------------

func TestLoadConfig_CanaryExplicit(t *testing.T) {
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
  canary:
    enabled: true
    threshold_duration_ms: 30000
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Monitor.Canary.Enabled {
		t.Error("canary should be enabled")
	}
	if cfg.Monitor.Canary.ThresholdDurationMs != 30000 {
		t.Errorf("canary threshold = %d, want 30000", cfg.Monitor.Canary.ThresholdDurationMs)
	}
}

// LogRotation defaults vs. explicit-zero handling — see security review §7b.
// An omitted key takes the default; an explicit `0` is a config error
// (would otherwise disable rotation and produce unbounded log growth).

func TestLoadConfig_LogRotation_DefaultsWhenOmitted(t *testing.T) {
	cfg, err := LoadConfig(writeConfigFile(t, validMinimalYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LogRotation.MaxSizeMB != 100 {
		t.Errorf("MaxSizeMB = %d, want default 100", cfg.LogRotation.MaxSizeMB)
	}
	if cfg.LogRotation.MaxFiles != 3 {
		t.Errorf("MaxFiles = %d, want default 3", cfg.LogRotation.MaxFiles)
	}
}

func TestLoadConfig_LogRotation_ExplicitZeroRejected(t *testing.T) {
	tests := []struct {
		name   string
		yaml   string
		substr string
	}{
		{
			name: "max_size_mb explicit 0",
			yaml: validMinimalYAML + `
log_rotation:
  max_size_mb: 0
  max_files: 5
`,
			substr: "log_rotation.max_size_mb",
		},
		{
			name: "max_files explicit 0",
			yaml: validMinimalYAML + `
log_rotation:
  max_size_mb: 50
  max_files: 0
`,
			substr: "log_rotation.max_files",
		},
		{
			name: "both explicit 0",
			yaml: validMinimalYAML + `
log_rotation:
  max_size_mb: 0
  max_files: 0
`,
			substr: "log_rotation.max_size_mb",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadConfig(writeConfigFile(t, tt.yaml))
			if err == nil {
				t.Fatal("expected error for explicit-zero log_rotation, got nil")
			}
			if !strings.Contains(err.Error(), tt.substr) {
				t.Errorf("error should mention %q, got: %v", tt.substr, err)
			}
		})
	}
}

func TestLoadConfig_LogRotation_ExplicitPositiveRespected(t *testing.T) {
	yaml := validMinimalYAML + `
log_rotation:
  max_size_mb: 50
  max_files: 5
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LogRotation.MaxSizeMB != 50 {
		t.Errorf("MaxSizeMB = %d, want 50", cfg.LogRotation.MaxSizeMB)
	}
	if cfg.LogRotation.MaxFiles != 5 {
		t.Errorf("MaxFiles = %d, want 5", cfg.LogRotation.MaxFiles)
	}
}

// ---------------------------------------------------------------------------
// LoadConfig: config-validation hardening (#161)
// ---------------------------------------------------------------------------

// minimalNoMonitorToggles omits monitor.enabled and check_interval_s so the
// defaulting paths can be exercised.
const minimalNoMonitorToggles = `
clickhouse:
  host: localhost
  port: 9000
  database: default
exporters:
  otel:
    - collector_address: localhost:4317
monitor:
  min_trace_duration_ms: 1000
`

func TestLoadConfig_StrictRejectsUnknownKey(t *testing.T) {
	tests := []struct {
		name  string
		yaml  string
		field string
	}{
		{
			name: "nested typo in monitor",
			yaml: `
clickhouse:
  host: localhost
  port: 9000
  database: default
exporters:
  otel:
    - collector_address: localhost:4317
monitor:
  min_trace_duration_ms: 1000
  check_intervla_s: 30
`,
			field: "check_intervla_s",
		},
		{
			name:  "unknown top-level key",
			yaml:  minimalNoMonitorToggles + "totally_unknown_key: true\n",
			field: "totally_unknown_key",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(writeConfigFile(t, tc.yaml))
			if err == nil {
				t.Fatalf("expected error for unknown key %q, got nil", tc.field)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error should name the offending key %q, got: %v", tc.field, err)
			}
		})
	}
}

// TestLoadConfig_RemovedHAEnabledKeyGetsTargetedError pins the upgrade-path
// error for the removed ha.enabled key (#237): the strict decoder's generic
// "field enabled not found" reads like a typo, so LoadConfig must name the
// removed key and point at ha.keeper.hosts instead.
func TestLoadConfig_RemovedHAEnabledKeyGetsTargetedError(t *testing.T) {
	for _, value := range []string{"true", "false"} {
		t.Run("enabled_"+value, func(t *testing.T) {
			yaml := minimalNoMonitorToggles + "ha:\n  enabled: " + value + "\n"
			_, err := LoadConfig(writeConfigFile(t, yaml))
			if err == nil {
				t.Fatal("expected error for removed ha.enabled key, got nil")
			}
			if !strings.Contains(err.Error(), "ha.enabled has been removed") {
				t.Errorf("error should explain the removal, got: %v", err)
			}
			if !strings.Contains(err.Error(), "ha.keeper.hosts") {
				t.Errorf("error should point at ha.keeper.hosts, got: %v", err)
			}
		})
	}
}

func TestLoadConfig_MonitorEnabledDefaultsTrue(t *testing.T) {
	cfg, err := LoadConfig(writeConfigFile(t, minimalNoMonitorToggles))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Monitor.Enabled {
		t.Errorf("monitor.enabled should default to true when omitted, got false")
	}
}

func TestLoadConfig_MonitorEnabledExplicitFalsePreserved(t *testing.T) {
	yaml := `
clickhouse:
  host: localhost
  port: 9000
  database: default
exporters:
  otel:
    - collector_address: localhost:4317
monitor:
  enabled: false
  min_trace_duration_ms: 1000
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Monitor.Enabled {
		t.Errorf("explicit monitor.enabled: false should be preserved, got true")
	}
}

func TestLoadConfig_CheckIntervalDefaultsWhenOmitted(t *testing.T) {
	cfg, err := LoadConfig(writeConfigFile(t, minimalNoMonitorToggles))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Monitor.CheckIntervalS != 30 {
		t.Errorf("check_interval_s should default to 30 when omitted, got %d", cfg.Monitor.CheckIntervalS)
	}
}

func TestLoadConfig_CheckIntervalZeroRejectedWhenEnabled(t *testing.T) {
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
  check_interval_s: 0
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for check_interval_s: 0 with monitor.enabled: true, got nil")
	}
	if !strings.Contains(err.Error(), "check_interval_s must be greater than 0") {
		t.Errorf("error should explain the zero-interval rejection, got: %v", err)
	}
}

func TestLoadConfig_CheckIntervalZeroAllowedWhenDisabled(t *testing.T) {
	// A backfill-only deployment disables scheduled mode; the ticker is never
	// created, so an explicit zero interval must not block loading.
	yaml := `
clickhouse:
  host: localhost
  port: 9000
  database: default
exporters:
  otel:
    - collector_address: localhost:4317
monitor:
  enabled: false
  min_trace_duration_ms: 1000
  check_interval_s: 0
`
	if _, err := LoadConfig(writeConfigFile(t, yaml)); err != nil {
		t.Fatalf("check_interval_s: 0 should be allowed when monitor.enabled: false, got: %v", err)
	}
}

func TestLoadConfig_EmptyExporterCollectorAddressRejected(t *testing.T) {
	yaml := `
clickhouse:
  host: localhost
  port: 9000
  database: default
exporters:
  otel:
    - service_name: foo
monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 30
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for exporters.otel[] entry with empty collector_address, got nil")
	}
	if !strings.Contains(err.Error(), "exporters.otel[0].collector_address is required") {
		t.Errorf("error should flag the missing collector_address, got: %v", err)
	}
}

func TestLoadConfig_MinTraceDurationRequiredWhenEnabled(t *testing.T) {
	// monitor.enabled defaults to true, so a config that omits
	// min_trace_duration_ms must not silently start scheduled mode with a
	// zero (match-everything) trace filter.
	yaml := `
clickhouse:
  host: localhost
  port: 9000
  database: default
exporters:
  otel:
    - collector_address: localhost:4317
monitor:
  check_interval_s: 30
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for missing min_trace_duration_ms with scheduled mode enabled, got nil")
	}
	if !strings.Contains(err.Error(), "min_trace_duration_ms must be greater than 0") {
		t.Errorf("error should flag the missing trace filter, got: %v", err)
	}
}

func TestLoadConfig_MinTraceDurationZeroAllowedWhenDisabled(t *testing.T) {
	// Backfill-only deployment: scheduled mode is off, so the trace filter is
	// unused and a zero/omitted value must not block loading.
	yaml := `
clickhouse:
  host: localhost
  port: 9000
  database: default
exporters:
  otel:
    - collector_address: localhost:4317
monitor:
  enabled: false
`
	if _, err := LoadConfig(writeConfigFile(t, yaml)); err != nil {
		t.Fatalf("min_trace_duration_ms 0 should be allowed when monitor.enabled: false, got: %v", err)
	}
}
