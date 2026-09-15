//go:build integration

package main

import (
	"context"
	"os/exec"
	"testing"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/coltconsulting/click-dog/internal/clickhouse"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/metrics"
	"github.com/coltconsulting/click-dog/internal/model"
	"github.com/coltconsulting/click-dog/internal/processor"
	"github.com/coltconsulting/click-dog/internal/resilience"
)

// -------------------------------------------------------------------
// Docker helpers
// -------------------------------------------------------------------

func dockerPause(t *testing.T, container string) {
	t.Helper()
	out, err := exec.Command("docker", "pause", container).CombinedOutput()
	if err != nil {
		t.Fatalf("docker pause %s failed: %v\n%s", container, err, out)
	}
	t.Logf("Paused container: %s", container)
}

func dockerUnpause(t *testing.T, container string) {
	t.Helper()
	out, err := exec.Command("docker", "unpause", container).CombinedOutput()
	if err != nil {
		t.Logf("docker unpause %s failed (may already be unpaused): %v\n%s", container, err, out)
	} else {
		t.Logf("Unpaused container: %s", container)
	}
}

func dockerStop(t *testing.T, container string) {
	t.Helper()
	out, err := exec.Command("docker", "stop", "-t", "5", container).CombinedOutput()
	if err != nil {
		t.Fatalf("docker stop %s failed: %v\n%s", container, err, out)
	}
	t.Logf("Stopped container: %s", container)
}

func dockerStart(t *testing.T, container string) {
	t.Helper()
	out, err := exec.Command("docker", "start", container).CombinedOutput()
	if err != nil {
		t.Logf("docker start %s failed: %v\n%s", container, err, out)
	} else {
		t.Logf("Started container: %s", container)
	}
}

// ensureAllRunning makes sure all containers are running/unpaused.
// Register with t.Cleanup to guarantee teardown even on test failure.
func ensureAllRunning(t *testing.T) {
	t.Helper()
	containers := []string{"clickhouse-int-1", "clickhouse-int-2", "clickhouse-int-3", "otel-integration"}
	for _, c := range containers {
		// Unpause ignores errors if not paused
		exec.Command("docker", "unpause", c).Run()
		// Start ignores errors if already running
		exec.Command("docker", "start", c).Run()
	}
	t.Log("ensureAllRunning: restored all containers")
}

// -------------------------------------------------------------------
// Test 1: Single Node Down — Strict Cluster Fails, Then Recovers
// -------------------------------------------------------------------

func TestDegradation_SingleNodeDown_StrictClusterFailsAndRecovers(t *testing.T) {
	t.Cleanup(func() { ensureAllRunning(t) })

	conns := waitForAllClickHouse(t, 60*time.Second)
	for _, c := range conns {
		defer c.Close()
	}

	fixtures := make([]tracedQueryFixture, 0, len(conns))
	for _, conn := range conns {
		fixtures = append(fixtures, seedTracedQueryFixture(t, conn, 2))
	}

	clusterReader := newClusterCHReader(t, 1, "test_cluster")
	defer clusterReader.Close()

	spans, err := clusterReader.FetchOpenTelemetrySpans(
		context.Background(), 0, 0, 0, 0, 5*time.Minute, 1000,
	)
	if err != nil {
		t.Fatalf("pre-stop cluster query failed: %v", err)
	}
	exactFixtureSpans(t, spans, fixtures...)

	dockerStop(t, "clickhouse-int-2")

	// The surviving local node is still readable while the configured strict
	// cluster correctly rejects an incomplete result.
	localReader := newCHReader(t, 1)
	defer localReader.Close()
	localSpans, err := localReader.FetchOpenTelemetrySpans(
		context.Background(), 0, 0, 0, 0, 5*time.Minute, 1000,
	)
	if err != nil {
		t.Fatalf("local node query failed while node 2 was down: %v", err)
	}
	exactFixtureSpans(t, localSpans, fixtures[0])

	queryCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_, err = clusterReader.FetchOpenTelemetrySpans(
		queryCtx, 0, 0, 0, 0, 5*time.Minute, 1000,
	)
	cancel()
	if err == nil {
		t.Fatal("strict cluster query succeeded with a shard down; expected an incomplete-cluster error")
	}

	dockerStart(t, "clickhouse-int-2")
	recoveredConn := waitForClickHouse(t, 2, 60*time.Second)
	recoveredConn.Close()

	recoveryFixtures := make([]tracedQueryFixture, 0, len(conns))
	for _, conn := range conns {
		recoveryFixtures = append(recoveryFixtures, seedTracedQueryFixture(t, conn, 1))
	}
	waitForFixtureSpansFromReader(t, clusterReader, recoveryFixtures, 15*time.Second)
}

// -------------------------------------------------------------------
// Test 2: Collector Down — Circuit Breaker Trips
// -------------------------------------------------------------------

func TestDegradation_CollectorDown_CircuitBreakerTrips(t *testing.T) {
	t.Cleanup(func() { ensureAllRunning(t) })

	conns := waitForAllClickHouse(t, 60*time.Second)
	defer conns[0].Close()
	defer conns[1].Close()
	defer conns[2].Close()
	waitForOTELCollector(t, 30*time.Second)

	otelExp := newOTELExporter(t, "degradation-test-cb")
	defer otelExp.Close(context.Background())

	queryFilter, err := filter.NewQueryFilter(config.FiltersConfig{})
	if err != nil {
		t.Fatalf("NewQueryFilter: %v", err)
	}

	clock := newFakeClock()
	cb := resilience.NewCircuitBreakerWithClock(config.CircuitBreakerConfig{
		Enabled:          true,
		FailureThreshold: 2,
		SuccessThreshold: 1,
		ResetTimeoutS:    3,
	}, clock)

	dockerPause(t, "otel-integration")

	for attempt := 1; attempt <= 2; attempt++ {
		// A new trace per cycle prevents an empty/no-op cycle from resetting the
		// consecutive-failure count.
		seedTracedQueryFixture(t, conns[0], 1)
		chReader := newCHReader(t, 1)
		pipeline := newIntegrationProcessor(t, chReader, otelExp, queryFilter, degradationConfig(), cb, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := pipeline.Process(ctx)
		cancel()
		chReader.Close()
		if err == nil {
			t.Fatalf("collector-down cycle %d succeeded; expected an export error", attempt)
		}
		if cb.Failures() != attempt {
			t.Fatalf("after failure %d, circuit failures = %d", attempt, cb.Failures())
		}
	}

	if cb.State() != resilience.CircuitOpen {
		t.Fatalf("expected circuit breaker to be open, got %s", cb.State())
	}
	if cb.Allow() {
		t.Fatal("circuit breaker allowed a request before its reset deadline")
	}

	dockerUnpause(t, "otel-integration")
	waitForOTELCollector(t, 30*time.Second)
	clock.Advance(4 * time.Second)
	recoveryFixture := seedTracedQueryFixture(t, conns[0], 1)
	recoveryReader := newCHReader(t, 1)
	defer recoveryReader.Close()
	pipeline := newIntegrationProcessor(t, recoveryReader, otelExp, queryFilter, degradationConfig(), cb, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err = pipeline.Process(ctx)
	cancel()
	if err != nil {
		t.Fatalf("post-recovery cycle failed: %v", err)
	}
	if cb.State() != resilience.CircuitClosed {
		t.Fatalf("circuit state after successful recovery = %s, want closed", cb.State())
	}
	waitForFixtureTraceIDs(t, "degradation-test-cb", recoveryFixture, 10*time.Second)
}

// -------------------------------------------------------------------
// Test 3: Unavailable Node — Backoff Increases, Then Resets
// -------------------------------------------------------------------

func TestDegradation_UnavailableNode_BackoffIncreasesAndRecovers(t *testing.T) {
	t.Cleanup(func() { ensureAllRunning(t) })

	conns := waitForAllClickHouse(t, 60*time.Second)
	defer conns[0].Close()
	defer conns[1].Close()
	defer conns[2].Close()

	chReader := newCHReader(t, 2)
	defer chReader.Close()

	baseInterval := 10 * time.Second
	poller := resilience.NewAdaptivePoller(baseInterval, config.BackoffConfig{
		Enabled:       true,
		MaxIntervalS:  120,
		BackoffFactor: 2.0,
	})

	dockerStop(t, "clickhouse-int-2")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	healthy := chReader.IsHealthy(ctx)
	cancel()
	if healthy {
		t.Fatal("stopped node 2 still reported healthy")
	}
	poller.RecordFailure()
	if got, want := poller.CurrentInterval(), 20*time.Second; got != want {
		t.Fatalf("interval after one failed poll = %v, want %v", got, want)
	}
	if !poller.IsBackedOff() {
		t.Fatal("poller did not report backed-off state after a failed poll")
	}

	dockerStart(t, "clickhouse-int-2")
	recoveredConn := waitForClickHouse(t, 2, 60*time.Second)
	recoveredConn.Close()
	waitForReaderHealthy(t, chReader, 15*time.Second)

	poller.RecordSuccess()
	if got := poller.CurrentInterval(); got != baseInterval {
		t.Fatalf("interval after recovery = %v, want %v", got, baseInterval)
	}
	if poller.IsBackedOff() {
		t.Fatal("poller remained backed off after a successful health check")
	}
}

// -------------------------------------------------------------------
// Helpers
// -------------------------------------------------------------------

func mustLRU(t *testing.T, size int) *lru.Cache[model.SpanKey, bool] {
	t.Helper()
	cache, err := lru.New[model.SpanKey, bool](size)
	if err != nil {
		t.Fatalf("failed to create LRU cache: %v", err)
	}
	return cache
}

func degradationConfig() *config.Config {
	return &config.Config{
		Monitor: config.MonitorConfig{
			MinTraceDurationMs: 0,
			CheckIntervalS:     30,
			LookbackS:          300,
			MaxSpansPerCycle:   1000,
			DedupCacheSize:     1000,
		},
	}
}

func newIntegrationProcessor(
	t *testing.T,
	reader *clickhouse.ClickHouseReader,
	exporter model.SpanExporter,
	f *filter.QueryFilter,
	cfg *config.Config,
	cb *resilience.CircuitBreaker,
	poller *resilience.AdaptivePoller,
) *processor.Pipeline {
	t.Helper()
	dedupCacheSize := cfg.Monitor.DedupCacheSize
	if dedupCacheSize <= 0 {
		dedupCacheSize = 1000
	}
	pipeline, err := processor.NewPipeline(processor.Pipeline{
		Reader:         reader,
		Exporter:       exporter,
		Filter:         f,
		Config:         cfg,
		SeenSpans:      mustLRU(t, dedupCacheSize),
		CircuitBreaker: cb,
		Poller:         poller,
		CanaryQuerier:  reader,
		Metrics:        metrics.NewMetrics(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return pipeline
}

func waitForReaderHealthy(t *testing.T, reader *clickhouse.ClickHouseReader, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		healthy := reader.IsHealthy(ctx)
		cancel()
		if healthy {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("ClickHouse reader did not become healthy within %v", timeout)
}

func waitForFixtureSpansFromReader(
	t *testing.T,
	reader *clickhouse.ClickHouseReader,
	fixtures []tracedQueryFixture,
	timeout time.Duration,
) {
	t.Helper()
	wanted := make(map[string]struct{})
	for _, fixture := range fixtures {
		for _, traceID := range fixture.TraceIDs {
			wanted[traceID.String()] = struct{}{}
		}
	}
	found := make(map[string]struct{}, len(wanted))
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		spans, err := reader.FetchOpenTelemetrySpans(ctx, 0, 0, 0, 0, 5*time.Minute, 1000)
		cancel()
		lastErr = err
		if err == nil {
			for _, span := range spans {
				traceID := span.TraceID.String()
				if _, ok := wanted[traceID]; ok {
					found[traceID] = struct{}{}
				}
			}
			if len(found) == len(wanted) {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("reader recovered but returned %d/%d exact fixture traces within %v (last error: %v)", len(found), len(wanted), timeout, lastErr)
}

func waitForFixtureTraceIDs(t *testing.T, serviceName string, fixture tracedQueryFixture, timeout time.Duration) {
	t.Helper()
	wanted := make(map[string]struct{}, len(fixture.TraceIDs))
	for _, traceID := range fixture.TraceIDs {
		wanted[traceID.String()] = struct{}{}
	}
	waitForOTELFileObservations(t, timeout, func(observations []otelSpanObservation) bool {
		remaining := make(map[string]struct{}, len(wanted))
		for traceID := range wanted {
			remaining[traceID] = struct{}{}
		}
		for _, observation := range observations {
			if observation.ServiceName == serviceName {
				delete(remaining, observation.TraceID.String())
			}
		}
		return len(remaining) == 0
	}, "recovered pipeline did not export every exact fixture trace")
}
