package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidate_ValidConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "click-dog.yaml")
	if err := os.WriteFile(path, []byte(helperTestConfig), 0600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := runValidate([]string{"-config", path}, &out, &errOut)

	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, errOut.String())
	}
	for _, want := range []string{
		"Config " + path + " is valid.",
		"ClickHouse:  localhost:9000",
		"Exporters:   1 OTEL, 0 Splunk HEC",
		"localhost:4317 (service=helper-test, transport=plaintext (automatic))",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout missing %q; got:\n%s", want, out.String())
		}
	}
	if errOut.Len() > 0 {
		t.Errorf("expected clean stderr, got %q", errOut.String())
	}
}

func TestValidate_InvalidConfigReturns1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.yaml")

	var out, errOut bytes.Buffer
	code := runValidate([]string{"-config", path}, &out, &errOut)

	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "Invalid config:") {
		t.Errorf("stderr = %q, want 'Invalid config:' prefix", errOut.String())
	}
}

func TestValidate_RejectsPositionalArgs(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runValidate([]string{"extra"}, &out, &errOut)

	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), `unexpected argument "extra"`) {
		t.Errorf("stderr = %q, want unexpected-argument error", errOut.String())
	}
}

func TestValidate_ShowsOTLPTransportAndSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "click-dog.yaml")
	body := `clickhouse:
  host: localhost
exporters:
  otel:
    - collector_address: otel.example.com:4317
    - collector_address: otel-collector.monitoring:4317
      secure: false
    - collector_address: localhost:4317
      secure: true
metrics:
  otlp:
    enabled: true
    inherit_otel_connection: false
    collector_address: localhost:4317
monitor:
  min_trace_duration_ms: 1000
`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := runValidate([]string{"-config", path}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d; stderr: %s", code, errOut.String())
	}
	for _, want := range []string{
		"otel.example.com:4317 (service=click-dog-monitor, transport=TLS (automatic))",
		"otel-collector.monitoring:4317 (service=click-dog-monitor, transport=plaintext (explicit, collector is not loopback))",
		"localhost:4317 (service=click-dog-monitor, transport=TLS (explicit))",
		"(standalone, transport=plaintext (automatic))",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout missing %q; got:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "WARN") {
		t.Errorf("explicit plaintext is marked on its line, not warned:\n%s", out.String())
	}
}
