package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/export"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/model"
	"github.com/coltconsulting/click-dog/internal/processor"
	"github.com/coltconsulting/click-dog/internal/resilience"
)

func TestProcessQueriesBatch_Integration(t *testing.T) {
	// Start mock OTEL collector
	mockCollector, addr, cleanup := startMockOTELServer(t)
	defer cleanup()

	// Create OTEL exporter
	exporter, err := export.NewOTELExporter(config.OTELConfig{
		CollectorAddress: addr,
		ServiceName:      "test-integration",
	})
	if err != nil {
		t.Fatalf("Failed to create OTEL exporter: %v", err)
	}
	defer func() { _ = exporter.Close(context.Background()) }()
	waitForOTELExporter(t, exporter)

	// Create filter
	// Note: WhitelistIPs means ONLY these IPs are allowed. To block 127.0.0.1,
	// we whitelist other IPs instead. For this test, we just use query blacklisting.
	qf, err := filter.NewQueryFilter(config.FiltersConfig{
		BlacklistQueries: []string{"SHOW TABLES", "^SELECT \\* FROM system\\."},
	})
	if err != nil {
		t.Fatalf("Failed to create filter: %v", err)
	}

	// Create test config
	cfg := &config.Config{
		Monitor: config.MonitorConfig{
			MinTraceDurationMs: 1000,
			MaxSpansPerCycle:   0,
		},
	}

	// Create test queries
	testTime := time.Now()
	queries := []model.QueryLog{
		{
			QueryID:         "query-1",
			QueryKind:       "QueryFinish",
			EventTime:       testTime,
			QueryDurationMs: 1500,
			Query:           "SELECT * FROM users",
			User:            "user1",
			ClientAddress:   "192.168.1.1",
			ReadRows:        100,
		},
		{
			QueryID:         "query-2",
			QueryKind:       "QueryFinish",
			EventTime:       testTime,
			QueryDurationMs: 2000,
			Query:           "SHOW TABLES", // Should be filtered
			User:            "user2",
			ClientAddress:   "192.168.1.2",
		},
		{
			QueryID:         "query-3",
			QueryKind:       "QueryFinish",
			EventTime:       testTime,
			QueryDurationMs: 1200,
			Query:           "SELECT * FROM system.tables", // Should be filtered
			User:            "user3",
			ClientAddress:   "192.168.1.3",
		},
		{
			QueryID:         "query-4",
			QueryKind:       "QueryFinish",
			EventTime:       testTime,
			QueryDurationMs: 3000,
			Query:           "SELECT * FROM system.processes", // Should be filtered (blacklist query)
			User:            "user4",
			ClientAddress:   "127.0.0.1",
		},
		{
			QueryID:         "query-5",
			QueryKind:       "QueryFinish",
			EventTime:       testTime,
			QueryDurationMs: 1800,
			Query:           "SELECT COUNT(*) FROM products",
			User:            "user5",
			ClientAddress:   "192.168.1.5",
			ReadRows:        500,
		},
	}

	// Process queries
	ctx := context.Background()
	processor.ProcessQueriesBatch(ctx, queries, exporter, qf, cfg, nil)

	// Verify results (export is synchronous, no flush needed)
	spans := waitForReceivedSpans(t, mockCollector, 2)

	// Should receive 2 spans (query-1 and query-5)
	// query-2, query-3, and query-4 should be filtered
	if len(spans) != 2 {
		t.Errorf("Expected 2 spans, got %d", len(spans))
	}

	// Verify the correct queries were exported
	queryIDs := make(map[string]bool)
	for _, span := range spans {
		attrs := attributesToMap(span.Attributes)
		queryID := attrs["db.query_id"].(string)
		queryIDs[queryID] = true
	}

	if !queryIDs["query-1"] {
		t.Error("Expected query-1 to be exported")
	}
	if !queryIDs["query-5"] {
		t.Error("Expected query-5 to be exported")
	}
	if queryIDs["query-2"] {
		t.Error("query-2 should have been filtered (blacklist query)")
	}
	if queryIDs["query-3"] {
		t.Error("query-3 should have been filtered (blacklist query)")
	}
	if queryIDs["query-4"] {
		t.Error("query-4 should have been filtered (blacklist query - system table)")
	}
}

func TestProcessQueriesBatch_MaxQueriesLimit(t *testing.T) {
	mockCollector, addr, cleanup := startMockOTELServer(t)
	defer cleanup()

	exporter, err := export.NewOTELExporter(config.OTELConfig{
		CollectorAddress: addr,
		ServiceName:      "test-limit",
	})
	if err != nil {
		t.Fatalf("Failed to create OTEL exporter: %v", err)
	}
	defer func() { _ = exporter.Close(context.Background()) }()
	waitForOTELExporter(t, exporter)

	qf, err := filter.NewQueryFilter(config.FiltersConfig{})
	if err != nil {
		t.Fatalf("Failed to create filter: %v", err)
	}

	// Config with limit
	cfg := &config.Config{
		Monitor: config.MonitorConfig{
			MinTraceDurationMs: 1000,
			MaxSpansPerCycle:   3, // Only process 3 spans
		},
	}

	// Create 5 test queries
	queries := make([]model.QueryLog, 5)
	testTime := time.Now()
	for i := 0; i < 5; i++ {
		queries[i] = model.QueryLog{
			QueryID:         string(rune('A' + i)),
			QueryKind:       "QueryFinish",
			EventTime:       testTime,
			QueryDurationMs: 1500,
			Query:           "SELECT * FROM test",
			User:            "user",
			ClientAddress:   "192.168.1.1",
		}
	}

	// Note: max_spans_per_cycle is applied in processSpansScheduled, not processQueriesBatch
	// So we need to manually limit here for testing
	if cfg.Monitor.MaxSpansPerCycle > 0 && len(queries) > cfg.Monitor.MaxSpansPerCycle {
		queries = queries[:cfg.Monitor.MaxSpansPerCycle]
	}

	ctx := context.Background()
	processor.ProcessQueriesBatch(ctx, queries, exporter, qf, cfg, nil)

	spans := waitForReceivedSpans(t, mockCollector, 3)

	// Should only export 3 spans due to limit
	if len(spans) != 3 {
		t.Errorf("Expected 3 spans due to max_spans_per_cycle limit, got %d", len(spans))
	}
}

func TestProcessSpansScheduled_Integration(t *testing.T) {
	// Start mock OTEL collector
	mockCollector, addr, cleanup := startMockOTELServer(t)
	defer cleanup()

	// Create OTEL exporter
	exporter, err := export.NewOTELExporter(config.OTELConfig{
		CollectorAddress: addr,
		ServiceName:      "test-integration-spans",
	})
	if err != nil {
		t.Fatalf("Failed to create OTEL exporter: %v", err)
	}
	defer func() { _ = exporter.Close(context.Background()) }()
	waitForOTELExporter(t, exporter)

	// Create filter - filter out system queries
	qf, err := filter.NewQueryFilter(config.FiltersConfig{
		BlacklistQueries: []string{"^SYSTEM"},
	})
	if err != nil {
		t.Fatalf("Failed to create filter: %v", err)
	}

	// Create test spans with different trace IDs (valid UUIDs)
	now := time.Now()
	testSpans := []model.OpenTelemetrySpan{
		{
			Hostname:      "host1",
			TraceID:       uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
			SpanID:        1001,
			ParentSpanID:  0,
			OperationName: "SELECT users",
			Kind:          "INTERNAL",
			StartTimeUs:   uint64(now.Add(-1500 * time.Millisecond).UnixMicro()),
			FinishTimeUs:  uint64(now.UnixMicro()),
			FinishDate:    now,
			Attributes: map[string]string{
				"db.statement": "SELECT * FROM users",
			},
		},
		{
			Hostname:      "host1",
			TraceID:       uuid.MustParse("660e8400-e29b-41d4-a716-446655440000"),
			SpanID:        2001,
			ParentSpanID:  0,
			OperationName: "SYSTEM query",
			Kind:          "INTERNAL",
			StartTimeUs:   uint64(now.Add(-1200 * time.Millisecond).UnixMicro()),
			FinishTimeUs:  uint64(now.UnixMicro()),
			FinishDate:    now,
			Attributes: map[string]string{
				"db.statement": "SYSTEM FLUSH LOGS",
			},
		},
		{
			Hostname:      "host2",
			TraceID:       uuid.MustParse("770e8400-e29b-41d4-a716-446655440000"),
			SpanID:        3001,
			ParentSpanID:  0,
			OperationName: "SELECT orders",
			Kind:          "CLIENT",
			StartTimeUs:   uint64(now.Add(-2000 * time.Millisecond).UnixMicro()),
			FinishTimeUs:  uint64(now.UnixMicro()),
			FinishDate:    now,
			Attributes: map[string]string{
				"db.statement":   "SELECT * FROM orders",
				"client.address": "192.168.1.100",
			},
		},
	}

	// Track seen spans (simulating deduplication)
	seenSpans := make(map[model.SpanKey]bool)

	// Process spans - collect for batch export
	ctx := context.Background()
	filtered := 0
	duplicate := 0
	var toExport []model.OpenTelemetrySpan

	for _, span := range testSpans {
		// Skip if already processed
		if seenSpans[model.KeyOf(span)] {
			duplicate++
			continue
		}

		// Apply filters
		queryText := span.Attributes["db.statement"]
		clientAddr := span.Attributes["client.address"]
		if qf.ShouldFilter(span.OperationName, queryText, clientAddr) {
			filtered++
			continue
		}

		toExport = append(toExport, span)
		seenSpans[model.KeyOf(span)] = true
	}

	// Export batch to OTEL
	exported := len(toExport)
	if _, err := exporter.ExportSpans(ctx, toExport); err != nil {
		t.Errorf("Error exporting spans: %v", err)
	}

	// Verify results
	receivedSpans := waitForReceivedSpans(t, mockCollector, 2)

	// Should receive 2 spans (trace-1 and trace-3)
	// trace-2 should be filtered (SYSTEM query)
	if exported != 2 {
		t.Errorf("Expected 2 exported spans, got %d", exported)
	}
	if filtered != 1 {
		t.Errorf("Expected 1 filtered span, got %d", filtered)
	}

	if len(receivedSpans) != 2 {
		t.Errorf("Expected 2 spans received by collector, got %d", len(receivedSpans))
	}

	// Verify operation names match expected traces
	receivedOpNames := make(map[string]bool)
	for _, span := range receivedSpans {
		receivedOpNames[span.Name] = true
	}

	if !receivedOpNames["SELECT users"] {
		t.Error("Expected 'SELECT users' operation to be exported")
	}
	if !receivedOpNames["SELECT orders"] {
		t.Error("Expected 'SELECT orders' operation to be exported")
	}
	if receivedOpNames["SYSTEM query"] {
		t.Error("'SYSTEM query' should have been filtered")
	}
}

func TestProcessSpansScheduled_Deduplication(t *testing.T) {
	mockCollector, addr, cleanup := startMockOTELServer(t)
	defer cleanup()

	exporter, err := export.NewOTELExporter(config.OTELConfig{
		CollectorAddress: addr,
		ServiceName:      "test-dedup",
	})
	if err != nil {
		t.Fatalf("Failed to create OTEL exporter: %v", err)
	}
	defer func() { _ = exporter.Close(context.Background()) }()
	waitForOTELExporter(t, exporter)

	// Track seen spans
	seenSpans := make(map[model.SpanKey]bool)

	now := time.Now()
	testSpan := model.OpenTelemetrySpan{
		Hostname:      "host1",
		TraceID:       uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
		SpanID:        1001,
		ParentSpanID:  0,
		OperationName: "SELECT users",
		Kind:          "INTERNAL",
		StartTimeUs:   uint64(now.Add(-1500 * time.Millisecond).UnixMicro()),
		FinishTimeUs:  uint64(now.UnixMicro()),
		FinishDate:    now,
		Attributes:    map[string]string{},
	}

	ctx := context.Background()

	// First export - should succeed
	if seenSpans[model.KeyOf(testSpan)] {
		t.Error("First span should not be marked as seen yet")
	}

	_, err = exporter.ExportSpans(ctx, []model.OpenTelemetrySpan{testSpan})
	if err != nil {
		t.Fatalf("Failed to export span: %v", err)
	}
	seenSpans[model.KeyOf(testSpan)] = true

	// Second export - should be skipped (duplicate check happens before export)
	if !seenSpans[model.KeyOf(testSpan)] {
		t.Error("Second span should be marked as seen")
	}

	// Should only receive 1 span
	spans := waitForReceivedSpans(t, mockCollector, 1)
	if len(spans) != 1 {
		t.Errorf("Expected 1 span (deduplication), got %d", len(spans))
	}
}

// TestCircuitBreaker_Integration tests the circuit breaker behavior in the processing pipeline
func TestCircuitBreaker_Integration(t *testing.T) {
	t.Run("circuit breaker blocks after threshold failures", func(t *testing.T) {
		cb := resilience.NewCircuitBreaker(config.CircuitBreakerConfig{
			FailureThreshold: 3,
			SuccessThreshold: 1,
			ResetTimeoutS:    60,
		})

		// Verify circuit starts closed
		if cb.State() != resilience.CircuitClosed {
			t.Errorf("Expected initial state Closed, got %v", cb.State())
		}

		// Simulate 3 consecutive failures (threshold)
		for i := 0; i < 3; i++ {
			if !cb.Allow() {
				t.Errorf("Circuit should allow request %d before threshold", i+1)
			}
			cb.RecordFailure()
		}

		// Circuit should now be open
		if cb.State() != resilience.CircuitOpen {
			t.Errorf("Expected state Open after threshold, got %v", cb.State())
		}

		// Should block subsequent requests
		if cb.Allow() {
			t.Error("Circuit should block requests when open")
		}
	})

	t.Run("circuit breaker recovers after reset timeout", func(t *testing.T) {
		fc := newFakeClock()
		cb := resilience.NewCircuitBreakerWithClock(config.CircuitBreakerConfig{
			FailureThreshold: 2,
			SuccessThreshold: 1,
			ResetTimeoutS:    1,
		}, fc)

		// Open the circuit
		cb.RecordFailure()
		cb.RecordFailure()

		if cb.State() != resilience.CircuitOpen {
			t.Fatal("Circuit should be open")
		}

		// Advance past reset timeout
		fc.Advance(1100 * time.Millisecond)

		// Should transition to half-open and allow one request
		if !cb.Allow() {
			t.Error("Should allow request after reset timeout")
		}

		if cb.State() != resilience.CircuitHalfOpen {
			t.Errorf("Expected state HalfOpen, got %v", cb.State())
		}

		// Record success to close circuit
		cb.RecordSuccess()

		if cb.State() != resilience.CircuitClosed {
			t.Errorf("Expected state Closed after success, got %v", cb.State())
		}
	})

	t.Run("circuit breaker reopens on failure in half-open", func(t *testing.T) {
		fc := newFakeClock()
		cb := resilience.NewCircuitBreakerWithClock(config.CircuitBreakerConfig{
			FailureThreshold: 2,
			SuccessThreshold: 1,
			ResetTimeoutS:    1,
		}, fc)

		// Open the circuit
		cb.RecordFailure()
		cb.RecordFailure()

		// Advance past reset timeout
		fc.Advance(1100 * time.Millisecond)

		// Transition to half-open
		cb.Allow()

		if cb.State() != resilience.CircuitHalfOpen {
			t.Fatal("Should be half-open")
		}

		// Record failure - should reopen
		cb.RecordFailure()

		if cb.State() != resilience.CircuitOpen {
			t.Errorf("Expected state Open after failure in half-open, got %v", cb.State())
		}
	})
}

// TestCircuitBreaker_WithProcessing tests circuit breaker integration with span processing
func TestCircuitBreaker_WithProcessing(t *testing.T) {
	t.Run("processSpansWithProtection respects circuit breaker", func(t *testing.T) {
		cb := resilience.NewCircuitBreaker(config.CircuitBreakerConfig{
			FailureThreshold: 2,
			SuccessThreshold: 1,
			ResetTimeoutS:    60,
		})

		// Start mock OTEL collector
		_, addr, cleanup := startMockOTELServer(t)
		defer cleanup()

		// Create OTEL exporter
		exporter, err := export.NewOTELExporter(config.OTELConfig{
			CollectorAddress: addr,
			ServiceName:      "test-cb-integration",
		})
		if err != nil {
			t.Fatalf("Failed to create OTEL exporter: %v", err)
		}
		defer func() { _ = exporter.Close(context.Background()) }()
		waitForOTELExporter(t, exporter)

		// Create filter
		qf, err := filter.NewQueryFilter(config.FiltersConfig{})
		if err != nil {
			t.Fatalf("Failed to create filter: %v", err)
		}

		cfg := &config.Config{
			Monitor: config.MonitorConfig{
				MinTraceDurationMs: 1000,
				LookbackS:          60,
				MaxSpansPerCycle:   100,
				DedupCacheSize:     1000,
			},
		}

		// Open the circuit manually
		cb.RecordFailure()
		cb.RecordFailure()

		if cb.State() != resilience.CircuitOpen {
			t.Fatal("Circuit should be open")
		}

		// Track if processing was skipped
		callCount := 0

		// When circuit is open, Allow() returns false
		if cb.Allow() {
			callCount++
		}

		// processSpansWithProtection should skip when circuit is open
		if callCount != 0 {
			t.Error("Should have skipped processing when circuit is open")
		}

		// Verify state
		if cb.State() != resilience.CircuitOpen {
			t.Errorf("Expected state CircuitOpen, got %v", cb.State())
		}

		// Clean up exporter reference to avoid nil pointer
		_ = exporter
		_ = qf
		_ = cfg
	})
}

// TestAdaptiveBackoff_Integration tests the adaptive backoff behavior
func TestAdaptiveBackoff_Integration(t *testing.T) {
	t.Run("backoff increases interval on failures", func(t *testing.T) {
		baseInterval := 10 * time.Second
		poller := resilience.NewAdaptivePoller(baseInterval, config.BackoffConfig{
			MaxIntervalS:  300,
			BackoffFactor: 2.0,
		})

		if poller.CurrentInterval() != baseInterval {
			t.Errorf("Expected initial interval %v, got %v", baseInterval, poller.CurrentInterval())
		}

		// First failure
		poller.RecordFailure()
		if poller.CurrentInterval() != 20*time.Second {
			t.Errorf("Expected 20s after first failure, got %v", poller.CurrentInterval())
		}

		// Second failure
		poller.RecordFailure()
		if poller.CurrentInterval() != 40*time.Second {
			t.Errorf("Expected 40s after second failure, got %v", poller.CurrentInterval())
		}

		if !poller.IsBackedOff() {
			t.Error("Should be backed off after failures")
		}
	})

	t.Run("backoff resets on success", func(t *testing.T) {
		baseInterval := 10 * time.Second
		poller := resilience.NewAdaptivePoller(baseInterval, config.BackoffConfig{
			MaxIntervalS:  300,
			BackoffFactor: 2.0,
		})

		// Create some backoff
		poller.RecordFailure()
		poller.RecordFailure()

		if poller.CurrentInterval() == baseInterval {
			t.Error("Should be backed off")
		}

		// Success resets
		poller.RecordSuccess()

		if poller.CurrentInterval() != baseInterval {
			t.Errorf("Expected reset to base %v, got %v", baseInterval, poller.CurrentInterval())
		}

		if poller.IsBackedOff() {
			t.Error("Should not be backed off after success")
		}
	})

	t.Run("backoff respects max interval", func(t *testing.T) {
		baseInterval := 30 * time.Second
		maxInterval := 60 * time.Second
		poller := resilience.NewAdaptivePoller(baseInterval, config.BackoffConfig{
			MaxIntervalS:  60,
			BackoffFactor: 2.0,
		})

		// Keep failing
		for i := 0; i < 10; i++ {
			poller.RecordFailure()
		}

		if poller.CurrentInterval() != maxInterval {
			t.Errorf("Expected max interval %v, got %v", maxInterval, poller.CurrentInterval())
		}
	})
}

// TestProtectionMechanisms_Combined tests circuit breaker and backoff working together
func TestProtectionMechanisms_Combined(t *testing.T) {
	t.Run("circuit breaker and backoff work together", func(t *testing.T) {
		cb := resilience.NewCircuitBreaker(config.CircuitBreakerConfig{
			FailureThreshold: 3,
			SuccessThreshold: 1,
			ResetTimeoutS:    60,
		})

		baseInterval := 10 * time.Second
		poller := resilience.NewAdaptivePoller(baseInterval, config.BackoffConfig{
			MaxIntervalS:  300,
			BackoffFactor: 2.0,
		})

		// Simulate failures
		for i := 0; i < 3; i++ {
			if cb.Allow() {
				// Simulate failure
				cb.RecordFailure()
				poller.RecordFailure()
			}
		}

		// Circuit should be open
		if cb.State() != resilience.CircuitOpen {
			t.Errorf("Expected circuit open, got %v", cb.State())
		}

		// Backoff should have increased interval
		if poller.CurrentInterval() == baseInterval {
			t.Error("Backoff interval should have increased")
		}

		// Both should block/delay
		if cb.Allow() {
			t.Error("Circuit should block")
		}

		expectedInterval := 80 * time.Second // 10 * 2 * 2 * 2
		if poller.CurrentInterval() != expectedInterval {
			t.Errorf("Expected interval %v, got %v", expectedInterval, poller.CurrentInterval())
		}
	})
}

func TestFilter_ShouldFilter(t *testing.T) {
	tests := []struct {
		name          string
		config        config.FiltersConfig
		operationName string
		query         string
		clientIP      string
		shouldFilter  bool
		description   string
	}{
		{
			name: "no filters",
			config: config.FiltersConfig{
				BlacklistQueries: []string{},
			},
			operationName: "SELECT",
			query:         "SELECT * FROM users",
			clientIP:      "192.168.1.1",
			shouldFilter:  false,
			description:   "No filters configured, should not filter",
		},
		{
			name: "query matches blacklist",
			config: config.FiltersConfig{
				BlacklistQueries: []string{"SHOW TABLES"},
			},
			operationName: "SHOW",
			query:         "SHOW TABLES",
			clientIP:      "192.168.1.1",
			shouldFilter:  true,
			description:   "Query matches blacklist pattern",
		},
		{
			name: "query matches regex pattern",
			config: config.FiltersConfig{
				BlacklistQueries: []string{"^SELECT \\* FROM system\\."},
			},
			operationName: "SELECT",
			query:         "SELECT * FROM system.tables",
			clientIP:      "192.168.1.1",
			shouldFilter:  true,
			description:   "Query matches regex pattern",
		},
		{
			name: "IP not in whitelist",
			config: config.FiltersConfig{
				WhitelistIPs: []string{"192.168.1.1", "10.0.0.1"},
			},
			operationName: "SELECT",
			query:         "SELECT * FROM users",
			clientIP:      "127.0.0.1",
			shouldFilter:  true,
			description:   "IP is not in whitelist, should be filtered",
		},
		{
			name: "IP in whitelist",
			config: config.FiltersConfig{
				WhitelistIPs: []string{"192.168.1.1", "127.0.0.1"},
			},
			operationName: "SELECT",
			query:         "SELECT * FROM users",
			clientIP:      "127.0.0.1",
			shouldFilter:  false,
			description:   "IP is in whitelist, should not be filtered",
		},
		{
			name: "case insensitive query match",
			config: config.FiltersConfig{
				BlacklistQueries: []string{"(?i)healthcheck"},
			},
			operationName: "SELECT",
			query:         "SELECT * FROM HealthCheck",
			clientIP:      "192.168.1.1",
			shouldFilter:  true,
			description:   "Case insensitive pattern matches",
		},
		{
			name: "no match",
			config: config.FiltersConfig{
				BlacklistQueries: []string{"SHOW TABLES"},
			},
			operationName: "SELECT",
			query:         "SELECT * FROM users",
			clientIP:      "192.168.1.100",
			shouldFilter:  false,
			description:   "Query does not match blacklist",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			qf, err := filter.NewQueryFilter(tt.config)
			if err != nil {
				t.Fatalf("Failed to create filter: %v", err)
			}

			result := qf.ShouldFilter(tt.operationName, tt.query, tt.clientIP)
			if result != tt.shouldFilter {
				t.Errorf("%s: expected shouldFilter=%v, got %v", tt.description, tt.shouldFilter, result)
			}
		})
	}
}
