package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveSecret(t *testing.T) {
	dir := t.TempDir()
	write := func(name, contents string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("no file returns inline unchanged", func(t *testing.T) {
		got, err := resolveSecret("clickhouse.password", "inline-pw", "")
		if err != nil {
			t.Fatal(err)
		}
		if got != "inline-pw" {
			t.Errorf("got %q, want inline-pw", got)
		}
	})

	t.Run("file wins and trims one trailing newline", func(t *testing.T) {
		got, err := resolveSecret("clickhouse.password", "", write("pw", "filesecret\n"))
		if err != nil {
			t.Fatal(err)
		}
		if got != "filesecret" {
			t.Errorf("got %q, want filesecret (newline trimmed)", got)
		}
	})

	t.Run("file trims trailing CRLF", func(t *testing.T) {
		got, err := resolveSecret("clickhouse.password", "", write("pwcrlf", "filesecret\r\n"))
		if err != nil {
			t.Fatal(err)
		}
		if got != "filesecret" {
			t.Errorf("got %q, want filesecret (CRLF trimmed)", got)
		}
	})

	t.Run("only the last newline is trimmed", func(t *testing.T) {
		got, err := resolveSecret("clickhouse.password", "", write("pwmulti", "a\nb\n"))
		if err != nil {
			t.Fatal(err)
		}
		if got != "a\nb" {
			t.Errorf("got %q, want %q", got, "a\nb")
		}
	})

	t.Run("a bare trailing CR is preserved (only CRLF/LF trimmed)", func(t *testing.T) {
		got, err := resolveSecret("clickhouse.password", "", write("pwcr", "secret\r"))
		if err != nil {
			t.Fatal(err)
		}
		if got != "secret\r" {
			t.Errorf("got %q, want %q (lone CR not a line ending)", got, "secret\r")
		}
	})

	t.Run("both inline and file is an error", func(t *testing.T) {
		_, err := resolveSecret("clickhouse.password", "inline-pw", write("pw2", "x"))
		if err == nil {
			t.Fatal("expected error when both inline and file set")
		}
		if !strings.Contains(err.Error(), "not both") {
			t.Errorf("error %q should explain the conflict", err)
		}
	})

	t.Run("unreadable file is an error (fail closed)", func(t *testing.T) {
		_, err := resolveSecret("clickhouse.password", "", filepath.Join(dir, "does-not-exist"))
		if err == nil {
			t.Fatal("expected error for missing file")
		}
		if !strings.Contains(err.Error(), "clickhouse.password_file") {
			t.Errorf("error %q should name the *_file key", err)
		}
	})

	t.Run("empty file is an error (never an empty secret)", func(t *testing.T) {
		for _, contents := range []string{"", "\n", "\r\n"} {
			_, err := resolveSecret("clickhouse.password", "", write("pwempty", contents))
			if err == nil {
				t.Fatalf("expected error for empty file (contents %q)", contents)
			}
			if !strings.Contains(err.Error(), "is empty") {
				t.Errorf("error %q should explain the file is empty", err)
			}
		}
	})

	t.Run("templated ${ENV} file path is expanded and read", func(t *testing.T) {
		p := write("pwenv", "envsecret\n")
		t.Setenv("CLICKDOG_TEST_PW_PATH", p)
		got, err := resolveSecret("clickhouse.password", "", "${CLICKDOG_TEST_PW_PATH}")
		if err != nil {
			t.Fatal(err)
		}
		if got != "envsecret" {
			t.Errorf("got %q, want envsecret (read via templated path)", got)
		}
	})

	t.Run("templated path with an unset var fails closed", func(t *testing.T) {
		// The var is deliberately never set: the *_file key is non-empty but
		// expands to "", which must error rather than be treated as absent.
		_, err := resolveSecret("clickhouse.password", "", "${CLICKDOG_TEST_UNSET_PW_PATH}")
		if err == nil {
			t.Fatal("expected error when a non-empty *_file expands to an empty path")
		}
		if !strings.Contains(err.Error(), "empty path") {
			t.Errorf("error %q should explain the path expands to empty", err)
		}
	})
}

// TestLoadConfig_PasswordFileTemplatedUnsetFailsClosed pins the P2 fix end to
// end: a `password_file: ${UNSET}` whose env var is unset must fail LoadConfig,
// not silently expand to an empty path and connect with no credential.
func TestLoadConfig_PasswordFileTemplatedUnsetFailsClosed(t *testing.T) {
	yaml := `
clickhouse:
  host: ch.example.com
  port: 9000
  database: system
  password_file: ${CLICKDOG_TEST_UNSET_PW_PATH}
exporters:
  otel:
    - collector_address: otel:4317
      service_name: s
monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 30
`
	if _, err := LoadConfig(writeConfigFile(t, yaml)); err == nil {
		t.Fatal("expected LoadConfig to reject a password_file that expands to an empty path")
	}
}

func TestLoadConfig_PasswordFile(t *testing.T) {
	dir := t.TempDir()
	pwPath := filepath.Join(dir, "ch.pw")
	if err := os.WriteFile(pwPath, []byte("file-pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	yaml := `
clickhouse:
  host: ch.example.com
  port: 9000
  database: system
  password_file: ` + pwPath + `
exporters:
  otel:
    - collector_address: otel:4317
      service_name: s
monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 30
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ClickHouse.Password != "file-pw" {
		t.Errorf("password = %q, want file-pw (from password_file)", cfg.ClickHouse.Password)
	}
}

func TestLoadConfig_PasswordAndFileConflict(t *testing.T) {
	dir := t.TempDir()
	pwPath := filepath.Join(dir, "ch.pw")
	if err := os.WriteFile(pwPath, []byte("file-pw"), 0o600); err != nil {
		t.Fatal(err)
	}
	yaml := `
clickhouse:
  host: ch.example.com
  port: 9000
  database: system
  password: inline-pw
  password_file: ` + pwPath + `
exporters:
  otel:
    - collector_address: otel:4317
      service_name: s
monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 30
`
	if _, err := LoadConfig(writeConfigFile(t, yaml)); err == nil {
		t.Fatal("expected LoadConfig to reject password + password_file together")
	}
}

func TestLoadConfig_SplunkTokenFile(t *testing.T) {
	dir := t.TempDir()
	tokPath := filepath.Join(dir, "hec.token")
	if err := os.WriteFile(tokPath, []byte("hec-tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	yaml := `
clickhouse:
  host: ch.example.com
  port: 9000
  database: system
  password: pw
exporters:
  splunk_hec:
    - endpoint: https://splunk:8088
      token_file: ` + tokPath + `
monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 30
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Exporters.SplunkHEC) != 1 || cfg.Exporters.SplunkHEC[0].Token != "hec-tok" {
		t.Errorf("splunk token = %q, want hec-tok (from token_file)", cfg.Exporters.SplunkHEC[0].Token)
	}
}
