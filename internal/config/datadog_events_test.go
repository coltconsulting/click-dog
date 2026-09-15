package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidate_DatadogEventsConfiguration(t *testing.T) {
	valid := DatadogEventsConfig{
		Enabled:        true,
		Site:           "datadoghq.com",
		APIKey:         "api-secret",
		ApplicationKey: "app-secret",
		TimeoutS:       10,
		Environment:    "production",
		Service:        "click-dog-monitor",
	}
	tests := []struct {
		name   string
		mutate func(*DatadogEventsConfig)
		want   string
	}{
		{name: "valid", mutate: func(*DatadogEventsConfig) {}},
		{name: "missing site", mutate: func(c *DatadogEventsConfig) { c.Site = "" }, want: "site is required"},
		{name: "authority injection", mutate: func(c *DatadogEventsConfig) { c.Site = "datadoghq.com@attacker.example" }, want: "site"},
		{name: "missing api key", mutate: func(c *DatadogEventsConfig) { c.APIKey = "" }, want: "api_key"},
		{name: "blank api key", mutate: func(c *DatadogEventsConfig) { c.APIKey = "  " }, want: "api_key"},
		{name: "missing application key", mutate: func(c *DatadogEventsConfig) { c.ApplicationKey = "" }, want: "application_key"},
		{name: "zero timeout", mutate: func(c *DatadogEventsConfig) { c.TimeoutS = 0 }, want: "timeout_s"},
		{name: "excessive timeout", mutate: func(c *DatadogEventsConfig) { c.TimeoutS = MaxDatadogEventsTimeoutS + 1 }, want: "timeout_s"},
		{name: "missing environment", mutate: func(c *DatadogEventsConfig) { c.Environment = "" }, want: "environment"},
		{name: "high-cardinality environment", mutate: func(c *DatadogEventsConfig) { c.Environment = "prod west" }, want: "environment"},
		{name: "missing service", mutate: func(c *DatadogEventsConfig) { c.Service = "" }, want: "service"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.DatadogEvents = valid
			tt.mutate(&cfg.DatadogEvents)
			err := cfg.Validate()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate error = %v, want %q", err, tt.want)
			}
			for _, secret := range []string{"api-secret", "app-secret"} {
				if err != nil && strings.Contains(err.Error(), secret) {
					t.Fatalf("validation error leaked credential %q: %v", secret, err)
				}
			}
		})
	}
}

func TestLoadConfig_DatadogEventsEnvironmentAndSecretFiles(t *testing.T) {
	dir := t.TempDir()
	apiPath := filepath.Join(dir, "dd-api-key")
	if err := os.WriteFile(apiPath, []byte("api-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DD_SITE_TEST", "DATADOGHQ.EU")
	t.Setenv("DD_API_FILE_TEST", apiPath)
	t.Setenv("DD_APP_TEST", "app-from-env")
	t.Setenv("DD_ENV_TEST", "staging")
	t.Setenv("DD_SERVICE_TEST", "orders-api")

	yaml := validMinimalYAML + `
datadog_events:
  enabled: true
  site: ${DD_SITE_TEST}
  api_key_file: ${DD_API_FILE_TEST}
  application_key: ${DD_APP_TEST}
  environment: ${DD_ENV_TEST}
  service: ${DD_SERVICE_TEST}
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DatadogEvents.Site != "datadoghq.eu" || cfg.DatadogEvents.APIKey != "api-from-file" || cfg.DatadogEvents.ApplicationKey != "app-from-env" {
		t.Fatalf("resolved Datadog config = %+v", cfg.DatadogEvents)
	}
	if cfg.DatadogEvents.TimeoutS != DefaultDatadogEventsTimeoutS || cfg.DatadogEvents.Environment != "staging" || cfg.DatadogEvents.Service != "orders-api" {
		t.Fatalf("default/tag config = %+v", cfg.DatadogEvents)
	}
}

func TestLoadConfig_DatadogEventsSecretFormsAreMutuallyExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("file-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	yaml := validMinimalYAML + `
datadog_events:
  enabled: true
  site: datadoghq.com
  api_key: inline-secret
  api_key_file: ` + path + `
  application_key: app-secret
  environment: production
  service: click-dog-monitor
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil || !strings.Contains(err.Error(), "set either the inline value") {
		t.Fatalf("LoadConfig error = %v", err)
	}
	for _, secret := range []string{"inline-secret", "file-secret", "app-secret"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked credential %q: %v", secret, err)
		}
	}
}

func TestLoadConfig_DatadogEventsApplicationKeyFile(t *testing.T) {
	appPath := filepath.Join(t.TempDir(), "dd-app-key")
	if err := os.WriteFile(appPath, []byte("app-from-file\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DD_APP_FILE_TEST", appPath)
	yaml := validMinimalYAML + `
datadog_events:
  enabled: true
  site: datadoghq.com
  api_key: api-inline
  application_key_file: ${DD_APP_FILE_TEST}
  environment: production
  service: click-dog-monitor
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DatadogEvents.ApplicationKey != "app-from-file" {
		t.Fatalf("application key = %q, want file value with one CRLF trimmed", cfg.DatadogEvents.ApplicationKey)
	}
}

func TestValidate_WebhookAnalysisFindingsEvent(t *testing.T) {
	cfg := validConfig()
	cfg.Webhook = WebhookConfig{
		Enabled: true,
		URL:     "https://hooks.slack.com/services/example",
		Events:  []string{"analysis_findings"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("analysis_findings event should validate: %v", err)
	}
}
