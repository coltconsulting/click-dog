package leader

import (
	"fmt"
	"os"
	"testing"

	"github.com/coltconsulting/click-dog/internal/config"
)

func TestLeaderElection_InvalidConfig_EmptyHosts(t *testing.T) {
	_, err := NewLeaderElection(
		config.LeaderElectionConfig{
			Hosts: []string{},
		},
		func() {},
		func() {},
	)
	if err == nil {
		t.Error("Expected error for empty hosts")
	}
}

func TestLeaderElection_InvalidConfig_NilHosts(t *testing.T) {
	_, err := NewLeaderElection(
		config.LeaderElectionConfig{
			Hosts: nil,
		},
		func() {},
		func() {},
	)
	if err == nil {
		t.Error("Expected error for nil hosts")
	}
}

func TestLeaderElection_IsLeader_DefaultFalse(t *testing.T) {
	le := &LeaderElection{}
	if le.IsLeader() {
		t.Error("Default LeaderElection should not be leader")
	}
}

func TestLeaderElection_HandleDemotion_NotLeader(t *testing.T) {
	demoted := false
	le := &LeaderElection{
		onDemoted: func() { demoted = true },
	}
	le.handleDemotion()
	if demoted {
		t.Error("Should not call onDemoted when not leader")
	}
}

func TestLeaderElection_HandleDemotion_WasLeader(t *testing.T) {
	demoted := false
	le := &LeaderElection{
		onDemoted: func() { demoted = true },
	}
	le.isLeader.Store(true)
	le.handleDemotion()
	if !demoted {
		t.Error("Should call onDemoted when was leader")
	}
	if le.IsLeader() {
		t.Error("Should no longer be leader after demotion")
	}
}

func TestLeaderElection_HandleDemotion_NilCallback(t *testing.T) {
	le := &LeaderElection{}
	le.isLeader.Store(true)
	le.handleDemotion() // should not panic
	if le.IsLeader() {
		t.Error("Should no longer be leader after demotion")
	}
}

func TestLeaderElection_HandleDemotion_PanicRecovery(t *testing.T) {
	le := &LeaderElection{
		onDemoted: func() { panic("callback panic") },
	}
	le.isLeader.Store(true)
	le.handleDemotion() // should recover, not crash
	if le.IsLeader() {
		t.Error("Should no longer be leader after demotion")
	}
}

func TestLeaderElection_HandlePromotion_PanicRecovery(t *testing.T) {
	// safeCallback should recover from panics in onPromoted
	recovered := true
	func() {
		defer func() {
			if r := recover(); r != nil {
				recovered = false
			}
		}()
		safeCallback("onPromoted", func() { panic("boom") })
	}()
	if !recovered {
		t.Error("safeCallback should have recovered the panic")
	}
}

func TestLeaderElection_ResignIdempotent(t *testing.T) {
	le := &LeaderElection{closed: true}
	err := le.Resign()
	if err != nil {
		t.Errorf("Resign on already-closed should return nil, got: %v", err)
	}
}

func TestLeaderElection_ACL_WorldWhenNoAuth(t *testing.T) {
	le := &LeaderElection{
		config: config.LeaderElectionConfig{},
	}
	acl := le.acl()
	if len(acl) != 1 {
		t.Fatalf("Expected 1 ACL entry, got %d", len(acl))
	}
	if acl[0].Scheme != "world" {
		t.Errorf("Expected world scheme, got %s", acl[0].Scheme)
	}
}

func TestLeaderElection_ACL_AuthWhenCredentials(t *testing.T) {
	le := &LeaderElection{
		config: config.LeaderElectionConfig{
			AuthUser:     "click-dog",
			AuthPassword: "secret",
		},
	}
	acl := le.acl()
	if len(acl) != 1 {
		t.Fatalf("Expected 1 ACL entry, got %d", len(acl))
	}
	if acl[0].Scheme != "auth" {
		t.Errorf("Expected auth scheme, got %s", acl[0].Scheme)
	}
}

func TestHAConfig_Defaults(t *testing.T) {
	configYAML := `
clickhouse:
  host: localhost
  port: 9000
exporters:
  otel:
    - collector_address: localhost:4317
monitor:
  enabled: true
  min_trace_duration_ms: 100
  check_interval_s: 30
log_level: info
ha:
  keeper:
    hosts:
      - "localhost:9181"
`
	tmpFile := t.TempDir() + "/test-config.yaml"
	if err := os.WriteFile(tmpFile, []byte(configYAML), 0644); err != nil {
		t.Fatalf("Failed to write test config: %v", err)
	}

	cfg, err := config.LoadConfig(tmpFile)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if !cfg.HA.Active() {
		t.Error("HA should be active (Keeper hosts configured)")
	}
	if cfg.HA.Keeper.SessionTimeout != 10 {
		t.Errorf("Expected session timeout 10, got %d", cfg.HA.Keeper.SessionTimeout)
	}
	if cfg.HA.Keeper.BasePath != "/click-dog/election" {
		t.Errorf("Expected base path /click-dog/election, got %s", cfg.HA.Keeper.BasePath)
	}
}

func TestHAConfig_CustomValues(t *testing.T) {
	configYAML := `
clickhouse:
  host: localhost
  port: 9000
exporters:
  otel:
    - collector_address: localhost:4317
monitor:
  enabled: true
  min_trace_duration_ms: 100
  check_interval_s: 30
log_level: info
ha:
  keeper:
    hosts:
      - "keeper1:9181"
      - "keeper2:9181"
      - "keeper3:9181"
    session_timeout_s: 15
    secure: true
    base_path: "/click-dog/my-app/leader"
    auth_user: "click-dog"
    auth_password: "secret"
`
	tmpFile := t.TempDir() + "/test-config.yaml"
	if err := os.WriteFile(tmpFile, []byte(configYAML), 0644); err != nil {
		t.Fatalf("Failed to write test config: %v", err)
	}

	cfg, err := config.LoadConfig(tmpFile)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if len(cfg.HA.Keeper.Hosts) != 3 {
		t.Errorf("Expected 3 hosts, got %d", len(cfg.HA.Keeper.Hosts))
	}
	if cfg.HA.Keeper.SessionTimeout != 15 {
		t.Errorf("Expected session timeout 15, got %d", cfg.HA.Keeper.SessionTimeout)
	}
	if cfg.HA.Keeper.BasePath != "/click-dog/my-app/leader" {
		t.Errorf("Expected base path /click-dog/my-app/leader, got %s", cfg.HA.Keeper.BasePath)
	}
	if !cfg.HA.Keeper.Secure {
		t.Error("Expected secure true")
	}
	if cfg.HA.Keeper.AuthUser != "click-dog" {
		t.Errorf("Expected auth_user click-dog, got %s", cfg.HA.Keeper.AuthUser)
	}
	if cfg.HA.Keeper.AuthPassword != "secret" {
		t.Errorf("Expected auth_password secret, got %s", cfg.HA.Keeper.AuthPassword)
	}
}

func TestHAConfig_NoHostsInactive(t *testing.T) {
	configYAML := `
clickhouse:
  host: localhost
  port: 9000
exporters:
  otel:
    - collector_address: localhost:4317
monitor:
  enabled: true
  min_trace_duration_ms: 100
  check_interval_s: 30
log_level: info
ha:
  keeper:
    hosts: []
`
	tmpFile := t.TempDir() + "/test-config.yaml"
	if err := os.WriteFile(tmpFile, []byte(configYAML), 0644); err != nil {
		t.Fatalf("Failed to write test config: %v", err)
	}

	cfg, err := config.LoadConfig(tmpFile)
	if err != nil {
		t.Fatalf("empty keeper hosts should load fine (HA inactive), got: %v", err)
	}
	if cfg.HA.Active() {
		t.Error("HA should be inactive with empty keeper hosts")
	}
}

func TestHAConfig_ValidationBadBasePath(t *testing.T) {
	configYAML := `
clickhouse:
  host: localhost
  port: 9000
exporters:
  otel:
    - collector_address: localhost:4317
monitor:
  enabled: true
  min_trace_duration_ms: 100
  check_interval_s: 30
log_level: info
ha:
  keeper:
    hosts:
      - "localhost:9181"
    base_path: "/clickhouse/tables"
`
	tmpFile := t.TempDir() + "/test-config.yaml"
	if err := os.WriteFile(tmpFile, []byte(configYAML), 0644); err != nil {
		t.Fatalf("Failed to write test config: %v", err)
	}

	_, err := config.LoadConfig(tmpFile)
	if err == nil {
		t.Error("Expected validation error for base_path overlapping /clickhouse")
	}
}

func TestHAConfig_ValidationSessionTimeoutBounds(t *testing.T) {
	for _, tt := range []struct {
		name    string
		timeout int
		wantErr bool
	}{
		{"too low", 2, true},
		{"minimum", 5, false},
		{"maximum", 30, false},
		{"too high", 60, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			configYAML := `
clickhouse:
  host: localhost
  port: 9000
exporters:
  otel:
    - collector_address: localhost:4317
monitor:
  enabled: true
  min_trace_duration_ms: 100
  check_interval_s: 30
log_level: info
ha:
  keeper:
    hosts:
      - "localhost:9181"
    session_timeout_s: ` + fmt.Sprintf("%d", tt.timeout) + `
`
			tmpFile := t.TempDir() + "/test-config.yaml"
			if err := os.WriteFile(tmpFile, []byte(configYAML), 0644); err != nil {
				t.Fatalf("Failed to write test config: %v", err)
			}

			_, err := config.LoadConfig(tmpFile)
			if tt.wantErr && err == nil {
				t.Errorf("Expected validation error for session_timeout_s=%d", tt.timeout)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("Unexpected error for session_timeout_s=%d: %v", tt.timeout, err)
			}
		})
	}
}

func TestHAConfig_DisabledNoValidation(t *testing.T) {
	configYAML := `
clickhouse:
  host: localhost
  port: 9000
exporters:
  otel:
    - collector_address: localhost:4317
monitor:
  enabled: true
  min_trace_duration_ms: 100
  check_interval_s: 30
log_level: info
`
	tmpFile := t.TempDir() + "/test-config.yaml"
	if err := os.WriteFile(tmpFile, []byte(configYAML), 0644); err != nil {
		t.Fatalf("Failed to write test config: %v", err)
	}

	cfg, err := config.LoadConfig(tmpFile)
	if err != nil {
		t.Fatalf("LoadConfig should succeed when HA disabled: %v", err)
	}

	if cfg.HA.Active() {
		t.Error("HA should be inactive (no Keeper hosts)")
	}
}

func TestHAConfig_AuthPasswordWithEnvVar(t *testing.T) {
	t.Setenv("KEEPER_PASSWORD", "env-secret")

	configYAML := `
clickhouse:
  host: localhost
  port: 9000
exporters:
  otel:
    - collector_address: localhost:4317
monitor:
  enabled: true
  min_trace_duration_ms: 100
  check_interval_s: 30
log_level: info
ha:
  keeper:
    hosts:
      - "localhost:9181"
    auth_user: "click-dog"
    auth_password: "${KEEPER_PASSWORD}"
`
	tmpFile := t.TempDir() + "/test-config.yaml"
	if err := os.WriteFile(tmpFile, []byte(configYAML), 0644); err != nil {
		t.Fatalf("Failed to write test config: %v", err)
	}

	cfg, err := config.LoadConfig(tmpFile)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.HA.Keeper.AuthPassword != "env-secret" {
		t.Errorf("Expected env-expanded password 'env-secret', got %q", cfg.HA.Keeper.AuthPassword)
	}
}
