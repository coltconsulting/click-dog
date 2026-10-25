//go:build integration

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/model"
)

type materializationObservingReader struct {
	tracingTestReader
	missingReads int
}

func (r *materializationObservingReader) FetchSpansForTraceIDs(ctx context.Context, ids []string, days, limit int, blacklist []string) ([]model.OpenTelemetrySpan, error) {
	spans, err := r.tracingTestReader.FetchSpansForTraceIDs(ctx, ids, days, limit, blacklist)
	var exception *clickhouse.Exception
	if errors.As(err, &exception) && exception.Code == chErrUnknownTable {
		r.missingReads++
	}
	return spans, err
}

func TestTracingCommand_FirstInvocationOnFreshNodeIntegration(t *testing.T) {
	// A separate empty-data node prevents prior fixtures or SYSTEM FLUSH LOGS
	// from pre-creating the table and silently bypassing the startup regression.
	name := "click-dog-fresh-" + newIntegrationFixtureMarker()
	configPath := filepath.Join(t.TempDir(), "tracing.xml")
	if err := os.WriteFile(configPath, []byte(`<clickhouse><opentelemetry_span_log>
<database>system</database><table>opentelemetry_span_log</table>
<flush_interval_milliseconds>3000</flush_interval_milliseconds>
</opentelemetry_span_log></clickhouse>`), 0o644); err != nil {
		t.Fatal(err)
	}
	docker := func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	image := os.Getenv("CLICKHOUSE_FRESH_IMAGE")
	if image == "" {
		image = "clickhouse/clickhouse-server:24.1"
	}
	t.Cleanup(func() { docker("rm", "-f", "-v", name) })
	// ClickHouse 25.x otherwise restricts passwordless default users to the
	// container's loopback, rejecting published-port traffic from the bridge.
	// Skip that rewrite for this disposable fixture, published on host loopback only.
	docker("run", "--detach", "--rm", "--name", name,
		"--publish", "127.0.0.1::9000", "--env", "CLICKHOUSE_USER=default", "--env", "CLICKHOUSE_PASSWORD=",
		"--env", "CLICKHOUSE_SKIP_USER_SETUP=1",
		"--volume", configPath+":/etc/clickhouse-server/config.d/tracing.xml:ro", image)
	// Keep this test serial: shared node-1 helpers use this process-wide override.
	t.Setenv("CLICKHOUSE1_ADDR", docker("port", name, "9000/tcp"))
	conn := waitForClickHouse(t, 1, 30*time.Second)
	defer func() { _ = conn.Close() }()
	var exists uint8
	if err := conn.QueryRow(context.Background(), "EXISTS TABLE system.opentelemetry_span_log").Scan(&exists); err != nil || exists != 0 {
		t.Fatalf("fresh span-log table exists=%d, error=%v; must be absent before the first probe", exists, err)
	}
	cfg := &config.Config{
		ClickHouse: config.ClickHouseConfig{
			Host: integrationCHHost(1), Port: integrationCHPort(1), Database: "default", Username: "default",
			MaxOpenConns: 2, MaxIdleConns: 1, QueryTimeoutS: 30, MaxMemoryUsage: 104857600,
		},
		Monitor: config.MonitorConfig{ExportTimeoutS: 5},
	}
	deps := productionTracingTestDependencies()
	newReader := deps.newReader
	var observed *materializationObservingReader
	deps.newReader = func(ctx context.Context, cfg *config.Config) (tracingTestReader, error) {
		reader, err := newReader(ctx, cfg)
		if err != nil {
			return nil, err
		}
		observed = &materializationObservingReader{tracingTestReader: reader}
		return observed, nil
	}
	assertNativeTracingCommand(t, cfg, deps)
	if observed == nil || observed.missingReads == 0 {
		t.Fatal("first invocation did not exercise UNKNOWN_TABLE before the first log flush")
	}
}
