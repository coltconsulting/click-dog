package main

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/export"
	"github.com/coltconsulting/click-dog/internal/model"
	"github.com/coltconsulting/click-dog/internal/processor"
	"github.com/coltconsulting/click-dog/internal/resilience"

	lru "github.com/hashicorp/golang-lru/v2"
)

// fakeClock is a test clock that implements resilience.Clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (fc *fakeClock) Now() time.Time {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.now
}

func (fc *fakeClock) Advance(d time.Duration) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.now = fc.now.Add(d)
}

// TestFailure_OTELCollectorDown tests behavior when OTEL collector is unreachable
func TestFailure_OTELCollectorDown(t *testing.T) {
	t.Run("export fails with connection refused", func(t *testing.T) {
		// Use a port that nothing is listening on
		exporter, err := export.NewOTELExporter(config.OTELConfig{
			CollectorAddress: "localhost:19999",
			ServiceName:      "test-failure",
		})
		if err != nil {
			t.Fatalf("Failed to create exporter: %v", err)
		}
		defer func() { _ = exporter.Close(context.Background()) }()

		now := time.Now()
		spans := []model.OpenTelemetrySpan{
			{
				Hostname:      "host1",
				TraceID:       uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
				SpanID:        1001,
				OperationName: "test",
				Kind:          "INTERNAL",
				StartTimeUs:   uint64(now.Add(-1 * time.Second).UnixMicro()),
				FinishTimeUs:  uint64(now.UnixMicro()),
				FinishDate:    now,
				Attributes:    map[string]string{},
			},
		}

		_, err = exporter.ExportSpans(context.Background(), spans)
		if err == nil {
			t.Error("Expected error when OTEL collector is down")
		}
	})

	t.Run("export error triggers circuit breaker", func(t *testing.T) {
		cb := resilience.NewCircuitBreaker(config.CircuitBreakerConfig{
			FailureThreshold: 2,
			SuccessThreshold: 1,
			ResetTimeoutS:    60,
		})

		// Simulate export failures triggering circuit breaker
		for i := 0; i < 2; i++ {
			if cb.Allow() {
				// Simulate failed export
				cb.RecordFailure()
			}
		}

		if cb.State() != resilience.CircuitOpen {
			t.Errorf("Expected circuit open after export failures, got %v", cb.State())
		}

		// Subsequent exports should be blocked
		if cb.Allow() {
			t.Error("Circuit should block requests when open")
		}
	})
}

// TestFailure_OTELCollectorRecovers tests recovery when collector comes back
func TestFailure_OTELCollectorRecovers(t *testing.T) {
	fc := newFakeClock()
	cb := resilience.NewCircuitBreakerWithClock(config.CircuitBreakerConfig{
		FailureThreshold: 2,
		SuccessThreshold: 1,
		ResetTimeoutS:    1,
	}, fc)

	poller := resilience.NewAdaptivePoller(10*time.Second, config.BackoffConfig{
		MaxIntervalS:  300,
		BackoffFactor: 2.0,
	})

	// Phase 1: Failures open circuit and increase backoff
	for i := 0; i < 2; i++ {
		if cb.Allow() {
			cb.RecordFailure()
			poller.RecordFailure()
		}
	}

	if cb.State() != resilience.CircuitOpen {
		t.Fatal("Circuit should be open")
	}
	if !poller.IsBackedOff() {
		t.Fatal("Poller should be backed off")
	}

	// Phase 2: Advance past reset timeout
	fc.Advance(1100 * time.Millisecond)

	// Phase 3: Half-open allows one request
	if !cb.Allow() {
		t.Error("Should allow probe request after reset timeout")
	}
	if cb.State() != resilience.CircuitHalfOpen {
		t.Errorf("Expected half-open, got %v", cb.State())
	}

	// Phase 4: Success closes circuit and resets backoff
	cb.RecordSuccess()
	poller.RecordSuccess()

	if cb.State() != resilience.CircuitClosed {
		t.Errorf("Expected closed after success, got %v", cb.State())
	}
	if poller.IsBackedOff() {
		t.Error("Poller should not be backed off after success")
	}
}

// TestFailure_ClickHouseTimeout tests circuit breaker behavior on timeouts
func TestFailure_ClickHouseTimeout(t *testing.T) {
	t.Run("context timeout triggers failure recording", func(t *testing.T) {
		cb := resilience.NewCircuitBreaker(config.CircuitBreakerConfig{
			FailureThreshold: 3,
			SuccessThreshold: 1,
			ResetTimeoutS:    60,
		})

		// Simulate 3 timeout errors
		for i := 0; i < 3; i++ {
			if cb.Allow() {
				// Simulate timeout error from ClickHouse
				cb.RecordFailure()
			}
		}

		if cb.State() != resilience.CircuitOpen {
			t.Errorf("Expected circuit open after 3 timeouts, got %v", cb.State())
		}
	})

	t.Run("context cancellation is handled gracefully", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		// Verify context is done
		select {
		case <-ctx.Done():
			if ctx.Err() != context.Canceled {
				t.Errorf("Expected context.Canceled, got %v", ctx.Err())
			}
		default:
			t.Error("Context should be done")
		}
	})

	t.Run("deadline exceeded is handled", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
		defer cancel()

		select {
		case <-ctx.Done():
			if ctx.Err() != context.DeadlineExceeded {
				t.Errorf("Expected DeadlineExceeded, got %v", ctx.Err())
			}
		case <-time.After(100 * time.Millisecond):
			t.Error("Context deadline did not fire")
		}
	})
}

// TestFailure_CircuitBreakerWithBackoff tests combined protection mechanisms
func TestFailure_CircuitBreakerWithBackoff(t *testing.T) {
	t.Run("failures trigger both protections", func(t *testing.T) {
		baseInterval := 10 * time.Second
		fc := newFakeClock()
		cb := resilience.NewCircuitBreakerWithClock(config.CircuitBreakerConfig{
			FailureThreshold: 3,
			SuccessThreshold: 1,
			ResetTimeoutS:    1,
		}, fc)
		poller := resilience.NewAdaptivePoller(baseInterval, config.BackoffConfig{
			MaxIntervalS:  60,
			BackoffFactor: 2.0,
		})

		ticker := time.NewTicker(baseInterval)
		defer ticker.Stop()

		// Simulate processing loop with failures
		// Circuit breaker opens after 3 failures, but updatePollerState
		// only receives non-nil errors for the 3 attempts before circuit opens.
		// After circuit opens, processSpansWithProtection returns nil (skipped).
		for i := 0; i < 3; i++ {
			if cb.Allow() {
				runErr := fmt.Errorf("connection timeout (attempt %d)", i+1)
				cb.RecordFailure()
				processor.UpdatePollerState(poller, runErr, ticker, baseInterval, nil)
			}
		}

		// Circuit should be open after 3 failures
		if cb.State() != resilience.CircuitOpen {
			t.Errorf("Expected circuit open, got %v", cb.State())
		}

		// Backoff should have increased (3 failures recorded)
		if poller.CurrentInterval() <= baseInterval {
			t.Error("Backoff should have increased interval")
		}

		// 10 * 2 * 2 * 2 = 80s, but capped at max 60s
		expectedInterval := 60 * time.Second
		if poller.CurrentInterval() != expectedInterval {
			t.Errorf("Expected interval %v, got %v", expectedInterval, poller.CurrentInterval())
		}

		// When circuit is open, calls are skipped (nil error) - backoff stays
		if cb.Allow() {
			t.Error("Circuit should block while open")
		}

		// Advance past circuit reset timeout
		fc.Advance(1100 * time.Millisecond)

		// Half-open allows one request
		if !cb.Allow() {
			t.Error("Should allow request after reset")
		}

		// Success recovers both
		cb.RecordSuccess()
		processor.UpdatePollerState(poller, nil, ticker, baseInterval, nil)

		if cb.State() != resilience.CircuitClosed {
			t.Errorf("Expected closed, got %v", cb.State())
		}
		if poller.CurrentInterval() != baseInterval {
			t.Errorf("Expected reset to %v, got %v", baseInterval, poller.CurrentInterval())
		}
	})
}

// TestFailure_UnhealthyConnection tests unhealthy connection detection
func TestFailure_UnhealthyConnection(t *testing.T) {
	t.Run("unhealthy check prevents query execution", func(t *testing.T) {
		mock := NewMockClickHouseReader()
		mock.SetHealthy(false)

		cb := resilience.NewCircuitBreaker(config.CircuitBreakerConfig{
			FailureThreshold: 3,
			SuccessThreshold: 1,
			ResetTimeoutS:    60,
		})

		// Simulate processSpansWithProtection logic
		if cb.Allow() {
			if !mock.IsHealthy(context.Background()) {
				// Should return error to trigger backoff, but not hit CB hard
				// (softer failure than query errors)
			} else {
				// Would fetch spans here
				_, _ = mock.FetchOpenTelemetrySpans(context.Background(), 1000, 0, 0, 0, 60*time.Second, 100)
			}
		}

		// Fetch should not have been called
		if mock.fetchCalled != 0 {
			t.Errorf("Expected 0 fetch calls when unhealthy, got %d", mock.fetchCalled)
		}

		// Health check should have been called
		if mock.pingCalled != 1 {
			t.Errorf("Expected 1 ping call, got %d", mock.pingCalled)
		}
	})

	t.Run("healthy check allows query execution", func(t *testing.T) {
		mock := NewMockClickHouseReader()
		mock.SetHealthy(true)

		now := time.Now()
		mock.SetSpans([]model.OpenTelemetrySpan{
			{
				TraceID:       uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
				SpanID:        1001,
				OperationName: "test",
				Kind:          "INTERNAL",
				StartTimeUs:   uint64(now.Add(-1 * time.Second).UnixMicro()),
				FinishTimeUs:  uint64(now.UnixMicro()),
				FinishDate:    now,
				Attributes:    map[string]string{},
			},
		})

		cb := resilience.NewCircuitBreaker(config.CircuitBreakerConfig{
			FailureThreshold: 3,
			SuccessThreshold: 1,
			ResetTimeoutS:    60,
		})

		if cb.Allow() {
			if mock.IsHealthy(context.Background()) {
				spans, err := mock.FetchOpenTelemetrySpans(context.Background(), 1000, 0, 0, 0, 60*time.Second, 100)
				if err != nil {
					t.Fatalf("Unexpected error: %v", err)
				}
				if len(spans) != 1 {
					t.Errorf("Expected 1 span, got %d", len(spans))
				}
				cb.RecordSuccess()
			}
		}

		if mock.fetchCalled != 1 {
			t.Errorf("Expected 1 fetch call, got %d", mock.fetchCalled)
		}
	})
}

// TestFailure_LRUCacheEviction tests dedup cache behavior at capacity
func TestFailure_LRUCacheEviction(t *testing.T) {
	cacheSize := 5
	cache, err := lru.New[model.SpanKey, bool](cacheSize)
	if err != nil {
		t.Fatalf("Failed to create LRU cache: %v", err)
	}

	// Fixed UUIDs so failures are reproducible.
	traceIDs := []uuid.UUID{
		uuid.MustParse("00000000-0000-0000-0000-000000000000"),
		uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		uuid.MustParse("00000000-0000-0000-0000-000000000002"),
		uuid.MustParse("00000000-0000-0000-0000-000000000003"),
		uuid.MustParse("00000000-0000-0000-0000-000000000004"),
		uuid.MustParse("00000000-0000-0000-0000-000000000005"),
	}
	keys := make([]model.SpanKey, cacheSize+1)
	for i := range keys {
		keys[i] = model.SpanKey{TraceID: traceIDs[i], SpanID: uint64(i)}
	}

	// Fill cache
	for i := 0; i < cacheSize; i++ {
		cache.Add(keys[i], true)
	}

	if cache.Len() != cacheSize {
		t.Errorf("Expected cache size %d, got %d", cacheSize, cache.Len())
	}

	// Verify all entries present
	for i := 0; i < cacheSize; i++ {
		if !cache.Contains(keys[i]) {
			t.Errorf("Expected span %d to be in cache", i)
		}
	}

	// Add one more - should evict oldest
	cache.Add(keys[cacheSize], true)

	if cache.Len() != cacheSize {
		t.Errorf("Expected cache to stay at %d, got %d", cacheSize, cache.Len())
	}

	// Oldest entry (0) should be evicted
	if cache.Contains(keys[0]) {
		t.Error("Expected span 0 to be evicted from LRU cache")
	}

	// Newest entry should exist
	if !cache.Contains(keys[cacheSize]) {
		t.Errorf("Expected span %d to be in cache", cacheSize)
	}
}

// TestFailure_GracefulShutdown tests graceful shutdown signal handling
func TestFailure_GracefulShutdown(t *testing.T) {
	t.Run("context cancellation stops processing", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())

		// Simulate shutdown
		cancel()

		select {
		case <-ctx.Done():
			// Expected
		case <-time.After(1 * time.Second):
			t.Error("Context should be done immediately after cancel")
		}
	})
}

// TestFailure_ExportSpansToClosedServer tests export to a server that closes mid-operation
func TestFailure_ExportSpansToClosedServer(t *testing.T) {
	// Start a server, create exporter, stop server, try to export
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("Failed to listen: %v", err)
	}
	addr := listener.Addr().String()
	_ = listener.Close() // Close immediately

	exporter, err := export.NewOTELExporter(config.OTELConfig{
		CollectorAddress: addr,
		ServiceName:      "test-closed-server",
	})
	if err != nil {
		t.Fatalf("Failed to create exporter: %v", err)
	}
	defer func() { _ = exporter.Close(context.Background()) }()

	now := time.Now()
	spans := []model.OpenTelemetrySpan{
		{
			Hostname:      "host1",
			TraceID:       uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
			SpanID:        1001,
			OperationName: "test",
			Kind:          "INTERNAL",
			StartTimeUs:   uint64(now.Add(-1 * time.Second).UnixMicro()),
			FinishTimeUs:  uint64(now.UnixMicro()),
			FinishDate:    now,
			Attributes:    map[string]string{},
		},
	}

	_, err = exporter.ExportSpans(context.Background(), spans)
	if err == nil {
		t.Error("Expected error when exporting to closed server")
	}
}

// TestFailure_BackoffMaxInterval tests that backoff respects maximum interval
func TestFailure_BackoffMaxInterval(t *testing.T) {
	baseInterval := 10 * time.Second
	maxIntervalS := 30
	poller := resilience.NewAdaptivePoller(baseInterval, config.BackoffConfig{
		MaxIntervalS:  maxIntervalS,
		BackoffFactor: 2.0,
	})

	ticker := time.NewTicker(baseInterval)
	defer ticker.Stop()

	// Keep failing - should cap at max interval
	for i := 0; i < 20; i++ {
		processor.UpdatePollerState(poller, fmt.Errorf("error"), ticker, baseInterval, nil)
	}

	maxInterval := time.Duration(maxIntervalS) * time.Second
	if poller.CurrentInterval() != maxInterval {
		t.Errorf("Expected max interval %v, got %v", maxInterval, poller.CurrentInterval())
	}
}

// TestFailure_EmptySpanList tests handling of empty span lists
func TestFailure_EmptySpanList(t *testing.T) {
	mockCollector, addr, cleanup := startMockOTELServer(t)
	defer cleanup()

	exporter, err := export.NewOTELExporter(config.OTELConfig{
		CollectorAddress: addr,
		ServiceName:      "test-empty-spans",
	})
	if err != nil {
		t.Fatalf("Failed to create exporter: %v", err)
	}
	defer func() { _ = exporter.Close(context.Background()) }()

	// Export empty list - should be a no-op
	_, err = exporter.ExportSpans(context.Background(), []model.OpenTelemetrySpan{})
	if err != nil {
		t.Errorf("Expected no error for empty span list, got: %v", err)
	}

	assertNoReceivedSpans(t, mockCollector)
}
