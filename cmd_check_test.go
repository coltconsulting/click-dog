package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/clickhouse"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/export"
	"google.golang.org/grpc"
)

func newOTELTestConfig(addr string) config.OTELConfig {
	return config.OTELConfig{
		CollectorAddress: addr,
		ServiceName:      "test-service",
		MaxQueryLength:   100000,
	}
}

func TestOTELExporter_CheckConnectivity_Success(t *testing.T) {
	// Start a dummy gRPC server
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	srv := grpc.NewServer()
	go func() { _ = srv.Serve(lis) }()
	defer srv.GracefulStop()

	exp, err := export.NewOTELExporter(newOTELTestConfig(lis.Addr().String()))
	if err != nil {
		t.Fatalf("failed to create exporter: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = exp.Close(ctx)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := exp.CheckConnectivity(ctx); err != nil {
		t.Errorf("expected connectivity check to pass: %v", err)
	}
}

func TestOTELExporter_CheckConnectivity_BadAddress(t *testing.T) {
	exp, err := export.NewOTELExporter(newOTELTestConfig("localhost:1"))
	if err != nil {
		t.Fatalf("failed to create exporter: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = exp.Close(ctx)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err = exp.CheckConnectivity(ctx)
	if err == nil {
		t.Error("expected connectivity check to fail for bad address")
	}
}

func TestPrintReadinessChecks_FailExits(t *testing.T) {
	var out bytes.Buffer
	ok, _ := printReadinessChecks(&out, []clickhouse.ReadinessCheck{
		{Name: "span_log readable", Status: clickhouse.ReadinessPass, Detail: "selectable"},
		{Name: "query_log readable", Status: clickhouse.ReadinessFail, Detail: "ACCESS_DENIED", Remedy: "grant SELECT"},
	})
	if ok {
		t.Error("expected printReadinessChecks to return false when a check fails")
	}
	s := out.String()
	if !strings.Contains(s, "[FAIL] query_log readable") {
		t.Errorf("output missing FAIL line:\n%s", s)
	}
	if !strings.Contains(s, "fix: grant SELECT") {
		t.Errorf("FAIL line should print its remedy:\n%s", s)
	}
}

func TestPrintReadinessChecks_WarnDoesNotFail(t *testing.T) {
	var out bytes.Buffer
	ok, warned := printReadinessChecks(&out, []clickhouse.ReadinessCheck{
		{Name: "span_log recent data", Status: clickhouse.ReadinessWarn, Detail: "no recent spans", Remedy: "widen lookback"},
		{Name: "normalized_query_hash support", Status: clickhouse.ReadinessPass, Detail: "available"},
	})
	if !ok {
		t.Error("warnings should not fail the readiness check")
	}
	if !warned {
		t.Error("a WARN line should set warned=true so the closing message can't claim 'All checks passed'")
	}
	s := out.String()
	if !strings.Contains(s, "fix: widen lookback") {
		t.Errorf("WARN line should print its remedy:\n%s", s)
	}
}

func TestPrintReadinessChecks_PassHidesRemedy(t *testing.T) {
	var out bytes.Buffer
	printReadinessChecks(&out, []clickhouse.ReadinessCheck{
		{Name: "span_log readable", Status: clickhouse.ReadinessPass, Detail: "selectable", Remedy: "should not show"},
	})
	if strings.Contains(out.String(), "should not show") {
		t.Errorf("PASS lines must not print a remedy:\n%s", out.String())
	}
}

func TestCertHostnameHint(t *testing.T) {
	const host = "fsnpch201"
	tests := []struct {
		name    string
		err     error
		wantHit bool
	}{
		{
			name:    "wrapped x509 hostname mismatch (string)",
			err:     errors.New("failed to ping ClickHouse: tls: failed to verify certificate: x509: certificate is valid for fsnpch201.day8.com.au, voz-clickhouse-3.day8.com.au, not fsnpch201"),
			wantHit: true,
		},
		{
			name:    "typed HostnameError (chain preserved)",
			err:     fmt.Errorf("ping: %w", x509.HostnameError{Host: host}),
			wantHit: true,
		},
		{
			name:    "unrelated gRPC transient failure",
			err:     errors.New("localhost:4317: gRPC connection in unexpected state: TRANSIENT_FAILURE"),
			wantHit: false,
		},
		{
			name:    "unknown authority needs a different fix, not the FQDN hint",
			err:     errors.New("x509: certificate signed by unknown authority"),
			wantHit: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := certHostnameHint(tt.err, host)
			if (got != "") != tt.wantHit {
				t.Fatalf("certHostnameHint() = %q, wantHit=%v", got, tt.wantHit)
			}
			if tt.wantHit && !strings.Contains(got, host) {
				t.Errorf("hint should name the configured host %q: %q", host, got)
			}
		})
	}
}

func TestQueryLogNeeded(t *testing.T) {
	off := false
	tests := []struct {
		name   string
		mutate func(*config.Config)
		want   bool
	}{
		{"enrichment on by default", func(c *config.Config) {}, true},
		{"enrichment off, no filters", func(c *config.Config) { c.Monitor.EnrichFromQueryLog = &off }, false},
		{"whitelist_users", func(c *config.Config) {
			c.Monitor.EnrichFromQueryLog = &off
			c.Filters.WhitelistUsers = []string{"app"}
		}, true},
		{"blacklist_users", func(c *config.Config) {
			c.Monitor.EnrichFromQueryLog = &off
			c.Filters.BlacklistUsers = []string{"backup"}
		}, true},
		{"whitelist_ips", func(c *config.Config) {
			c.Monitor.EnrichFromQueryLog = &off
			c.Filters.WhitelistIPs = []string{"10.0.0.0/8"}
		}, true},
		// The live path reads statements from the span log.
		{"blacklist_queries", func(c *config.Config) {
			c.Monitor.EnrichFromQueryLog = &off
			c.Filters.BlacklistQueries = []string{"secret"}
		}, false},
	}
	for _, tt := range tests {
		cfg := &config.Config{}
		tt.mutate(cfg)
		if got := queryLogNeeded(cfg); got != tt.want {
			t.Errorf("%s: queryLogNeeded = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestQueryIDNeeded(t *testing.T) {
	off := false
	cfg := &config.Config{}
	cfg.Monitor.EnrichFromQueryLog = &off
	if queryIDNeeded(cfg) {
		t.Error("no enrichment and no query filters should not need query_id")
	}
	cfg.Filters.BlacklistQueries = []string{"secret"}
	if !queryIDNeeded(cfg) {
		t.Error("blacklist_queries reads trace statements by query_id")
	}
	cfg.Filters.BlacklistQueries = nil
	cfg.Filters.WhitelistIPs = []string{"10.0.0.0/8"}
	if !queryIDNeeded(cfg) {
		t.Error("whitelist_ips resolves clients by query_id")
	}
}
