package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// helper: build a Config that passes Validate()
// ---------------------------------------------------------------------------

func validConfig() *Config {
	return &Config{
		ClickHouse: ClickHouseConfig{
			Host:           "localhost",
			Port:           9000,
			Database:       "default",
			MaxOpenConns:   2,
			MaxIdleConns:   1,
			QueryTimeoutS:  30,
			MaxMemoryUsage: 104857600,
		},
		Exporters: ExportersConfig{
			OTEL: []OTELConfig{{
				CollectorAddress: "localhost:4317",
				ServiceName:      "test",
				MaxQueryLength:   100000,
			}},
		},
		Monitor: MonitorConfig{
			Enabled:            true,
			MinTraceDurationMs: 1000,
			CheckIntervalS:     30,
			LookbackS:          40,
			LookbackBufferS:    10,
			MaxSpansPerCycle:   1000,
			DedupCacheSize:     10000,
			MaxQueryLength:     100000,
			CircuitBreaker: CircuitBreakerConfig{
				FailureThreshold: 3,
				SuccessThreshold: 1,
				ResetTimeoutS:    60,
			},
			Backoff: BackoffConfig{
				MaxIntervalS:  300,
				BackoffFactor: 2.0,
			},
		},
		LogLevel: "info",
		LogRotation: LogRotationConfig{
			MaxSizeMB: 100,
			MaxFiles:  3,
		},
	}
}

// ---------------------------------------------------------------------------
// Validate: individual invalid fields
// ---------------------------------------------------------------------------

func TestValidate_ClickHousePort(t *testing.T) {
	tests := []struct {
		name    string
		port    int
		wantErr bool
	}{
		{"port 0 invalid", 0, true},
		{"port 9000 valid", 9000, false},
		{"port 65535 valid", 65535, false},
		{"port -1 invalid", -1, true},
		{"port 65536 invalid", 65536, true},
		{"port 100000 invalid", 100000, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.ClickHouse.Port = tt.port
			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("port=%d: err=%v, wantErr=%v", tt.port, err, tt.wantErr)
			}
			if tt.wantErr && err != nil && !strings.Contains(err.Error(), "clickhouse.port") {
				t.Errorf("error should mention clickhouse.port, got: %v", err)
			}
		})
	}
}

func TestValidate_NegativeNumericFields(t *testing.T) {
	// Each entry: field description, mutator that sets it to -1, expected error substring
	tests := []struct {
		name   string
		mutate func(*Config)
		substr string
	}{
		{"max_open_conns", func(c *Config) { c.ClickHouse.MaxOpenConns = -1 }, "max_open_conns"},
		{"max_idle_conns", func(c *Config) { c.ClickHouse.MaxIdleConns = -1 }, "max_idle_conns"},
		{"query_timeout_s", func(c *Config) { c.ClickHouse.QueryTimeoutS = -1 }, "query_timeout_s"},
		{"max_memory_usage", func(c *Config) { c.ClickHouse.MaxMemoryUsage = -1 }, "max_memory_usage"},
		{"min_trace_duration_ms", func(c *Config) { c.Monitor.MinTraceDurationMs = -1 }, "min_trace_duration_ms"},
		{"min_span_duration_ms", func(c *Config) { c.Monitor.MinSpanDurationMs = -1 }, "min_span_duration_ms"},
		{"max_trace_duration_ms", func(c *Config) { c.Monitor.MaxTraceDurationMs = -1 }, "max_trace_duration_ms"},
		{"max_span_duration_ms", func(c *Config) { c.Monitor.MaxSpanDurationMs = -1 }, "max_span_duration_ms"},
		{"check_interval_s", func(c *Config) { c.Monitor.CheckIntervalS = -1 }, "check_interval_s"},
		{"lookback_s", func(c *Config) { c.Monitor.LookbackS = -1 }, "lookback_s"},
		{"lookback_buffer_s", func(c *Config) { c.Monitor.LookbackBufferS = -1 }, "lookback_buffer_s"},
		{"max_spans_per_cycle", func(c *Config) { c.Monitor.MaxSpansPerCycle = -1 }, "max_spans_per_cycle"},
		{"batch_size", func(c *Config) { c.Monitor.BatchSize = -1 }, "batch_size"},
		{"batch_delay_ms", func(c *Config) { c.Monitor.BatchDelayMs = -1 }, "batch_delay_ms"},
		{"export_timeout_s", func(c *Config) { c.Monitor.ExportTimeoutS = -1 }, "export_timeout_s"},
		{"dedup_cache_size", func(c *Config) { c.Monitor.DedupCacheSize = -1 }, "dedup_cache_size"},
		{"cb failure_threshold", func(c *Config) { c.Monitor.CircuitBreaker.FailureThreshold = -1 }, "failure_threshold"},
		{"cb success_threshold", func(c *Config) { c.Monitor.CircuitBreaker.SuccessThreshold = -1 }, "success_threshold"},
		{"cb reset_timeout_s", func(c *Config) { c.Monitor.CircuitBreaker.ResetTimeoutS = -1 }, "reset_timeout_s"},
		{"backoff max_interval_s", func(c *Config) { c.Monitor.Backoff.MaxIntervalS = -1 }, "max_interval_s"},
		{"backoff backoff_factor", func(c *Config) { c.Monitor.Backoff.BackoffFactor = -1 }, "backoff_factor"},
		{"monitor max_query_length", func(c *Config) { c.Monitor.MaxQueryLength = -1 }, "monitor.max_query_length"},
		{"canary threshold_duration_ms", func(c *Config) { c.Monitor.Canary.ThresholdDurationMs = -1 }, "canary.threshold_duration_ms"},
		{"exporters.otel max_query_length", func(c *Config) { c.Exporters.OTEL[0].MaxQueryLength = -1 }, "exporters.otel[0].max_query_length"},
		{"log_rotation max_size_mb", func(c *Config) { c.LogRotation.MaxSizeMB = -1 }, "log_rotation.max_size_mb"},
		{"log_rotation max_files", func(c *Config) { c.LogRotation.MaxFiles = -1 }, "log_rotation.max_files"},
		{"topology_audit interval_s", func(c *Config) { c.Monitor.TopologyAudit.IntervalS = -1 }, "topology_audit.interval_s"},
		{"topology_audit debounce_count", func(c *Config) { c.Monitor.TopologyAudit.DebounceCount = -1 }, "topology_audit.debounce_count"},
		{"topology_audit query_log_lookback_minutes", func(c *Config) { c.Monitor.TopologyAudit.QueryLogLookbackMinutes = -1 }, "topology_audit.query_log_lookback_minutes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error for negative %s", tt.name)
			}
			if !strings.Contains(err.Error(), tt.substr) {
				t.Errorf("error should mention %q, got: %v", tt.substr, err)
			}
		})
	}
}

func TestValidate_OTLPMetrics(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		substr string
	}{
		{
			name: "inherit requires otel exporter",
			mutate: func(c *Config) {
				c.Exporters.OTEL = nil
				c.Exporters.SplunkHEC = []SplunkHECConfig{{
					Endpoint:       "https://splunk.example:8088",
					Token:          "token",
					MaxQueryLength: 100000,
				}}
				c.Metrics.OTLP = OTLPMetricsConfig{
					Enabled:               true,
					InheritOTELConnection: true,
					IntervalSeconds:       10,
				}
			},
			substr: "metrics.otlp.inherit_otel_connection",
		},
		{
			name: "standalone requires collector address",
			mutate: func(c *Config) {
				c.Metrics.OTLP = OTLPMetricsConfig{
					Enabled:               true,
					InheritOTELConnection: false,
					IntervalSeconds:       10,
				}
			},
			substr: "metrics.otlp.collector_address",
		},
		{
			name: "positive interval required",
			mutate: func(c *Config) {
				c.Metrics.OTLP = OTLPMetricsConfig{
					Enabled:               true,
					InheritOTELConnection: true,
					IntervalSeconds:       0,
				}
			},
			substr: "metrics.otlp.interval_seconds",
		},
		{
			name: "duplicate rename target rejected",
			mutate: func(c *Config) {
				c.Metrics.OTLP = OTLPMetricsConfig{
					Enabled:               true,
					InheritOTELConnection: true,
					IntervalSeconds:       10,
					Rename: map[string]string{
						"spans_exported":   "my.metric",
						"spans_duplicates": "my.metric",
					},
				}
			},
			substr: "metrics.otlp.rename maps both",
		},
		{
			name: "unknown rename key rejected",
			mutate: func(c *Config) {
				c.Metrics.OTLP = OTLPMetricsConfig{
					Enabled:               true,
					InheritOTELConnection: true,
					IntervalSeconds:       10,
					Rename: map[string]string{
						"spnas_exported": "my.metric",
					},
				}
			},
			substr: "metrics.otlp.rename.spnas_exported",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected validation error")
			}
			if !strings.Contains(err.Error(), tt.substr) {
				t.Fatalf("error should mention %q, got: %v", tt.substr, err)
			}
		})
	}
}

func TestValidate_OTLPMetricsValidRename(t *testing.T) {
	cfg := validConfig()
	cfg.Metrics.OTLP = OTLPMetricsConfig{
		Enabled:               true,
		InheritOTELConnection: true,
		IntervalSeconds:       10,
		Rename: map[string]string{
			"spans_exported": "myco.clickdog.spans_exported",
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate with valid metrics.otlp.rename returned error: %v", err)
	}
}

func TestValidate_TopologyAuditEnabledRequiresPositiveKnobs(t *testing.T) {
	// interval_s and debounce_count must be > 0 when the audit is enabled (a
	// zero interval panics time.NewTicker; a zero debounce trips on tick 1).
	t.Run("zero interval when enabled", func(t *testing.T) {
		cfg := validConfig()
		cfg.Monitor.TopologyAudit.Enabled = true
		cfg.Monitor.TopologyAudit.IntervalS = 0
		cfg.Monitor.TopologyAudit.DebounceCount = 2
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "topology_audit.interval_s") {
			t.Errorf("expected interval_s error, got: %v", err)
		}
	})
	t.Run("zero debounce when enabled", func(t *testing.T) {
		cfg := validConfig()
		cfg.Monitor.TopologyAudit.Enabled = true
		cfg.Monitor.TopologyAudit.IntervalS = 300
		cfg.Monitor.TopologyAudit.DebounceCount = 0
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "topology_audit.debounce_count") {
			t.Errorf("expected debounce_count error, got: %v", err)
		}
	})
	t.Run("zero knobs OK when disabled", func(t *testing.T) {
		cfg := validConfig()
		cfg.Monitor.TopologyAudit.Enabled = false
		cfg.Monitor.TopologyAudit.IntervalS = 0
		cfg.Monitor.TopologyAudit.DebounceCount = 0
		if err := cfg.Validate(); err != nil {
			t.Errorf("disabled audit with zero knobs should pass, got: %v", err)
		}
	})
}

func TestValidate_MaxSpansPerCycleCeiling(t *testing.T) {
	// The ceiling must match the query-side clamp; values above it are rejected
	// at config load rather than silently capped at query time.
	t.Run("at ceiling is valid", func(t *testing.T) {
		cfg := validConfig()
		cfg.Monitor.MaxSpansPerCycle = MaxSpansPerCycleCeiling
		if err := cfg.Validate(); err != nil {
			t.Fatalf("max_spans_per_cycle=%d should be valid, got: %v", MaxSpansPerCycleCeiling, err)
		}
	})
	t.Run("above ceiling is rejected", func(t *testing.T) {
		cfg := validConfig()
		cfg.Monitor.MaxSpansPerCycle = MaxSpansPerCycleCeiling + 1
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("max_spans_per_cycle=%d should be rejected", MaxSpansPerCycleCeiling+1)
		}
		if !strings.Contains(err.Error(), "max_spans_per_cycle") {
			t.Errorf("error should mention max_spans_per_cycle, got: %v", err)
		}
	})
}

func TestValidate_MultipleErrors(t *testing.T) {
	cfg := validConfig()
	cfg.ClickHouse.Port = -1
	cfg.Monitor.MinTraceDurationMs = -1
	cfg.Monitor.LookbackS = -1

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// All three fields should appear in the error
	for _, substr := range []string{"clickhouse.port", "min_trace_duration_ms", "lookback_s"} {
		if !strings.Contains(err.Error(), substr) {
			t.Errorf("error should mention %q, got: %v", substr, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Validate: log level
// ---------------------------------------------------------------------------

func TestValidate_LogLevel(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error"} {
		t.Run("valid_"+level, func(t *testing.T) {
			cfg := validConfig()
			cfg.LogLevel = level
			if err := cfg.Validate(); err != nil {
				t.Errorf("log_level=%q should be valid, got: %v", level, err)
			}
		})
	}
	for _, level := range []string{"INFO", "Warning", "trace", "fatal", ""} {
		t.Run("invalid_"+level, func(t *testing.T) {
			cfg := validConfig()
			cfg.LogLevel = level
			err := cfg.Validate()
			if err == nil {
				t.Errorf("log_level=%q should be invalid", level)
			}
			if err != nil && !strings.Contains(err.Error(), "log_level") {
				t.Errorf("error should mention log_level, got: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Validate: HA / Keeper
// ---------------------------------------------------------------------------

func TestValidate_HA_NoHostsMeansInactive(t *testing.T) {
	// Election is driven by the presence of Keeper hosts; with none, HA is
	// simply inactive (no error) and Active() reports false. There is no
	// separate enable flag that could be on without hosts.
	cfg := validConfig()
	cfg.HA.Keeper.Hosts = nil

	if cfg.HA.Active() {
		t.Error("HA.Active() should be false with no Keeper hosts")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("no Keeper hosts should be valid (HA inactive), got: %v", err)
	}
}

func TestValidate_HA_SessionTimeoutBoundaries(t *testing.T) {
	tests := []struct {
		name    string
		timeout int
		wantErr bool
		substr  string
	}{
		{"too low 1s", 1, true, "must be >= 5"},
		{"too low 4s", 4, true, "must be >= 5"},
		{"valid 5s", 5, false, ""},
		{"valid 10s", 10, false, ""},
		{"valid 30s", 30, false, ""},
		{"too high 31s", 31, true, "must be <= 30"},
		{"too high 60s", 60, true, "must be <= 30"},
		{"negative", -1, true, "cannot be negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.HA.Keeper.Hosts = []string{"keeper:9181"}
			cfg.HA.Keeper.SessionTimeout = tt.timeout
			cfg.HA.Keeper.BasePath = "/click-dog/election"

			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("timeout=%d: err=%v, wantErr=%v", tt.timeout, err, tt.wantErr)
			}
			if tt.wantErr && err != nil && !strings.Contains(err.Error(), tt.substr) {
				t.Errorf("error should contain %q, got: %v", tt.substr, err)
			}
		})
	}
}

func TestValidate_HA_KeeperBasePath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
		substr  string
	}{
		{"valid /click-dog/election", "/click-dog/election", false, ""},
		{"valid /click-dog", "/click-dog", false, ""},
		{"valid /click-dog/custom/path", "/click-dog/custom/path", false, ""},
		{"forbidden /clickhouse/tables", "/clickhouse/tables", true, "/clickhouse"},
		{"forbidden /clickhouse", "/clickhouse", true, "/clickhouse"},
		{"wrong prefix /zookeeper/click-dog", "/zookeeper/click-dog", true, "must start with /click-dog/"},
		{"wrong prefix /other", "/other", true, "must start with /click-dog/"},
		{"empty path invalid", "", true, "must start with /click-dog/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.HA.Keeper.Hosts = []string{"keeper:9181"}
			cfg.HA.Keeper.SessionTimeout = 10
			cfg.HA.Keeper.BasePath = tt.path

			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("path=%q: err=%v, wantErr=%v", tt.path, err, tt.wantErr)
			}
			if tt.wantErr && err != nil && !strings.Contains(err.Error(), tt.substr) {
				t.Errorf("error should contain %q, got: %v", tt.substr, err)
			}
		})
	}
}

func TestValidate_HA_DisabledSkipsKeeperChecks(t *testing.T) {
	cfg := validConfig()
	cfg.HA.Keeper.Hosts = nil // no hosts → HA inactive → keeper checks skipped
	cfg.HA.Keeper.SessionTimeout = 0
	cfg.HA.Keeper.BasePath = "/wrong/prefix"

	if err := cfg.Validate(); err != nil {
		t.Errorf("HA inactive should skip keeper validation, got: %v", err)
	}
}

func TestLoadConfig_HA_KeeperSecurityWarnings(t *testing.T) {
	tests := []struct {
		name      string
		keeperYML string
		want      string
	}{
		{
			name: "missing auth warns about world ACLs",
			keeperYML: `
    hosts:
      - keeper:9181
`,
			want: "world ACLs",
		},
		{
			name: "auth without TLS warns about plaintext",
			keeperYML: `
    hosts:
      - keeper:9181
    auth_user: click-dog
    auth_password: secret
`,
			want: "plaintext connection",
		},
		{
			name: "auth with TLS has no keeper security warning",
			keeperYML: `
    hosts:
      - keeper:9181
    secure: true
    auth_user: click-dog
    auth_password: secret
`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yaml := validMinimalYAML + `
ha:
  keeper:` + tt.keeperYML
			cfg, err := LoadConfig(writeConfigFile(t, yaml))
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}

			got := strings.Join(cfg.ValidationWarnings, "\n")
			if tt.want == "" {
				if strings.Contains(got, "ha.keeper") {
					t.Fatalf("unexpected keeper security warning: %v", cfg.ValidationWarnings)
				}
				return
			}
			if !strings.Contains(got, tt.want) {
				t.Fatalf("ValidationWarnings = %v, want substring %q", cfg.ValidationWarnings, tt.want)
			}
		})
	}
}

func TestValidate_UseClusterQueriesRequiresClusterName(t *testing.T) {
	// use_cluster_queries with an empty cluster name silently falls back to the
	// local table, so a leader-gated deployment would drop every standby node's
	// spans. Validation must reject it.
	cfg := validConfig()
	cfg.ClickHouse.UseClusterQueries = true
	cfg.ClickHouse.Cluster = ""

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "clickhouse.cluster is required") {
		t.Fatalf("expected cluster-required error, got: %v", err)
	}

	cfg.ClickHouse.Cluster = "main"
	if err := cfg.Validate(); err != nil {
		t.Errorf("use_cluster_queries with a cluster name should be valid, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Validate: Health cluster (#19, phase 1)
// ---------------------------------------------------------------------------

func TestValidate_HealthCluster_RequiresHealthEnabled(t *testing.T) {
	cfg := validConfig()
	cfg.Health.Enabled = false
	cfg.Health.Cluster.Enabled = true
	cfg.Health.Cluster.Self = "node-0:8686"
	cfg.Health.Cluster.PeerTimeoutMs = 1000
	cfg.Health.Cluster.Peers = []string{"node-1:8686"}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error: health.cluster.enabled without health.enabled")
	}
	if !strings.Contains(err.Error(), "health.enabled") {
		t.Errorf("error should call out health.enabled, got: %v", err)
	}
}

func TestValidate_HealthCluster_RequiresSelf(t *testing.T) {
	// Without `self`, the leader can't identify its own peer slot — the
	// in-process /readyz wouldn't match any Peers entry, and HTTP fanout
	// would self-loop. Reject at config load so the misconfiguration is
	// caught before deployment.
	cfg := validConfig()
	cfg.Health.Enabled = true
	cfg.Health.Cluster.Enabled = true
	cfg.Health.Cluster.Self = ""
	cfg.Health.Cluster.PeerTimeoutMs = 1000
	cfg.Health.Cluster.Peers = []string{"node-1:8686"}

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "health.cluster.self") {
		t.Fatalf("expected health.cluster.self required error, got: %v", err)
	}
}

func TestValidate_HealthCluster_RequiresSelf_RejectsWhitespace(t *testing.T) {
	// A whitespace-only `self` is operationally identical to empty —
	// nothing in Peers will match it. Treat it like the missing case.
	cfg := validConfig()
	cfg.Health.Enabled = true
	cfg.Health.Cluster.Enabled = true
	cfg.Health.Cluster.Self = "   "
	cfg.Health.Cluster.PeerTimeoutMs = 1000

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "health.cluster.self") {
		t.Fatalf("expected health.cluster.self whitespace rejection, got: %v", err)
	}
}

func TestValidate_HealthCluster_NegativeTimeoutRejected(t *testing.T) {
	cfg := validConfig()
	cfg.Health.Enabled = true
	cfg.Health.Cluster.Enabled = true
	cfg.Health.Cluster.Self = "node-0:8686"
	cfg.Health.Cluster.PeerTimeoutMs = -1

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "peer_timeout_ms") {
		t.Fatalf("expected peer_timeout_ms negative error, got: %v", err)
	}
}

func TestValidate_HealthCluster_ZeroTimeoutAccepted(t *testing.T) {
	// Zero is valid at the schema level — LoadConfig replaces it with the
	// 3000ms default. Validate() runs AFTER defaults, so a literal 0 here
	// would only happen via direct struct construction; the test pins
	// that direct path behaves like "no override".
	cfg := validConfig()
	cfg.Health.Enabled = true
	cfg.Health.Cluster.Enabled = true
	cfg.Health.Cluster.Self = "node-0:8686"
	cfg.Health.Cluster.PeerTimeoutMs = 0

	if err := cfg.Validate(); err != nil {
		t.Errorf("zero peer_timeout_ms should be accepted (defaulting happens earlier), got: %v", err)
	}
}

func TestValidate_HealthCluster_EmptyPeerRejected(t *testing.T) {
	cfg := validConfig()
	cfg.Health.Enabled = true
	cfg.Health.Cluster.Enabled = true
	cfg.Health.Cluster.Self = "node-0:8686"
	cfg.Health.Cluster.PeerTimeoutMs = 1000
	cfg.Health.Cluster.Peers = []string{"node-1:8686", "  "}

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "health.cluster.peers[1]") {
		t.Fatalf("expected per-index empty-peer error, got: %v", err)
	}
}

func TestValidate_HealthCluster_RejectsSchemePrefix(t *testing.T) {
	// `http://node-1:8686` would silently produce `http://http://node-1:8686/readyz`
	// at fanout time and a cryptic dial error. Reject at config load.
	cfg := validConfig()
	cfg.Health.Enabled = true
	cfg.Health.Cluster.Enabled = true
	cfg.Health.Cluster.Self = "node-0:8686"
	cfg.Health.Cluster.PeerTimeoutMs = 1000
	cfg.Health.Cluster.Peers = []string{"http://node-1:8686"}

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "scheme") {
		t.Fatalf("expected scheme-prefix rejection, got: %v", err)
	}
}

func TestValidate_HealthCluster_RejectsPath(t *testing.T) {
	cfg := validConfig()
	cfg.Health.Enabled = true
	cfg.Health.Cluster.Enabled = true
	cfg.Health.Cluster.Self = "node-0:8686"
	cfg.Health.Cluster.PeerTimeoutMs = 1000
	cfg.Health.Cluster.Peers = []string{"node-1:8686/readyz"}

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "path") {
		t.Fatalf("expected path rejection, got: %v", err)
	}
}

func TestValidate_HealthCluster_RejectsMissingPort(t *testing.T) {
	cfg := validConfig()
	cfg.Health.Enabled = true
	cfg.Health.Cluster.Enabled = true
	cfg.Health.Cluster.Self = "node-0:8686"
	cfg.Health.Cluster.PeerTimeoutMs = 1000
	cfg.Health.Cluster.Peers = []string{"node-1"}

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "host:port") {
		t.Fatalf("expected host:port rejection, got: %v", err)
	}
}

func TestValidate_HealthCluster_AcceptsIPv6(t *testing.T) {
	// IPv6 host:port form is unusual but valid; SplitHostPort handles it.
	cfg := validConfig()
	cfg.Health.Enabled = true
	cfg.Health.Cluster.Enabled = true
	cfg.Health.Cluster.Self = "[::1]:8686"
	cfg.Health.Cluster.PeerTimeoutMs = 1000
	cfg.Health.Cluster.Peers = []string{"[fd00::1]:8686"}

	if err := cfg.Validate(); err != nil {
		t.Errorf("IPv6 host:port should validate, got: %v", err)
	}
}

func TestValidate_HealthCluster_RejectsSchemePrefixOnSelf(t *testing.T) {
	// Same defense applies to `self` — that string is matched against
	// peers entries and used as the response key, so a malformed value
	// would corrupt both paths.
	cfg := validConfig()
	cfg.Health.Enabled = true
	cfg.Health.Cluster.Enabled = true
	cfg.Health.Cluster.Self = "http://node-0:8686"
	cfg.Health.Cluster.PeerTimeoutMs = 1000

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "health.cluster.self") {
		t.Fatalf("expected self scheme rejection, got: %v", err)
	}
}

func TestValidate_HealthCluster_RejectsDuplicatePeers(t *testing.T) {
	// At fanout time, two goroutines would race to write nodes[peer]
	// and summary.Total would silently report one entry instead of two.
	// Always a typo — error at load.
	cfg := validConfig()
	cfg.Health.Enabled = true
	cfg.Health.Cluster.Enabled = true
	cfg.Health.Cluster.Self = "node-0:8686"
	cfg.Health.Cluster.PeerTimeoutMs = 1000
	cfg.Health.Cluster.Peers = []string{"node-1:8686", "node-2:8686", "node-1:8686"}

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicates peers[0]") {
		t.Fatalf("expected duplicate-peer error referencing peers[0], got: %v", err)
	}
}

func TestValidate_HealthCluster_RejectsDuplicatesAfterTrim(t *testing.T) {
	// Detection runs on the trimmed value, so "  node-1:8686  " and
	// "node-1:8686" are also a duplicate pair.
	cfg := validConfig()
	cfg.Health.Enabled = true
	cfg.Health.Cluster.Enabled = true
	cfg.Health.Cluster.Self = "node-0:8686"
	cfg.Health.Cluster.PeerTimeoutMs = 1000
	cfg.Health.Cluster.Peers = []string{"node-1:8686", "  node-1:8686  "}

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("expected whitespace-equivalent duplicate detection, got: %v", err)
	}
}

func TestValidate_HealthCluster_DisabledSkipsAllChecks(t *testing.T) {
	// When health.cluster.enabled is false, the other fields should not trigger
	// validation — operators may leave a partial-but-disabled stanza in
	// their config without it counting as a misconfiguration.
	cfg := validConfig()
	cfg.Health.Enabled = false
	cfg.Health.Cluster.Enabled = false
	cfg.Health.Cluster.PeerTimeoutMs = -999
	cfg.Health.Cluster.Peers = []string{""}

	if err := cfg.Validate(); err != nil {
		t.Errorf("health.cluster.enabled=false should bypass cluster checks, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Validate: Splunk HEC
// ---------------------------------------------------------------------------

func TestValidate_SplunkHEC_MissingFields(t *testing.T) {
	cfg := validConfig()
	cfg.Exporters.SplunkHEC = []SplunkHECConfig{
		{Endpoint: "", Token: ""},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for empty Splunk HEC fields")
	}
	if !strings.Contains(err.Error(), "endpoint") {
		t.Errorf("error should mention endpoint, got: %v", err)
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error should mention token, got: %v", err)
	}
}

func TestValidate_NoExporters(t *testing.T) {
	cfg := validConfig()
	cfg.Exporters.OTEL = nil
	cfg.Exporters.SplunkHEC = nil
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for no exporters")
	}
	if !strings.Contains(err.Error(), "at least one exporter") {
		t.Errorf("error should mention 'at least one exporter', got: %v", err)
	}
	if strings.Contains(err.Error(), "(otel or") {
		t.Errorf("error should not name the removed top-level otel key, got: %v", err)
	}
	for _, current := range []string{"exporters.otel", "exporters.splunk_hec"} {
		if !strings.Contains(err.Error(), current) {
			t.Errorf("error should point at %s, got: %v", current, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Validate: redact_queries
// ---------------------------------------------------------------------------

func TestValidate_RedactQueries_EmptyPattern(t *testing.T) {
	cfg := validConfig()
	cfg.Filters.RedactQueries = []RedactionRule{{Pattern: ""}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for empty redaction pattern")
	}
	if !strings.Contains(err.Error(), "redact_queries[0].pattern must not be empty") {
		t.Errorf("error should mention empty pattern, got: %v", err)
	}
}

func TestValidate_RedactQueries_InvalidRegex(t *testing.T) {
	cfg := validConfig()
	cfg.Filters.RedactQueries = []RedactionRule{{Pattern: "[invalid("}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for invalid redaction regex")
	}
	if !strings.Contains(err.Error(), "redact_queries[0].pattern is invalid regex") {
		t.Errorf("error should mention invalid regex, got: %v", err)
	}
	if strings.Contains(err.Error(), "requires at least one valid") {
		t.Errorf("invalid regex should not also report a missing-rule error, got: %v", err)
	}
}

func TestValidate_UserLists_RejectEmptyEntries(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		errPart string
	}{
		{
			name:    "whitelist contains empty string",
			mutate:  func(c *Config) { c.Filters.WhitelistUsers = []string{"app", ""} },
			errPart: "filters.whitelist_users[1] must not be empty",
		},
		{
			name:    "blacklist contains whitespace-only entry",
			mutate:  func(c *Config) { c.Filters.BlacklistUsers = []string{"   "} },
			errPart: "filters.blacklist_users[0] must not be empty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tt.errPart) {
				t.Errorf("error should mention %q, got: %v", tt.errPart, err)
			}
		})
	}
}

func TestValidate_UserLists_AcceptsPopulatedEntries(t *testing.T) {
	cfg := validConfig()
	cfg.Filters.WhitelistUsers = []string{"app_frontend", "app_analytics"}
	cfg.Filters.BlacklistUsers = []string{"patient_records"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("populated user lists should pass validation, got: %v", err)
	}
}

func TestValidate_UserFilter_RequiresEnrichment(t *testing.T) {
	// User filtering can't work without query_log enrichment — span_log has
	// no user attribute, so every span would resolve to "" (whitelist drops
	// all, blacklist passes all). The combination must be rejected.
	disabled := false
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{
			name: "whitelist with enrichment disabled",
			mutate: func(c *Config) {
				c.Filters.WhitelistUsers = []string{"app_frontend"}
				c.Monitor.EnrichFromQueryLog = &disabled
			},
		},
		{
			name: "blacklist with enrichment disabled",
			mutate: func(c *Config) {
				c.Filters.BlacklistUsers = []string{"patient_records"}
				c.Monitor.EnrichFromQueryLog = &disabled
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), "require query_log enrichment") {
				t.Errorf("error should mention enrichment requirement, got: %v", err)
			}
		})
	}
}

func TestValidate_UserFilter_PassesWithEnrichmentExplicitlyEnabled(t *testing.T) {
	enabled := true
	cfg := validConfig()
	cfg.Filters.WhitelistUsers = []string{"app_frontend"}
	cfg.Monitor.EnrichFromQueryLog = &enabled
	if err := cfg.Validate(); err != nil {
		t.Fatalf("user filter with enrichment explicitly enabled should pass, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Validate: positive baseline — the helpers must themselves produce a config
// that passes Validate. Without these every other test that mutates one field
// is implicitly relying on the helper, so a regression in the baseline shows
// up only as cascading failures elsewhere.
// ---------------------------------------------------------------------------

func TestValidate_ValidMinimalPasses(t *testing.T) {
	cfg := validConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validConfig() helper should pass Validate(), got: %v", err)
	}
}

func TestValidate_ValidFullPasses(t *testing.T) {
	cfg, err := LoadConfig(writeConfigFile(t, validFullYAMLWithTLSFiles(t)))
	if err != nil {
		t.Fatalf("LoadConfig of validFullYAML failed: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validFullYAML should pass Validate(), got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Validate: filter IP whitelist (CIDR + plain IP)
// ---------------------------------------------------------------------------

func TestValidate_FilterWhitelistIPs(t *testing.T) {
	tests := []struct {
		name    string
		entries []string
		wantErr bool
		substr  string
	}{
		{"valid IPv4", []string{"10.0.0.1"}, false, ""},
		{"valid IPv6", []string{"::1"}, false, ""},
		{"valid CIDR /8", []string{"10.0.0.0/8"}, false, ""},
		{"valid CIDR /24", []string{"192.168.1.0/24"}, false, ""},
		{"valid IPv6 CIDR", []string{"fe80::/10"}, false, ""},
		{"mixed valid", []string{"10.0.0.1", "10.0.0.0/8", "::1"}, false, ""},
		{"invalid plain IP", []string{"not-an-ip"}, true, "not a valid IP address"},
		{"typo'd plain IP", []string{"10.0.0..1"}, true, "not a valid IP address"},
		{"invalid CIDR mask", []string{"10.0.0.0/99"}, true, "not a valid CIDR"},
		{"garbage CIDR", []string{"not-a-cidr/24"}, true, "not a valid CIDR"},
		{"empty entry", []string{""}, true, "not a valid IP address"},
		// First good, second bad — both should be reported with index
		{"second entry bad", []string{"10.0.0.1", "bogus"}, true, "filters.whitelist_ips[1]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Filters.WhitelistIPs = tt.entries
			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("entries=%v: err=%v, wantErr=%v", tt.entries, err, tt.wantErr)
			}
			if tt.wantErr && err != nil && !strings.Contains(err.Error(), tt.substr) {
				t.Errorf("error should contain %q, got: %v", tt.substr, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Validate: insecure_skip_verify interactions with secure / https://
// ---------------------------------------------------------------------------

func TestValidate_ClickHouseInsecureSkipVerifyRequiresSecure(t *testing.T) {
	cfg := validConfig()
	cfg.ClickHouse.InsecureSkipVerify = true
	cfg.ClickHouse.Secure = false
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error: clickhouse.insecure_skip_verify without secure")
	}
	if !strings.Contains(err.Error(), "clickhouse.insecure_skip_verify requires secure: true") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_ExportersOTEL_InsecureSkipVerifyRequiresSecure(t *testing.T) {
	cfg := validConfig()
	cfg.Exporters.OTEL = []OTELConfig{{
		CollectorAddress:   "localhost:4317",
		ServiceName:        "test",
		MaxQueryLength:     100000,
		InsecureSkipVerify: true,
		Secure:             false,
	}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error: exporters.otel[0].insecure_skip_verify without secure")
	}
	if !strings.Contains(err.Error(), "exporters.otel[0].insecure_skip_verify requires secure: true") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_ExportersOTEL_ClientCertificatePair(t *testing.T) {
	validExporter := OTELConfig{
		CollectorAddress: "localhost:4317",
		ServiceName:      "test",
		MaxQueryLength:   100000,
	}
	tests := []struct {
		name      string
		exporters []OTELConfig
		want      string
	}{
		{
			name: "client certificate without key while plaintext",
			exporters: []OTELConfig{{
				CollectorAddress: "localhost:4317",
				ServiceName:      "test",
				MaxQueryLength:   100000,
				ClientCert:       "/tls/client.pem",
			}},
			want: "exporters.otel[0].client_cert and exporters.otel[0].client_key must be provided together",
		},
		{
			name: "client key without certificate at second exporter",
			exporters: []OTELConfig{validExporter, {
				CollectorAddress: "backup:4317",
				ServiceName:      "backup",
				MaxQueryLength:   100000,
				Secure:           true,
				ClientKey:        "/tls/client-key.pem",
			}},
			want: "exporters.otel[1].client_cert and exporters.otel[1].client_key must be provided together",
		},
		{
			name: "complete pair is coherent even while plaintext",
			exporters: []OTELConfig{{
				CollectorAddress: "localhost:4317",
				ServiceName:      "test",
				MaxQueryLength:   100000,
				ClientCert:       "/tls/client.pem",
				ClientKey:        "/tls/client-key.pem",
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Exporters.OTEL = tt.exporters
			err := cfg.Validate()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("complete client certificate pair should pass Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestValidationWarnings_TLSMaterialWithoutSecure(t *testing.T) {
	tests := []struct {
		name              string
		mutate            func(*Config)
		want              []string
		wantValidationErr bool
	}{
		{
			name: "ClickHouse CA is ignored without TLS",
			mutate: func(c *Config) {
				c.ClickHouse.CACert = "/tls/clickhouse-ca.pem"
			},
			want: []string{"clickhouse.ca_cert", "clickhouse.secure is false"},
		},
		{
			name: "ClickHouse CA is active with TLS",
			mutate: func(c *Config) {
				c.ClickHouse.Secure = true
				c.ClickHouse.CACert = "/tls/clickhouse-ca.pem"
			},
		},
		{
			name: "second exporter CA and complete mTLS pair are ignored",
			mutate: func(c *Config) {
				c.Exporters.OTEL = append(c.Exporters.OTEL, OTELConfig{
					CollectorAddress: "backup:4317",
					ServiceName:      "backup",
					MaxQueryLength:   100000,
					CACert:           "/tls/otel-ca.pem",
					ClientCert:       "/tls/client.pem",
					ClientKey:        "/tls/client-key.pem",
				})
			},
			want: []string{
				"exporters.otel[1].ca_cert",
				"exporters.otel[1].secure is false",
				"exporters.otel[1].client_cert and client_key",
			},
		},
		{
			name: "active exporter TLS material is silent",
			mutate: func(c *Config) {
				c.Exporters.OTEL[0].Secure = true
				c.Exporters.OTEL[0].CACert = "/tls/otel-ca.pem"
				c.Exporters.OTEL[0].ClientCert = "/tls/client.pem"
				c.Exporters.OTEL[0].ClientKey = "/tls/client-key.pem"
			},
		},
		{
			name: "incoherent half pair has only a validation error",
			mutate: func(c *Config) {
				c.Exporters.OTEL[0].ClientCert = "/tls/client.pem"
			},
			wantValidationErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(cfg)
			warnings := cfg.tlsMaterialWarnings()
			got := strings.Join(warnings, "\n")
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("tlsMaterialWarnings() = %v, want substring %q", warnings, want)
				}
			}
			if len(tt.want) == 0 && len(warnings) != 0 {
				t.Errorf("tlsMaterialWarnings() = %v, want none", warnings)
			}
			if gotErr := cfg.Validate() != nil; gotErr != tt.wantValidationErr {
				t.Errorf("Validate() error presence = %v, want %v", gotErr, tt.wantValidationErr)
			}
		})
	}
}

func TestLoadConfig_TLSSemantics(t *testing.T) {
	t.Run("populates indexed inactive-material warnings", func(t *testing.T) {
		const yaml = `
clickhouse:
  host: localhost
  port: 9000
  database: default
  ca_cert: /not/read/while/plaintext/clickhouse-ca.pem
exporters:
  otel:
    - collector_address: primary:4317
    - collector_address: backup:4317
      ca_cert: /not/read/while/plaintext/otel-ca.pem
      client_cert: /not/read/while/plaintext/client.pem
      client_key: /not/read/while/plaintext/client-key.pem
monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 30
`
		cfg, err := LoadConfig(writeConfigFile(t, yaml))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		got := strings.Join(cfg.ValidationWarnings, "\n")
		for _, want := range []string{
			"clickhouse.ca_cert",
			"exporters.otel[1].ca_cert",
			"exporters.otel[1].client_cert and client_key",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("ValidationWarnings = %v, want substring %q", cfg.ValidationWarnings, want)
			}
		}
	})

	t.Run("rejects half pair at second exporter", func(t *testing.T) {
		const yaml = `
clickhouse:
  host: localhost
  port: 9000
  database: default
exporters:
  otel:
    - collector_address: primary:4317
    - collector_address: backup:4317
      client_key: /tls/client-key.pem
monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 30
`
		_, err := LoadConfig(writeConfigFile(t, yaml))
		if err == nil || !strings.Contains(err.Error(), "exporters.otel[1].client_cert and exporters.otel[1].client_key") {
			t.Fatalf("LoadConfig() error = %v, want indexed client certificate pair error", err)
		}
	})
}

func TestLoadConfig_RejectsUnreadableActiveTLSFiles(t *testing.T) {
	dir := t.TempDir()
	readable := filepath.Join(dir, "client.pem")
	if err := os.WriteFile(readable, []byte("test TLS material\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.pem")
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "ClickHouse CA",
			yaml: `
clickhouse:
  host: localhost
  port: 9000
  database: default
  secure: true
  ca_cert: ` + missing + `
exporters:
  otel:
    - collector_address: primary:4317
monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 30
`,
			want: "clickhouse.ca_cert",
		},
		{
			name: "second exporter CA",
			yaml: `
clickhouse:
  host: localhost
  port: 9000
  database: default
exporters:
  otel:
    - collector_address: primary:4317
    - collector_address: backup:4317
      secure: true
      ca_cert: ` + missing + `
monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 30
`,
			want: "exporters.otel[1].ca_cert",
		},
		{
			name: "second exporter client key",
			yaml: `
clickhouse:
  host: localhost
  port: 9000
  database: default
exporters:
  otel:
    - collector_address: primary:4317
    - collector_address: backup:4317
      secure: true
      client_cert: ` + readable + `
      client_key: ` + missing + `
monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 30
`,
			want: "exporters.otel[1].client_key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadConfig(writeConfigFile(t, tt.yaml))
			if err == nil {
				t.Fatal("LoadConfig() succeeded with an unreadable active TLS file")
			}
			if !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "fix the path or file permissions") {
				t.Fatalf("LoadConfig() error = %v, want field %q and actionable remedy", err, tt.want)
			}
		})
	}
}

func TestValidate_SplunkHEC_InsecureSkipVerifyRequiresHTTPS(t *testing.T) {
	cfg := validConfig()
	cfg.Exporters.SplunkHEC = []SplunkHECConfig{{
		Endpoint:           "http://splunk.example.com:8088",
		Token:              "abc",
		InsecureSkipVerify: true,
	}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error: splunk_hec[0].insecure_skip_verify without https")
	}
	if !strings.Contains(err.Error(), "exporters.splunk_hec[0].insecure_skip_verify requires an https:// endpoint") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_SplunkHEC_HTTPRequiresExplicitOptIn(t *testing.T) {
	cfg := validConfig()
	cfg.Exporters.SplunkHEC = []SplunkHECConfig{{
		Endpoint:       "http://splunk.example.com:8088",
		Token:          "abc",
		MaxQueryLength: 100000,
	}}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error: splunk_hec[0].endpoint uses http")
	}
	if !strings.Contains(err.Error(), "set allow_insecure_http: true") {
		t.Errorf("unexpected error: %v", err)
	}

	cfg.Exporters.SplunkHEC[0].AllowInsecureHTTP = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("http endpoint with allow_insecure_http should validate: %v", err)
	}
}

func TestValidate_SplunkHEC_RejectsEndpointWithoutScheme(t *testing.T) {
	cfg := validConfig()
	cfg.Exporters.SplunkHEC = []SplunkHECConfig{{
		Endpoint:       "splunk.example.com:8088",
		Token:          "abc",
		MaxQueryLength: 100000,
	}}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error: splunk_hec[0].endpoint missing scheme")
	}
	if !strings.Contains(err.Error(), "endpoint must start with https://") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_WebhookURLRequiresHTTPOrHTTPSWithHost(t *testing.T) {
	tests := []struct {
		name   string
		url    string
		substr string
	}{
		{"valid https", "https://hooks.slack.com/services/T000/B000/secret", ""},
		{"valid http", "http://alerts.internal/webhook", ""},
		{"missing scheme", "hooks.slack.com/services/T000", "scheme must be http or https"},
		{"unsupported scheme", "file:///tmp/webhook", "scheme must be http or https"},
		{"missing host", "https:///no-host", "host is required"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Webhook.Enabled = true
			cfg.Webhook.URL = tt.url

			err := cfg.Validate()
			if tt.substr == "" {
				if err != nil {
					t.Fatalf("expected valid webhook URL, got: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.substr) {
				t.Fatalf("expected error containing %q, got: %v", tt.substr, err)
			}
			if strings.Contains(err.Error(), tt.url) {
				t.Fatalf("webhook validation error leaked URL: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Validate: ClickHouse cluster name safe-identifier guard. The cluster name
// is interpolated into SQL via cluster('name', table); the regex blocks any
// characters that could let a config value escape the quoted identifier.
// ---------------------------------------------------------------------------

func TestValidate_ClickHouseClusterName_UnsafeIdentifier(t *testing.T) {
	tests := []struct {
		name    string
		cluster string
		wantErr bool
	}{
		{"empty allowed", "", false},
		{"simple alphanumeric", "prod", false},
		{"hyphenated", "prod-east", false},
		{"underscored", "prod_east", false},
		{"mixed allowed chars", "Prod-east_1", false},
		{"single quote injection", "prod', system.tables) --", true},
		{"space disallowed", "prod east", true},
		{"slash disallowed", "prod/east", true},
		{"semicolon disallowed", "prod;DROP", true},
		{"leading digit disallowed", "1prod", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.ClickHouse.Cluster = tt.cluster
			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("cluster=%q: err=%v, wantErr=%v", tt.cluster, err, tt.wantErr)
			}
			if tt.wantErr && err != nil && !strings.Contains(err.Error(), "clickhouse.cluster") {
				t.Errorf("error should mention clickhouse.cluster, got: %v", err)
			}
		})
	}
}

func TestConfigValidation_QueryTextMode(t *testing.T) {
	tests := []struct {
		name    string
		mode    QueryTextMode
		rules   []RedactionRule
		wantErr string
	}{
		{name: "raw", mode: QueryTextModeRaw},
		{name: "redacted", mode: QueryTextModeRedacted, rules: []RedactionRule{{Pattern: `'[^']*'`}}},
		{name: "normalized only", mode: QueryTextModeNormalizedOnly},
		{name: "normalized only ignores stale rules", mode: QueryTextModeNormalizedOnly, rules: []RedactionRule{{Pattern: `[invalid(`}}},
		{name: "none", mode: QueryTextModeNone},
		{name: "none ignores stale rules", mode: QueryTextModeNone, rules: []RedactionRule{{Pattern: `[invalid(`}}},
		{name: "ambiguous normalized rejected", mode: "normalized", wantErr: "normalized_only"},
		{name: "unknown rejected", mode: "scrubbed", wantErr: "query_text_mode"},
		{name: "redacted requires rules", mode: QueryTextModeRedacted, wantErr: "requires at least one valid"},
		{name: "rules rejected under raw", mode: QueryTextModeRaw, rules: []RedactionRule{{Pattern: `secret`}}, wantErr: "only used when"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Filters.QueryTextMode = tt.mode
			cfg.Filters.RedactQueries = tt.rules
			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Non-fatal advisories: topology-audit cap-vs-cadence (issue #243.6)
// ---------------------------------------------------------------------------

// TestTopologyAuditWarnings pins the cap-vs-cadence advisory: when the audit is
// live (enabled + use_cluster_queries) and the poll cadence runs past interval/2,
// the auditor caps its recency window below the cadence, so a genuinely-concurrent
// cross-cluster reader can be missed or the warning can flap. It's a non-fatal
// WARN, never a Validate error, and stays silent when the audit can't run.
func TestTopologyAuditWarnings(t *testing.T) {
	mk := func(clusterQueries, enabled bool, check, interval int) *Config {
		c := &Config{}
		c.ClickHouse.UseClusterQueries = clusterQueries
		c.Monitor.CheckIntervalS = check
		c.Monitor.TopologyAudit.Enabled = enabled
		c.Monitor.TopologyAudit.IntervalS = interval
		return c
	}
	cases := []struct {
		name     string
		c        *Config
		wantWarn bool
	}{
		{"defaults: 30s poll, 5m audit — fine", mk(true, true, 30, 300), false},
		{"boundary: window == cadence — fine", mk(true, true, 150, 300), false},
		{"poll past interval/2 — warns", mk(true, true, 200, 300), true},
		{"audit disabled — silent", mk(true, false, 200, 300), false},
		{"single-instance (no cluster queries) — silent", mk(false, true, 200, 300), false},
		{"unset knobs — silent", mk(true, true, 0, 0), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := tc.c.topologyAuditWarnings()
			if got := len(w) > 0; got != tc.wantWarn {
				t.Fatalf("topologyAuditWarnings() = %v, wantWarn=%v", w, tc.wantWarn)
			}
			if tc.wantWarn && !strings.Contains(w[0], "interval_s") {
				t.Errorf("warning should name the offending knob: %q", w[0])
			}
		})
	}
}

// TestLoadConfig_PopulatesTopologyAuditWarning pins the wiring: LoadConfig fills
// cfg.ValidationWarnings from the *resolved* config (after defaults), which is
// what main/analyze actually read — not just the direct helper.
func TestLoadConfig_PopulatesTopologyAuditWarning(t *testing.T) {
	const yaml = `
clickhouse:
  host: localhost
  port: 9000
  database: default
  cluster: prod
  use_cluster_queries: true
exporters:
  otel:
    - collector_address: localhost:4317
monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 200
  topology_audit:
    interval_s: 300
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.ValidationWarnings) == 0 {
		t.Fatal("expected a topology-audit cadence warning in ValidationWarnings, got none")
	}
}
