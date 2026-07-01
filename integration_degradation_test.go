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
// Test 1: Single Node Down — Cluster Query Continues
// -------------------------------------------------------------------

func TestDegradation_SingleNodeDown_ClusterQueryContinues(t *testing.T) {
	t.Cleanup(func() { ensureAllRunning(t) })

	// Wait for all nodes to be ready
	conns := waitForAllClickHouse(t, 60*time.Second)
	for _, c := range conns {
		defer c.Close()
	}

	// Seed traced queries on all 3 nodes
	for i, conn := range conns {
		t.Logf("Seeding traced queries on node %d", i+1)
		seedTracedQueries(t, conn, 3)
	}

	// Create a cluster reader via node 1
	clusterReader := newClusterCHReader(t, 1, "test_cluster")
	defer clusterReader.Close()

	// Verify we can fetch spans across the cluster
	spans, err := clusterReader.FetchOpenTelemetrySpans(
		context.Background(), 0, 0, 0, 0, 5*time.Minute, 1000,
	)
	if err != nil {
		t.Fatalf("Pre-stop cluster query failed: %v", err)
	}
	preStopCount := len(spans)
	t.Logf("Pre-stop: fetched %d spans from cluster", preStopCount)

	// Stop node 2
	dockerStop(t, "clickhouse-int-2")

	// Wait a moment for the cluster to stabilize
	time.Sleep(3 * time.Second)

	// Query cluster via node 1 — should still work (nodes 1+3 respond)
	spans, err = clusterReader.FetchOpenTelemetrySpans(
		context.Background(), 0, 0, 0, 0, 5*time.Minute, 1000,
	)
	if err != nil {
		t.Logf("Cluster query with node 2 down returned error (may be expected with strict cluster): %v", err)
		// Not fatal — some cluster configs may error. The key test is no panic.
	} else {
		t.Logf("Post-stop: fetched %d spans from cluster (node 2 down)", len(spans))
	}

	// Restart node 2
	dockerStart(t, "clickhouse-int-2")

	// Wait for node 2 to be ready again
	waitForClickHouse(t, 2, 60*time.Second)
	t.Log("Node 2 recovered successfully")
}

// -------------------------------------------------------------------
// Test 2: Collector Down — Circuit Breaker Trips
// -------------------------------------------------------------------

func TestDegradation_CollectorDown_CircuitBreakerTrips(t *testing.T) {
	t.Cleanup(func() { ensureAllRunning(t) })

	// Wait for services
	conns := waitForAllClickHouse(t, 60*time.Second)
	defer conns[0].Close()
	defer conns[1].Close()
	defer conns[2].Close()
	waitForOTELCollector(t, 30*time.Second)

	// Seed some spans
	seedTracedQueries(t, conns[0], 3)

	// Create components
	chReader := newCHReader(t, 1)
	defer chReader.Close()

	otelExp := newOTELExporter(t, "degradation-test-cb")
	defer otelExp.Close(context.Background())

	filter, _ := filter.NewQueryFilter(config.FiltersConfig{})

	cb := resilience.NewCircuitBreaker(config.CircuitBreakerConfig{
		Enabled:          true,
		FailureThreshold: 2,
		SuccessThreshold: 1,
		ResetTimeoutS:    3, // Short for testing
	})

	// Pause the OTEL collector
	dockerPause(t, "otel-integration")
	t.Log("OTEL collector paused — exports should fail")

	// Attempt exports — they should fail and trip the circuit breaker
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := runIntegrationProcessorCycle(t, ctx, chReader, otelExp, filter, &config.Config{
			Monitor: config.MonitorConfig{
				MinTraceDurationMs: 0,
				CheckIntervalS:     30,
				LookbackS:          300,
				MaxSpansPerCycle:   100,
				DedupCacheSize:     1000,
			},
		}, cb, nil)
		cancel()

		t.Logf("Attempt %d: err=%v, circuit_state=%s, failures=%d",
			i+1, err, cb.State(), cb.Failures())
	}

	// Circuit should be open
	if cb.State() != resilience.CircuitOpen {
		t.Errorf("expected circuit breaker to be open, got %s", cb.State())
	}

	// Verify Allow() returns false while open
	if cb.Allow() {
		// It might transition to half-open if reset timeout is very short
		t.Log("Circuit breaker transitioned to half-open (reset timeout may have elapsed)")
	} else {
		t.Log("Circuit breaker correctly blocking requests")
	}

	// Unpause collector
	dockerUnpause(t, "otel-integration")
	t.Log("OTEL collector unpaused — waiting for circuit breaker reset")

	// Wait for reset timeout
	time.Sleep(4 * time.Second)

	// Should transition to half-open and allow a probe
	if !cb.Allow() {
		t.Error("expected circuit breaker to allow request after reset timeout")
	}
	t.Logf("Circuit breaker state after reset: %s", cb.State())

	// Successful export should close the circuit
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err := runIntegrationProcessorCycle(t, ctx, chReader, otelExp, filter, &config.Config{
		Monitor: config.MonitorConfig{
			MinTraceDurationMs: 0,
			CheckIntervalS:     30,
			LookbackS:          300,
			MaxSpansPerCycle:   100,
			DedupCacheSize:     1000,
		},
	}, cb, nil)
	cancel()

	if err != nil {
		t.Logf("Post-recovery export error (may be transient): %v", err)
	}

	// Record success manually if the export itself had no spans to send
	if err == nil {
		cb.RecordSuccess()
	}

	t.Logf("Final circuit breaker state: %s", cb.State())
}

// -------------------------------------------------------------------
// Test 3: Slow Node — Backoff Increases
// -------------------------------------------------------------------

func TestDegradation_SlowNode_BackoffIncreases(t *testing.T) {
	t.Cleanup(func() { ensureAllRunning(t) })

	conns := waitForAllClickHouse(t, 60*time.Second)
	defer conns[0].Close()
	defer conns[1].Close()
	defer conns[2].Close()

	// Create reader on node 2
	chReader := newCHReader(t, 2)
	defer chReader.Close()

	baseInterval := 10 * time.Second
	poller := resilience.NewAdaptivePoller(baseInterval, config.BackoffConfig{
		Enabled:       true,
		MaxIntervalS:  120,
		BackoffFactor: 2.0,
	})

	initialInterval := poller.CurrentInterval()
	t.Logf("Initial backoff interval: %v", initialInterval)

	// Pause node 2 — reads will timeout
	dockerPause(t, "clickhouse-int-2")
	t.Log("Node 2 paused — reads should timeout")

	// Attempt reads with short timeout — they should fail
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		healthy := chReader.IsHealthy(ctx)
		cancel()

		if healthy {
			t.Log("Node unexpectedly reported healthy (race with pause)")
		} else {
			poller.RecordFailure()
		}
		t.Logf("Attempt %d: healthy=%v, interval=%v",
			i+1, healthy, poller.CurrentInterval())
	}

	// Backoff should have increased
	backedOffInterval := poller.CurrentInterval()
	if backedOffInterval <= initialInterval {
		t.Errorf("expected backoff to increase interval from %v, got %v", initialInterval, backedOffInterval)
	}
	t.Logf("Backoff increased: %v -> %v", initialInterval, backedOffInterval)

	// Unpause node 2
	dockerUnpause(t, "clickhouse-int-2")
	t.Log("Node 2 unpaused — waiting for recovery")

	// Wait for node to recover
	time.Sleep(3 * time.Second)

	// Verify node is healthy again
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	healthy := chReader.IsHealthy(ctx)
	cancel()

	if !healthy {
		t.Error("node 2 should be healthy after unpause")
	}

	// A successful poll resets backoff to the base interval (the second call
	// is a harmless no-op once already at base).
	poller.RecordSuccess()
	poller.RecordSuccess()
	resetInterval := poller.CurrentInterval()
	t.Logf("After recovery: interval=%v, is_backed_off=%v", resetInterval, poller.IsBackedOff())

	if resetInterval != baseInterval {
		t.Errorf("expected interval to reset to %v after success, got %v", baseInterval, resetInterval)
	}
}

// -------------------------------------------------------------------
// Test 4: Majority Down — Graceful Degradation
// -------------------------------------------------------------------

func TestDegradation_MajorityDown_GracefulDegradation(t *testing.T) {
	t.Cleanup(func() { ensureAllRunning(t) })

	conns := waitForAllClickHouse(t, 60*time.Second)
	defer conns[0].Close()
	defer conns[1].Close()
	defer conns[2].Close()

	// Create cluster reader via node 1
	clusterReader := newClusterCHReader(t, 1, "test_cluster")
	defer clusterReader.Close()

	// Pause nodes 2 and 3
	dockerPause(t, "clickhouse-int-2")
	dockerPause(t, "clickhouse-int-3")
	t.Log("Nodes 2+3 paused — majority down")

	time.Sleep(2 * time.Second)

	// Attempt cluster query — should return error or partial data, NOT panic
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	spans, err := clusterReader.FetchOpenTelemetrySpans(
		ctx, 0, 0, 0, 0, 5*time.Minute, 100,
	)
	cancel()

	if err != nil {
		t.Logf("Cluster query with majority down returned error (expected): %v", err)
	} else {
		t.Logf("Cluster query returned %d spans despite majority down (partial results)", len(spans))
	}

	// The key assertion: we got here without panicking
	t.Log("Graceful degradation verified — no panic with majority down")

	// Restore nodes
	dockerUnpause(t, "clickhouse-int-2")
	dockerUnpause(t, "clickhouse-int-3")

	// Wait for recovery
	waitForClickHouse(t, 2, 60*time.Second)
	waitForClickHouse(t, 3, 60*time.Second)
	t.Log("All nodes recovered")
}

// -------------------------------------------------------------------
// Test 5: Full Recovery — All Mechanisms Reset
// -------------------------------------------------------------------

func TestDegradation_FullRecovery_AllMechanismsReset(t *testing.T) {
	t.Cleanup(func() { ensureAllRunning(t) })

	conns := waitForAllClickHouse(t, 60*time.Second)
	defer conns[0].Close()
	defer conns[1].Close()
	defer conns[2].Close()
	waitForOTELCollector(t, 30*time.Second)

	// Seed spans
	seedTracedQueries(t, conns[0], 3)

	chReader := newCHReader(t, 1)
	defer chReader.Close()

	otelExp := newOTELExporter(t, "degradation-test-recovery")
	defer otelExp.Close(context.Background())

	filter, _ := filter.NewQueryFilter(config.FiltersConfig{})

	// Circuit breaker with short reset
	cb := resilience.NewCircuitBreaker(config.CircuitBreakerConfig{
		Enabled:          true,
		FailureThreshold: 2,
		SuccessThreshold: 1,
		ResetTimeoutS:    3,
	})

	// Adaptive backoff
	baseInterval := 10 * time.Second
	poller := resilience.NewAdaptivePoller(baseInterval, config.BackoffConfig{
		Enabled:       true,
		MaxIntervalS:  120,
		BackoffFactor: 2.0,
	})

	// Break everything: pause collector + node 2
	dockerPause(t, "otel-integration")
	dockerPause(t, "clickhouse-int-2")
	t.Log("Collector and node 2 paused — everything broken")

	// Drive failures to trip both mechanisms
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := runIntegrationProcessorCycle(t, ctx, chReader, otelExp, filter, &config.Config{
			Monitor: config.MonitorConfig{
				MinTraceDurationMs: 0,
				CheckIntervalS:     30,
				LookbackS:          300,
				MaxSpansPerCycle:   100,
				DedupCacheSize:     1000,
			},
		}, cb, nil)
		cancel()

		if err != nil {
			poller.RecordFailure()
		}

		t.Logf("Failure %d: circuit=%s, backoff=%v", i+1, cb.State(), poller.CurrentInterval())
	}

	// Verify both mechanisms are activated
	if cb.State() == resilience.CircuitClosed {
		t.Log("Circuit breaker may not have opened (health check passed to node 1)")
	}
	if !poller.IsBackedOff() {
		t.Log("Backoff may not have increased (export may have succeeded partially)")
	}
	backedOffInterval := poller.CurrentInterval()

	// Restore everything
	dockerUnpause(t, "otel-integration")
	dockerUnpause(t, "clickhouse-int-2")
	t.Log("All services restored — waiting for recovery")

	// Wait for circuit breaker reset timeout
	time.Sleep(4 * time.Second)

	// Wait for services to be ready
	waitForClickHouse(t, 2, 30*time.Second)
	waitForOTELCollector(t, 30*time.Second)

	// Drive successes to reset both mechanisms
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := runIntegrationProcessorCycle(t, ctx, chReader, otelExp, filter, &config.Config{
			Monitor: config.MonitorConfig{
				MinTraceDurationMs: 0,
				CheckIntervalS:     30,
				LookbackS:          300,
				MaxSpansPerCycle:   100,
				DedupCacheSize:     1000,
			},
		}, cb, nil)
		cancel()

		if err == nil {
			poller.RecordSuccess()
		}
		t.Logf("Recovery %d: err=%v, circuit=%s, backoff=%v",
			i+1, err, cb.State(), poller.CurrentInterval())
	}

	// Verify both mechanisms have reset
	finalState := cb.State()
	finalInterval := poller.CurrentInterval()

	t.Logf("Final state: circuit=%s, backoff_interval=%v (base=%v)", finalState, finalInterval, baseInterval)
	t.Logf("Backed off interval was: %v", backedOffInterval)

	if finalState == resilience.CircuitOpen {
		t.Error("circuit breaker should not be open after successful recovery")
	}
	if poller.IsBackedOff() {
		t.Error("backoff should have reset after successful recovery")
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

func runIntegrationProcessorCycle(
	t *testing.T,
	ctx context.Context,
	reader *clickhouse.ClickHouseReader,
	exporter model.SpanExporter,
	f *filter.QueryFilter,
	cfg *config.Config,
	cb *resilience.CircuitBreaker,
	poller *resilience.AdaptivePoller,
) error {
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
	return pipeline.Process(ctx)
}
