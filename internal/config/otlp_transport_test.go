package config

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

func TestOTLPTransport_AddressTable(t *testing.T) {
	f, err := os.Open("testdata/otlp-transport-targets.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	rows := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		addr, want, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("malformed row %q", line)
		}
		rows++
		t.Run(addr, func(t *testing.T) {
			listenErr := otlpAddressError("collector_address", addr, addr)
			if want == "listen" {
				if !strings.Contains(listenErr, "is a listen address") {
					t.Fatalf("otlpAddressError(%q) = %q, want a listen-address error", addr, listenErr)
				}
				return
			}
			if listenErr != "" {
				t.Fatalf("otlpAddressError(%q) = %q, want none", addr, listenErr)
			}
			secure, source := ResolveOTLPTransport(addr, nil, false)
			if got := map[bool]string{true: "tls", false: "plaintext"}[secure]; got != want {
				t.Errorf("ResolveOTLPTransport(%q) = %s, want %s", addr, got, want)
			}
			if source != TransportAutomatic {
				t.Errorf("source = %q, want automatic", source)
			}
		})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if rows < 30 {
		t.Fatalf("read %d rows; the table is incomplete", rows)
	}
}

func TestOTLPTransport_ExplicitAndMaterial(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name       string
		addr       string
		secure     *bool
		material   bool
		wantSecure bool
		wantSource TransportSource
	}{
		{"explicit false off loopback", "otel.example.com:4317", &no, false, false, TransportExplicit},
		{"explicit true on loopback", "localhost:4317", &yes, false, true, TransportExplicit},
		{"material on loopback selects TLS", "localhost:4317", nil, true, true, TransportExplicit},
		{"automatic loopback", "localhost:4317", nil, false, false, TransportAutomatic},
		{"automatic remote", "otel.example.com:4317", nil, false, true, TransportAutomatic},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secure, source := ResolveOTLPTransport(tt.addr, tt.secure, tt.material)
			if secure != tt.wantSecure || source != tt.wantSource {
				t.Errorf("got (%v, %s), want (%v, %s)", secure, source, tt.wantSecure, tt.wantSource)
			}
		})
	}
}

func TestTransportSource_ZeroValueIsAutomatic(t *testing.T) {
	var s TransportSource
	if s.IsExplicit() || s.String() != "automatic" {
		t.Fatalf("zero TransportSource = %q explicit=%v, want automatic", s.String(), s.IsExplicit())
	}
}

func TestLoadConfig_ResolvesOTLPTransport(t *testing.T) {
	const yaml = `
clickhouse:
  host: localhost
monitor:
  min_trace_duration_ms: 1000
exporters:
  otel:
    - collector_address: otel.example.com:4317
    - collector_address: localhost:4317
    - collector_address: otel-collector.monitoring:4317
      secure: false
metrics:
  otlp:
    enabled: true
    inherit_otel_connection: false
    collector_address: localhost:4317
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	want := []struct {
		secure bool
		source TransportSource
	}{
		{true, TransportAutomatic},
		{false, TransportAutomatic},
		{false, TransportExplicit},
	}
	for i, w := range want {
		o := cfg.Exporters.OTEL[i]
		if o.Secure != w.secure || o.SecureSource != w.source {
			t.Errorf("exporters.otel[%d] = (%v, %s), want (%v, %s)", i, o.Secure, o.SecureSource, w.secure, w.source)
		}
		if o.Name != "exporters.otel["+string(rune('0'+i))+"]" {
			t.Errorf("exporters.otel[%d].Name = %q", i, o.Name)
		}
	}
	if cfg.Metrics.OTLP.Secure || cfg.Metrics.OTLP.SecureSource != TransportAutomatic {
		t.Errorf("metrics.otlp = (%v, %s), want automatic plaintext", cfg.Metrics.OTLP.Secure, cfg.Metrics.OTLP.SecureSource)
	}
	// Explicit plaintext off loopback is reported by validate and the startup
	// log, not as a validation warning on every run.
	for _, w := range cfg.ValidationWarnings {
		if strings.Contains(w, "exporters.otel") {
			t.Errorf("unexpected OTLP validation warning: %s", w)
		}
	}
}

func TestLoadConfig_RejectsOTLPListenAndExpandedEmptyHost(t *testing.T) {
	t.Setenv("CLICK_DOG_TEST_UNSET_HOST", "")
	tests := []struct {
		name string
		addr string
		want string
	}{
		{"listen address", "0.0.0.0:4317", `exporters.otel[0].collector_address "0.0.0.0:4317" is a listen address`},
		{"IPv6 listen address", "[::]:4317", "is a listen address"},
		{"expansion produced an empty host", "${CLICK_DOG_TEST_UNSET_HOST}:4317", `expands to ":4317", which has no host`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yaml := "clickhouse:\n  host: localhost\nmonitor:\n  min_trace_duration_ms: 1000\nexporters:\n  otel:\n    - collector_address: \"" + tt.addr + "\"\n"
			_, err := LoadConfig(writeConfigFile(t, yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("LoadConfig() error = %v, want %q", err, tt.want)
			}
		})
	}

	t.Run("a written empty host is the local host", func(t *testing.T) {
		yaml := "clickhouse:\n  host: localhost\nmonitor:\n  min_trace_duration_ms: 1000\nexporters:\n  otel:\n    - collector_address: \":4317\"\n"
		cfg, err := LoadConfig(writeConfigFile(t, yaml))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.Exporters.OTEL[0].Secure {
			t.Error(":4317 should be automatic plaintext")
		}
	})

	t.Run("standalone metrics listen address", func(t *testing.T) {
		yaml := "clickhouse:\n  host: localhost\nmonitor:\n  min_trace_duration_ms: 1000\nexporters:\n  otel:\n    - collector_address: localhost:4317\nmetrics:\n  otlp:\n    enabled: true\n    inherit_otel_connection: false\n    collector_address: 0.0.0.0:4317\n"
		_, err := LoadConfig(writeConfigFile(t, yaml))
		if err == nil || !strings.Contains(err.Error(), "metrics.otlp.collector_address") {
			t.Fatalf("LoadConfig() error = %v, want metrics.otlp listen-address error", err)
		}
	})
}

func TestTransportSummary(t *testing.T) {
	tests := []struct {
		addr   string
		secure bool
		source TransportSource
		want   string
	}{
		{"otel.example.com:4317", true, TransportAutomatic, "TLS (automatic)"},
		{"localhost:4317", false, "", "plaintext (automatic)"},
		{"localhost:4317", false, TransportExplicit, "plaintext (explicit)"},
		{"otel-collector:4317", false, TransportExplicit, "plaintext (explicit, collector is not loopback)"},
	}
	for _, tt := range tests {
		if got := TransportSummary(tt.addr, tt.secure, tt.source); got != tt.want {
			t.Errorf("TransportSummary(%q, %v, %q) = %q, want %q", tt.addr, tt.secure, tt.source, got, tt.want)
		}
	}
}
