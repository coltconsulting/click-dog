package config

import (
	"os"
	"strings"
	"testing"
)

func TestLookupPath_TopLevel(t *testing.T) {
	m := map[string]interface{}{
		"otel": map[string]interface{}{
			"collector_address": "localhost:4317",
		},
	}
	if !lookupPath(m, "otel") {
		t.Error("expected lookupPath to find top-level key 'otel'")
	}
}

func TestLookupPath_Nested(t *testing.T) {
	m := map[string]interface{}{
		"exporters": map[string]interface{}{
			"otel": map[string]interface{}{
				"tls": map[string]interface{}{
					"insecure": true,
				},
			},
		},
	}
	if !lookupPath(m, "exporters.otel.tls.insecure") {
		t.Error("expected lookupPath to find nested path")
	}
}

func TestLookupPath_Missing(t *testing.T) {
	m := map[string]interface{}{
		"clickhouse": map[string]interface{}{},
	}
	if lookupPath(m, "otel") {
		t.Error("expected lookupPath to return false for missing key")
	}
	if lookupPath(m, "clickhouse.nonexistent") {
		t.Error("expected lookupPath to return false for missing nested key")
	}
}

func TestLookupPath_PartialPath(t *testing.T) {
	m := map[string]interface{}{
		"a": "scalar_value",
	}
	// Traversing into a non-map value should return false
	if lookupPath(m, "a.b") {
		t.Error("expected lookupPath to return false when traversing into a non-map")
	}
}

func TestCheckDeprecations_NoDeprecated(t *testing.T) {
	raw := map[string]interface{}{
		"exporters": map[string]interface{}{
			"otel": []interface{}{},
		},
		"clickhouse": map[string]interface{}{},
	}
	warnings := checkDeprecations(raw)
	if len(warnings) != 0 {
		t.Fatalf("expected 0 warnings, got %d: %v", len(warnings), warnings)
	}
}

func TestCheckDeprecations_EmptyMap(t *testing.T) {
	warnings := checkDeprecations(map[string]interface{}{})
	if len(warnings) != 0 {
		t.Fatalf("expected 0 warnings for empty map, got %d", len(warnings))
	}
}

func TestLoadConfig_EnvWarnings(t *testing.T) {
	t.Setenv("CD_TEST_SET_HOST", "ch.example.com")
	// CD_TEST_UNSET_PASSWORD is deliberately not set.

	tmpFile := t.TempDir() + "/config.yaml"
	configYAML := `
clickhouse:
  host: ${CD_TEST_SET_HOST}
  port: 9000
  password: ${CD_TEST_UNSET_PASSWORD}
exporters:
  otel:
    - collector_address: localhost:4317
      service_name: test
monitor:
  min_trace_duration_ms: 1000
  check_interval_s: 30
`
	if err := writeTestFile(tmpFile, configYAML); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(tmpFile)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if len(cfg.EnvWarnings) != 1 {
		t.Fatalf("expected 1 env warning (unset var only), got %d: %v", len(cfg.EnvWarnings), cfg.EnvWarnings)
	}
	if !strings.Contains(cfg.EnvWarnings[0], "CD_TEST_UNSET_PASSWORD") {
		t.Errorf("expected warning to name the unset var, got: %s", cfg.EnvWarnings[0])
	}
	// Env refs must not bleed into the deprecation channel.
	if len(cfg.DeprecationWarnings) != 0 {
		t.Errorf("env refs must not produce deprecation warnings, got: %v", cfg.DeprecationWarnings)
	}
}

func TestLoadConfig_LegacyOTELKeyRejected(t *testing.T) {
	// The legacy top-level otel: key was removed in favor of exporters.otel[].
	// Strict decode must now reject it as an unknown field rather than silently
	// ignoring it (or, as before, promoting it).
	tmpFile := t.TempDir() + "/config.yaml"
	configYAML := `
clickhouse:
  host: localhost
  port: 9000
otel:
  collector_address: localhost:4317
  service_name: test
monitor:
  min_trace_duration_ms: 1000
  check_interval_s: 30
`
	if err := writeTestFile(tmpFile, configYAML); err != nil {
		t.Fatal(err)
	}

	_, err := LoadConfig(tmpFile)
	if err == nil {
		t.Fatal("expected LoadConfig to reject the removed top-level otel: key, got nil")
	}
	if !strings.Contains(err.Error(), "otel") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

func writeTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0644)
}
