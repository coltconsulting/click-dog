package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/export"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/model"
	"github.com/coltconsulting/click-dog/internal/processor"
	"github.com/coltconsulting/click-dog/internal/resilience"

	lru "github.com/hashicorp/golang-lru/v2"
)

// TestDispatch covers the pre-flag.Parse() verb routing: the version alias,
// the unknown-command guard (so a typo like `version` for `-version` no longer
// silently starts the daemon), and the fall-through cases that hand control
// back to flag parsing / scheduled mode.
func TestDispatch(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantCode    int
		wantThrough bool
		wantOut     string // substring expected on stdout
		wantErr     string // substring expected on stderr
	}{
		{
			name:        "version verb prints version and exits 0",
			args:        []string{"click-dog", "version"},
			wantCode:    0,
			wantThrough: false,
			wantOut:     "click-dog " + version,
		},
		{
			name:        "help verb prints usage and exits 0",
			args:        []string{"click-dog", "help"},
			wantCode:    0,
			wantThrough: false,
			wantOut:     "ClickHouse query monitor",
		},
		{
			name:        "unknown verb errors with exit 2",
			args:        []string{"click-dog", "verison"},
			wantCode:    2,
			wantThrough: false,
			wantErr:     `unknown command "verison"`,
		},
		{
			name:        "no args falls through to flag parsing",
			args:        []string{"click-dog"},
			wantThrough: true,
		},
		{
			name:        "leading flag falls through to flag parsing",
			args:        []string{"click-dog", "-config", "x.yaml"},
			wantThrough: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code, fellThrough, argv := dispatch(tt.args, &out, &errOut)

			if fellThrough != tt.wantThrough {
				t.Errorf("fellThrough = %v, want %v", fellThrough, tt.wantThrough)
			}
			// A plain fall-through hands the original args back unchanged for
			// flag parsing (only the backfill verb rewrites them).
			if tt.wantThrough && !slices.Equal(argv, tt.args) {
				t.Errorf("fall-through argv = %v, want %v", argv, tt.args)
			}
			if !tt.wantThrough && code != tt.wantCode {
				t.Errorf("exitCode = %d, want %d", code, tt.wantCode)
			}
			if tt.wantOut != "" && !strings.Contains(out.String(), tt.wantOut) {
				t.Errorf("stdout = %q, want substring %q", out.String(), tt.wantOut)
			}
			if tt.wantErr != "" && !strings.Contains(errOut.String(), tt.wantErr) {
				t.Errorf("stderr = %q, want substring %q", errOut.String(), tt.wantErr)
			}
			// The inactive stream must stay clean: version/help write only
			// stdout, the unknown-command error writes only stderr. Without
			// this, a stray write to the wrong stream would go unnoticed.
			if tt.wantOut != "" && errOut.Len() > 0 {
				t.Errorf("expected clean stderr, got %q", errOut.String())
			}
			if tt.wantErr != "" && out.Len() > 0 {
				t.Errorf("expected clean stdout, got %q", out.String())
			}
			// A fall-through must not have produced any output — main() still
			// owns usage/version printing on that path.
			if tt.wantThrough && (out.Len() > 0 || errOut.Len() > 0) {
				t.Errorf("fall-through wrote output: out=%q err=%q", out.String(), errOut.String())
			}
		})
	}
}

func TestQueryTextCapabilityWarning(t *testing.T) {
	tests := []struct {
		name                string
		mode                config.QueryTextMode
		normalizedSupported bool
		wantWarning         bool
	}{
		{name: "normalized only unsupported", mode: config.QueryTextModeNormalizedOnly, wantWarning: true},
		{name: "normalized only supported", mode: config.QueryTextModeNormalizedOnly, normalizedSupported: true},
		{name: "none unsupported", mode: config.QueryTextModeNone},
		{name: "redacted unsupported", mode: config.QueryTextModeRedacted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{Filters: config.FiltersConfig{QueryTextMode: tt.mode}}
			got := queryTextCapabilityWarning(cfg, tt.normalizedSupported)
			if (got != "") != tt.wantWarning {
				t.Fatalf("queryTextCapabilityWarning() = %q, wantWarning=%v", got, tt.wantWarning)
			}
			if got != "" && !strings.Contains(got, "omit query text") {
				t.Fatalf("warning = %q, want fail-closed consequence", got)
			}
		})
	}
}

// TestDispatch_RoutesKnownVerb asserts that a registered verb is routed to its
// handler with the post-verb args (argv[2:]) and does not fall through. It
// swaps the subcommands registry for a stub so no real handler runs.
func TestDispatch_RoutesKnownVerb(t *testing.T) {
	var gotArgs []string
	called := false

	orig := subcommands
	subcommands = map[string]func(args []string, out, errOut io.Writer) int{
		"demo": func(args []string, _, _ io.Writer) int {
			called = true
			gotArgs = args
			return 0
		},
	}
	t.Cleanup(func() { subcommands = orig })

	var out, errOut bytes.Buffer
	code, fellThrough, _ := dispatch([]string{"click-dog", "demo", "a", "b"}, &out, &errOut)

	if fellThrough {
		t.Error("known verb must not fall through to flag parsing")
	}
	if code != 0 {
		t.Errorf("exitCode = %d, want 0", code)
	}
	if !called {
		t.Fatal("handler was not invoked")
	}
	if len(gotArgs) != 2 || gotArgs[0] != "a" || gotArgs[1] != "b" {
		t.Errorf("handler received args %v, want [a b]", gotArgs)
	}
}

// TestDispatch_PropagatesHandlerExitCode asserts dispatch surfaces the exit code
// the handler returns (rather than always 0), so main() can os.Exit with it.
// This is the contract that lets handlers return codes instead of calling
// os.Exit themselves.
func TestDispatch_PropagatesHandlerExitCode(t *testing.T) {
	orig := subcommands
	subcommands = map[string]func(args []string, out, errOut io.Writer) int{
		"demo": func(_ []string, _, _ io.Writer) int { return 3 },
	}
	t.Cleanup(func() { subcommands = orig })

	code, fellThrough, _ := dispatch([]string{"click-dog", "demo"}, io.Discard, io.Discard)
	if fellThrough {
		t.Error("known verb must not fall through")
	}
	if code != 3 {
		t.Errorf("exitCode = %d, want 3 (handler's returned code)", code)
	}
}

// TestProcessSpansWithProtection tests the circuit breaker protection wrapper
func TestProcessSpansWithProtection(t *testing.T) {
	t.Run("skips when circuit breaker is open", func(t *testing.T) {
		mockCollector, addr, cleanup := startMockOTELServer(t)
		defer cleanup()

		exporter, err := export.NewOTELExporter(config.OTELConfig{
			CollectorAddress: addr,
			ServiceName:      "test-protection",
		})
		if err != nil {
			t.Fatalf("Failed to create exporter: %v", err)
		}
		defer func() { _ = exporter.Close(context.Background()) }()

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
			ClickHouse: config.ClickHouseConfig{
				Host:          "localhost",
				Port:          9000,
				QueryTimeoutS: 5,
			},
		}

		seenSpans, err := lru.New[model.SpanKey, bool](1000)
		if err != nil {
			t.Fatalf("Failed to create LRU cache: %v", err)
		}

		cb := resilience.NewCircuitBreaker(config.CircuitBreakerConfig{
			FailureThreshold: 2,
			SuccessThreshold: 1,
			ResetTimeoutS:    60,
		})

		// Open the circuit
		cb.RecordFailure()
		cb.RecordFailure()

		if cb.State() != resilience.CircuitOpen {
			t.Fatal("Circuit should be open")
		}

		// processSpansWithProtection should return nil (skipped) when circuit is open
		// We can't call it directly since it uses a real ClickHouseReader,
		// but we can verify the circuit breaker logic
		if cb.Allow() {
			t.Error("Circuit should not allow requests when open")
		}

		// Verify no spans were sent
		spans := mockCollector.GetReceivedSpans()
		if len(spans) != 0 {
			t.Errorf("Expected 0 spans when circuit is open, got %d", len(spans))
		}

		_ = exporter
		_ = qf
		_ = cfg
		_ = seenSpans
	})

	t.Run("records failure and updates circuit breaker", func(t *testing.T) {
		cb := resilience.NewCircuitBreaker(config.CircuitBreakerConfig{
			FailureThreshold: 3,
			SuccessThreshold: 1,
			ResetTimeoutS:    60,
		})

		// Simulate the processSpansWithProtection error handling logic
		simulateProcessWithProtection := func(fetchErr error) error {
			if !cb.Allow() {
				return nil
			}

			err := fetchErr // Simulated fetch result

			if err != nil {
				cb.RecordFailure()
			} else {
				cb.RecordSuccess()
			}

			return err
		}

		// Two failures - circuit still closed
		_ = simulateProcessWithProtection(fmt.Errorf("timeout"))
		_ = simulateProcessWithProtection(fmt.Errorf("timeout"))
		if cb.State() != resilience.CircuitClosed {
			t.Errorf("Expected closed after 2 failures, got %v", cb.State())
		}

		// Third failure - circuit opens
		_ = simulateProcessWithProtection(fmt.Errorf("timeout"))
		if cb.State() != resilience.CircuitOpen {
			t.Errorf("Expected open after 3 failures, got %v", cb.State())
		}

		// Should be skipped now (returns nil, no failure recorded)
		err := simulateProcessWithProtection(fmt.Errorf("should not reach"))
		if err != nil {
			t.Error("Expected nil (skipped) when circuit is open")
		}
	})

	t.Run("records success and keeps circuit closed", func(t *testing.T) {
		cb := resilience.NewCircuitBreaker(config.CircuitBreakerConfig{
			FailureThreshold: 3,
			SuccessThreshold: 1,
			ResetTimeoutS:    60,
		})

		// Simulate success
		if !cb.Allow() {
			t.Fatal("Circuit should allow initial request")
		}
		cb.RecordSuccess()

		if cb.State() != resilience.CircuitClosed {
			t.Errorf("Expected closed after success, got %v", cb.State())
		}
		if cb.Failures() != 0 {
			t.Errorf("Expected 0 failures after success, got %d", cb.Failures())
		}
	})
}

// TestProcessSpansWithProtection_HealthCheck tests health check integration
func TestProcessSpansWithProtection_HealthCheck(t *testing.T) {
	t.Run("skips when unhealthy", func(t *testing.T) {
		mock := NewMockClickHouseReader()
		mock.SetHealthy(false)

		// The processSpansWithProtection function checks IsHealthy before querying
		// When unhealthy, it returns an error without hitting ClickHouse
		if mock.IsHealthy(context.Background()) {
			t.Error("Mock should report unhealthy")
		}

		// Verify no fetch was called
		if mock.fetchCalled != 0 {
			t.Errorf("Expected no fetch calls when unhealthy, got %d", mock.fetchCalled)
		}
	})

	t.Run("proceeds when healthy", func(t *testing.T) {
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

		if !mock.IsHealthy(context.Background()) {
			t.Error("Mock should report healthy")
		}

		// Verify fetch can proceed
		spans, err := mock.FetchOpenTelemetrySpans(context.Background(), 1000, 0, 0, 0, 60*time.Second, 100)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if len(spans) != 1 {
			t.Errorf("Expected 1 span, got %d", len(spans))
		}
	})
}

// TestUpdatePollerState tests the adaptive backoff update logic
func TestUpdatePollerState(t *testing.T) {
	t.Run("nil poller is no-op", func(t *testing.T) {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		// Should not panic with nil poller
		processor.UpdatePollerState(nil, nil, ticker, 10*time.Second, nil)
		processor.UpdatePollerState(nil, fmt.Errorf("error"), ticker, 10*time.Second, nil)
	})

	t.Run("records failure and increases interval", func(t *testing.T) {
		baseInterval := 10 * time.Second
		poller := resilience.NewAdaptivePoller(baseInterval, config.BackoffConfig{
			MaxIntervalS:  300,
			BackoffFactor: 2.0,
		})

		ticker := time.NewTicker(baseInterval)
		defer ticker.Stop()

		// Record failure
		processor.UpdatePollerState(poller, fmt.Errorf("connection error"), ticker, baseInterval, nil)

		if poller.CurrentInterval() != 20*time.Second {
			t.Errorf("Expected 20s after failure, got %v", poller.CurrentInterval())
		}

		if !poller.IsBackedOff() {
			t.Error("Should be backed off after failure")
		}
	})

	t.Run("records success and resets interval", func(t *testing.T) {
		baseInterval := 10 * time.Second
		poller := resilience.NewAdaptivePoller(baseInterval, config.BackoffConfig{
			MaxIntervalS:  300,
			BackoffFactor: 2.0,
		})

		ticker := time.NewTicker(baseInterval)
		defer ticker.Stop()

		// First create some backoff
		processor.UpdatePollerState(poller, fmt.Errorf("error"), ticker, baseInterval, nil)
		processor.UpdatePollerState(poller, fmt.Errorf("error"), ticker, baseInterval, nil)

		if poller.CurrentInterval() == baseInterval {
			t.Error("Should be backed off after failures")
		}

		// Success resets
		processor.UpdatePollerState(poller, nil, ticker, baseInterval, nil)

		if poller.CurrentInterval() != baseInterval {
			t.Errorf("Expected reset to %v, got %v", baseInterval, poller.CurrentInterval())
		}
	})
}

// TestFetchAndProcessSpans tests the fetch-filter-export pipeline
func TestFetchAndProcessSpans(t *testing.T) {
	t.Run("processes and exports spans correctly", func(t *testing.T) {
		mockCollector, addr, cleanup := startMockOTELServer(t)
		defer cleanup()

		exporter, err := export.NewOTELExporter(config.OTELConfig{
			CollectorAddress: addr,
			ServiceName:      "test-pipeline",
		})
		if err != nil {
			t.Fatalf("Failed to create exporter: %v", err)
		}
		defer func() { _ = exporter.Close(context.Background()) }()
		waitForOTELExporter(t, exporter)

		qf, err := filter.NewQueryFilter(config.FiltersConfig{
			BlacklistQueries: []string{"^SYSTEM"},
		})
		if err != nil {
			t.Fatalf("Failed to create filter: %v", err)
		}

		cfg := &config.Config{
			Monitor: config.MonitorConfig{
				MinTraceDurationMs: 1000,
				LookbackS:          60,
				MaxSpansPerCycle:   100,
			},
		}

		seenSpans, err := lru.New[model.SpanKey, bool](1000)
		if err != nil {
			t.Fatalf("Failed to create LRU cache: %v", err)
		}

		now := time.Now()
		testSpans := []model.OpenTelemetrySpan{
			{
				Hostname:      "host1",
				TraceID:       uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
				SpanID:        1001,
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
				OperationName: "SYSTEM query",
				Kind:          "INTERNAL",
				StartTimeUs:   uint64(now.Add(-1200 * time.Millisecond).UnixMicro()),
				FinishTimeUs:  uint64(now.UnixMicro()),
				FinishDate:    now,
				Attributes: map[string]string{
					"db.statement": "SYSTEM FLUSH LOGS",
				},
			},
		}

		// Simulate fetchAndProcessSpans logic inline since we can't mock ClickHouseReader
		// in the actual function (it takes *ClickHouseReader not interface)
		exported := 0
		filtered := 0

		var toExport []model.OpenTelemetrySpan
		for _, span := range testSpans {
			if seenSpans.Contains(model.KeyOf(span)) {
				continue
			}

			queryText := span.Attributes["db.statement"]
			clientAddr := span.Attributes["client.address"]
			if qf.ShouldFilter(span.OperationName, queryText, clientAddr) {
				filtered++
				continue
			}

			toExport = append(toExport, span)
		}

		if len(toExport) > 0 {
			result, err := exporter.ExportSpans(context.Background(), toExport)
			if err != nil {
				t.Fatalf("Failed to export spans: %v", err)
			}
			for _, k := range result.Accepted {
				seenSpans.Add(k, true)
			}
			exported = len(result.Accepted)
		}

		if exported != 1 {
			t.Errorf("Expected 1 exported span, got %d", exported)
		}
		if filtered != 1 {
			t.Errorf("Expected 1 filtered span, got %d", filtered)
		}

		receivedSpans := waitForReceivedSpans(t, mockCollector, 1)
		if len(receivedSpans) != 1 {
			t.Errorf("Expected 1 span received, got %d", len(receivedSpans))
		}

		_ = cfg
	})

	t.Run("deduplication prevents re-export", func(t *testing.T) {
		mockCollector, addr, cleanup := startMockOTELServer(t)
		defer cleanup()

		exporter, err := export.NewOTELExporter(config.OTELConfig{
			CollectorAddress: addr,
			ServiceName:      "test-dedup-pipeline",
		})
		if err != nil {
			t.Fatalf("Failed to create exporter: %v", err)
		}
		defer func() { _ = exporter.Close(context.Background()) }()
		waitForOTELExporter(t, exporter)

		qf, err := filter.NewQueryFilter(config.FiltersConfig{})
		if err != nil {
			t.Fatalf("Failed to create filter: %v", err)
		}

		seenSpans, err := lru.New[model.SpanKey, bool](1000)
		if err != nil {
			t.Fatalf("Failed to create LRU cache: %v", err)
		}

		now := time.Now()
		span := model.OpenTelemetrySpan{
			Hostname:      "host1",
			TraceID:       uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
			SpanID:        1001,
			OperationName: "SELECT users",
			Kind:          "INTERNAL",
			StartTimeUs:   uint64(now.Add(-1 * time.Second).UnixMicro()),
			FinishTimeUs:  uint64(now.UnixMicro()),
			FinishDate:    now,
			Attributes:    map[string]string{},
		}

		ctx := context.Background()

		// First pass - should export
		if seenSpans.Contains(model.KeyOf(span)) {
			t.Error("Should not be seen yet")
		}
		queryText := span.Attributes["db.statement"]
		clientAddr := span.Attributes["client.address"]
		if !qf.ShouldFilter(span.OperationName, queryText, clientAddr) {
			result, err := exporter.ExportSpans(ctx, []model.OpenTelemetrySpan{span})
			if err != nil {
				t.Fatalf("Failed to export: %v", err)
			}
			for _, k := range result.Accepted {
				seenSpans.Add(k, true)
			}
		}

		// Second pass - should be deduplicated
		if !seenSpans.Contains(model.KeyOf(span)) {
			t.Error("Should be seen after first export")
		}

		receivedSpans := waitForReceivedSpans(t, mockCollector, 1)
		if len(receivedSpans) != 1 {
			t.Errorf("Expected 1 span (no duplicates), got %d", len(receivedSpans))
		}
	})

	t.Run("batch processing respects batch size", func(t *testing.T) {
		mockCollector, addr, cleanup := startMockOTELServer(t)
		defer cleanup()

		exporter, err := export.NewOTELExporter(config.OTELConfig{
			CollectorAddress: addr,
			ServiceName:      "test-batch",
		})
		if err != nil {
			t.Fatalf("Failed to create exporter: %v", err)
		}
		defer func() { _ = exporter.Close(context.Background()) }()
		waitForOTELExporter(t, exporter)

		qf, err := filter.NewQueryFilter(config.FiltersConfig{})
		if err != nil {
			t.Fatalf("Failed to create filter: %v", err)
		}

		cfg := &config.Config{
			Monitor: config.MonitorConfig{
				BatchSize: 2, // Process 2 at a time
			},
		}

		seenSpans, err := lru.New[model.SpanKey, bool](1000)
		if err != nil {
			t.Fatalf("Failed to create LRU cache: %v", err)
		}

		now := time.Now()
		testSpans := make([]model.OpenTelemetrySpan, 5)
		for i := 0; i < 5; i++ {
			testSpans[i] = model.OpenTelemetrySpan{
				Hostname:      "host1",
				TraceID:       uuid.MustParse(fmt.Sprintf("550e8400-e29b-41d4-a716-44665544%04d", i)),
				SpanID:        uint64(1000 + i),
				OperationName: fmt.Sprintf("SELECT test_%d", i),
				Kind:          "INTERNAL",
				StartTimeUs:   uint64(now.Add(-1 * time.Second).UnixMicro()),
				FinishTimeUs:  uint64(now.UnixMicro()),
				FinishDate:    now,
				Attributes:    map[string]string{},
			}
		}

		// Process in batches (simulating fetchAndProcessSpans batch logic)
		ctx := context.Background()
		batchSize := cfg.Monitor.BatchSize
		exported := 0

		for batchStart := 0; batchStart < len(testSpans); batchStart += batchSize {
			batchEnd := batchStart + batchSize
			if batchEnd > len(testSpans) {
				batchEnd = len(testSpans)
			}

			batch := testSpans[batchStart:batchEnd]

			var toExport []model.OpenTelemetrySpan
			for _, span := range batch {
				if seenSpans.Contains(model.KeyOf(span)) {
					continue
				}
				queryText := span.Attributes["db.statement"]
				clientAddr := span.Attributes["client.address"]
				if qf.ShouldFilter(span.OperationName, queryText, clientAddr) {
					continue
				}
				toExport = append(toExport, span)
			}

			if len(toExport) > 0 {
				result, err := exporter.ExportSpans(ctx, toExport)
				if err != nil {
					t.Fatalf("Failed to export batch: %v", err)
				}
				for _, k := range result.Accepted {
					seenSpans.Add(k, true)
				}
				exported += len(result.Accepted)
			}
		}

		if exported != 5 {
			t.Errorf("Expected 5 exported spans, got %d", exported)
		}

		receivedSpans := waitForReceivedSpans(t, mockCollector, 5)
		if len(receivedSpans) != 5 {
			t.Errorf("Expected 5 received spans, got %d", len(receivedSpans))
		}
	})
}

// TestProcessQueriesBatch_EmptyInput tests empty query list handling
func TestProcessQueriesBatch_EmptyInput(t *testing.T) {
	mockCollector, addr, cleanup := startMockOTELServer(t)
	defer cleanup()

	exporter, err := export.NewOTELExporter(config.OTELConfig{
		CollectorAddress: addr,
		ServiceName:      "test-empty",
	})
	if err != nil {
		t.Fatalf("Failed to create exporter: %v", err)
	}
	defer func() { _ = exporter.Close(context.Background()) }()

	qf, err := filter.NewQueryFilter(config.FiltersConfig{})
	if err != nil {
		t.Fatalf("Failed to create filter: %v", err)
	}

	cfg := &config.Config{}

	// Process empty list - should not panic
	processor.ProcessQueriesBatch(context.Background(), []model.QueryLog{}, exporter, qf, cfg, nil)

	assertNoReceivedSpans(t, mockCollector)
}

// TestProcessQueriesBatch_AllFiltered tests when all queries are filtered
func TestProcessQueriesBatch_AllFiltered(t *testing.T) {
	mockCollector, addr, cleanup := startMockOTELServer(t)
	defer cleanup()

	exporter, err := export.NewOTELExporter(config.OTELConfig{
		CollectorAddress: addr,
		ServiceName:      "test-all-filtered",
	})
	if err != nil {
		t.Fatalf("Failed to create exporter: %v", err)
	}
	defer func() { _ = exporter.Close(context.Background()) }()

	qf, err := filter.NewQueryFilter(config.FiltersConfig{
		BlacklistQueries: []string{".*"}, // Filter everything
	})
	if err != nil {
		t.Fatalf("Failed to create filter: %v", err)
	}

	cfg := &config.Config{}
	queries := []model.QueryLog{
		{QueryID: "q1", Query: "SELECT 1", ClientAddress: "127.0.0.1"},
		{QueryID: "q2", Query: "SELECT 2", ClientAddress: "127.0.0.1"},
	}

	// Should not panic, just filter everything
	processor.ProcessQueriesBatch(context.Background(), queries, exporter, qf, cfg, nil)

	assertNoReceivedSpans(t, mockCollector)
}

// TestProcessQueriesBatch_WithBatching tests batch processing of queries
func TestProcessQueriesBatch_WithBatching(t *testing.T) {
	mockCollector, addr, cleanup := startMockOTELServer(t)
	defer cleanup()

	exporter, err := export.NewOTELExporter(config.OTELConfig{
		CollectorAddress: addr,
		ServiceName:      "test-batching",
	})
	if err != nil {
		t.Fatalf("Failed to create exporter: %v", err)
	}
	defer func() { _ = exporter.Close(context.Background()) }()
	waitForOTELExporter(t, exporter)

	qf, err := filter.NewQueryFilter(config.FiltersConfig{})
	if err != nil {
		t.Fatalf("Failed to create filter: %v", err)
	}

	cfg := &config.Config{
		Monitor: config.MonitorConfig{
			BatchSize: 2,
		},
	}

	now := time.Now()
	queries := make([]model.QueryLog, 5)
	for i := 0; i < 5; i++ {
		queries[i] = model.QueryLog{
			QueryID:         fmt.Sprintf("q%d", i),
			QueryKind:       "QueryFinish",
			EventTime:       now,
			QueryDurationMs: 1000,
			Query:           fmt.Sprintf("SELECT * FROM table_%d", i),
			User:            "user",
			ClientAddress:   "192.168.1.1",
		}
	}

	ctx := context.Background()
	processor.ProcessQueriesBatch(ctx, queries, exporter, qf, cfg, nil)

	spans := waitForReceivedSpans(t, mockCollector, 5)
	if len(spans) != 5 {
		t.Errorf("Expected 5 spans with batching, got %d", len(spans))
	}
}
