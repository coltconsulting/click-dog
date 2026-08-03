package metrics

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/coltconsulting/click-dog/internal/clicklog"
	"github.com/coltconsulting/click-dog/internal/model"
)

// testShutdownCtx returns a short-lived context for graceful server shutdown
// in tests — 1s is plenty for no-traffic test servers.
func testShutdownCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// dialPath performs a GET against the server's bound address on the given
// path. Uses srv.Addr so tests binding on :0 get the OS-assigned port.
func dialPath(t *testing.T, srv *http.Server, path string) (*http.Response, error) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	return client.Get("http://" + srv.Addr + path)
}

func TestMetrics_RecordCycle(t *testing.T) {
	m := NewMetrics()

	m.RecordCycle(10, 5, 2, 100, nil)
	m.RecordCycle(0, 0, 0, 50, io.ErrUnexpectedEOF)

	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.spansExported != 10 {
		t.Errorf("spansExported = %d, want 10", m.spansExported)
	}
	if m.spansFiltered != 5 {
		t.Errorf("spansFiltered = %d, want 5", m.spansFiltered)
	}
	if m.spansDupes != 2 {
		t.Errorf("spansDupes = %d, want 2", m.spansDupes)
	}
	if m.cyclesSuccess != 1 {
		t.Errorf("cyclesSuccess = %d, want 1", m.cyclesSuccess)
	}
	if m.cyclesError != 1 {
		t.Errorf("cyclesError = %d, want 1", m.cyclesError)
	}
	if m.lastSuccessUnix == 0 {
		t.Error("lastSuccessUnix should be set after successful cycle")
	}
}

func TestMetrics_SetCircuitBreakerState(t *testing.T) {
	m := NewMetrics()

	m.SetCircuitBreakerState("open")
	m.mu.RLock()
	if m.circuitBreakerState != "open" {
		t.Errorf("circuitBreakerState = %q, want open", m.circuitBreakerState)
	}
	m.mu.RUnlock()

	m.SetCircuitBreakerState("closed")
	m.mu.RLock()
	if m.circuitBreakerState != "closed" {
		t.Errorf("circuitBreakerState = %q, want closed", m.circuitBreakerState)
	}
	m.mu.RUnlock()
}

func TestMetrics_SetBackoffInterval(t *testing.T) {
	m := NewMetrics()

	m.SetBackoffInterval(30 * time.Second)
	m.mu.RLock()
	if m.backoffIntervalSecs != 30.0 {
		t.Errorf("backoffIntervalSecs = %f, want 30.0", m.backoffIntervalSecs)
	}
	m.mu.RUnlock()
}

func TestMetrics_Handler(t *testing.T) {
	m := NewMetrics()
	m.RecordCycle(100, 20, 5, 200, nil)
	m.RecordCycle(50, 10, 3, 150, io.ErrUnexpectedEOF)
	m.RecordSkippedCycle(7)
	m.SetCircuitBreakerState("open")
	m.SetBackoffInterval(60 * time.Second)

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()

	m.Handler()(w, req)

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)
	output := string(body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	// Check content type
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}

	// Check counters and last-cycle gauges. The last cycle here is the
	// skipped one (counts zero; duration_seconds = 0.007), which proves the
	// gauges follow the most recent recorded cycle rather than the most
	// recent successful cycle.
	checks := []struct {
		metric string
		value  string
	}{
		{"click_dog_spans_exported_total", "150"},
		{"click_dog_spans_filtered_total", "30"},
		{"click_dog_spans_duplicates_total", "8"},
		{`click_dog_cycle_results_total{result="success"}`, "1"},
		{`click_dog_cycle_results_total{result="error"}`, "1"},
		{`click_dog_cycle_results_total{result="skipped"}`, "1"},
		{"click_dog_circuit_breaker_state", "2"}, // open = 2
		{"click_dog_leader", "1"},
		{"click_dog_backoff_interval_seconds", "60.0"},
		{"click_dog_last_cycle_duration_seconds", "0.007"},
		{"click_dog_last_cycle_exported_spans", "0"},
		{"click_dog_last_cycle_filtered_spans", "0"},
		{"click_dog_last_cycle_duplicate_spans", "0"},
	}

	for _, c := range checks {
		expected := c.metric + " " + c.value
		if !strings.Contains(output, expected) {
			t.Errorf("expected %q in output, got:\n%s", expected, output)
		}
	}

	// Check that HELP and TYPE lines are present
	if !strings.Contains(output, "# HELP click_dog_spans_exported_total") {
		t.Error("missing HELP for click_dog_spans_exported_total")
	}
	if !strings.Contains(output, "# TYPE click_dog_spans_exported_total counter") {
		t.Error("missing TYPE for click_dog_spans_exported_total")
	}
	if !strings.Contains(output, "# HELP click_dog_circuit_breaker_state Circuit breaker state (0=closed, 1=half_open, 2=open).") {
		t.Error("missing severity-ordered HELP for click_dog_circuit_breaker_state")
	}
	if !strings.Contains(output, `click_dog_last_success_timestamp_seconds{role="active"}`) {
		t.Error("missing role-tagged active last-success metric")
	}
	// New metrics need HELP/TYPE too — operators import these into Datadog
	// via OpenMetrics, which strips lines without the prefix.
	for _, name := range []string{
		"click_dog_export_attempts_total",
		"click_dog_export_accepted_total",
		"click_dog_export_errors_total",
		"click_dog_cycle_results_total",
		"click_dog_leader",
		"click_dog_last_cycle_duration_seconds",
		"click_dog_last_cycle_exported_spans",
		"click_dog_last_cycle_filtered_spans",
		"click_dog_last_cycle_duplicate_spans",
	} {
		if !strings.Contains(output, "# HELP "+name) {
			t.Errorf("missing HELP for %s", name)
		}
		if !strings.Contains(output, "# TYPE "+name) {
			t.Errorf("missing TYPE for %s", name)
		}
	}
}

func TestMetricProfiles_RenderNames(t *testing.T) {
	for _, d := range CanonicalDescriptors() {
		t.Run(d.Key, func(t *testing.T) {
			prom := PrometheusName(d)
			if !strings.HasPrefix(prom, "click_dog_"+d.Key) {
				t.Fatalf("PrometheusName(%q) = %q", d.Key, prom)
			}
			if d.Kind == MetricKindCounter && !strings.HasSuffix(prom, "_total") {
				t.Fatalf("PrometheusName(%q) = %q, want _total suffix", d.Key, prom)
			}
			if d.Kind == MetricKindGauge && strings.HasSuffix(prom, "_total") {
				t.Fatalf("PrometheusName(%q) = %q, gauge must not carry _total suffix", d.Key, prom)
			}
			if got, want := OTLPName(d, nil), "click_dog."+d.Key; got != want {
				t.Fatalf("OTLPName(%q) = %q, want %q", d.Key, got, want)
			}
			switch d.ValueType {
			case MetricValueTypeInt64:
			case MetricValueTypeFloat64:
				if d.Kind != MetricKindGauge {
					t.Fatalf("%s has float value type for non-gauge kind %s", d.Key, d.Kind)
				}
			default:
				t.Fatalf("%s has unsupported value type %q", d.Key, d.ValueType)
			}
		})
	}
}

func TestMetricProfiles_OTLPRenameOverride(t *testing.T) {
	d := mustDescriptor(MetricSpansExported)
	rename := map[string]string{MetricSpansExported: "myco.clickdog.spans_exported"}
	if got, want := OTLPName(d, rename), "myco.clickdog.spans_exported"; got != want {
		t.Fatalf("OTLPName with rename = %q, want %q", got, want)
	}
}

func TestMetrics_RegisterObservers_ReadsState(t *testing.T) {
	m := NewMetrics()
	m.RecordCycle(9, 3, 1, 250, nil)
	m.RecordSkippedCycle(12)
	m.RecordExportResult(model.ExportResult{Sinks: []model.ExportSinkStatus{{
		Name:     "otel",
		Sent:     9,
		Accepted: 7,
		Error:    errors.New("partial"),
	}}})
	m.SetCircuitBreakerState("half_open")
	m.SetBackoffInterval(45 * time.Second)
	m.SetLeader(false)
	m.SetTopologyWarning(TopologyReasonSidecar, true)
	m.RecordSpanLogPoll(4, time.Now().Add(-20*time.Second))
	m.RecordQueryLogEnrichmentCycle(2, 4, nil)
	m.RecordSpansWithQueryIDRatio(3, 4)
	m.SetNormalizedQuerySupported(true)
	m.SetQueryOperationSupported(true)

	reader := sdkmetric.NewManualReader(sdkmetric.WithTemporalitySelector(sdkmetric.DeltaTemporalitySelector))
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	_, err := m.RegisterObservers(provider.Meter("click-dog-test"), ObserverConfig{
		SinkNames: map[string]string{"otel": "otel_0"},
	})
	if err != nil {
		t.Fatalf("RegisterObservers: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	metricsByName := flattenOTLPMetrics(rm)

	for _, d := range CanonicalDescriptors() {
		name := OTLPName(d, nil)
		if _, ok := metricsByName[name]; !ok {
			t.Fatalf("missing OTLP metric %s", name)
		}
	}

	spansExported := metricsByName["click_dog."+MetricSpansExported].Data.(metricdata.Sum[int64])
	if spansExported.Temporality != metricdata.DeltaTemporality {
		t.Fatalf("spans_exported temporality = %v, want delta", spansExported.Temporality)
	}
	if got := intDataPointValue(t, metricsByName, MetricSpansExported, nil); got != 9 {
		t.Fatalf("spans_exported = %d, want 9", got)
	}
	if got := intDataPointValue(t, metricsByName, MetricExportAttempts, map[string]string{"sink": "otel_0"}); got != 9 {
		t.Fatalf("export_attempts{sink=otel_0} = %d, want 9", got)
	}
	if got := intDataPointValue(t, metricsByName, MetricExportErrors, map[string]string{"sink": "otel_0"}); got != 1 {
		t.Fatalf("export_errors{sink=otel_0} = %d, want 1", got)
	}
	if got := intDataPointValue(t, metricsByName, MetricCycleResults, map[string]string{"result": "success"}); got != 1 {
		t.Fatalf("cycle_results{success} = %d, want 1", got)
	}
	if got := intDataPointValue(t, metricsByName, MetricCycleResults, map[string]string{"result": "skipped"}); got != 1 {
		t.Fatalf("cycle_results{skipped} = %d, want 1", got)
	}
	if got := intDataPointValue(t, metricsByName, MetricCircuitBreakerState, nil); got != 1 {
		t.Fatalf("circuit_breaker_state = %d, want 1", got)
	}
	if got := intDataPointValue(t, metricsByName, MetricLeader, nil); got != 0 {
		t.Fatalf("leader = %d, want 0", got)
	}
	if got := intDataPointValue(t, metricsByName, MetricLastSuccessTimestampSeconds, map[string]string{"role": "standby"}); got <= 0 {
		t.Fatalf("last_success_timestamp_seconds{role=standby} = %d, want >0", got)
	}
	if got := intDataPointValue(t, metricsByName, MetricTopologyWarning, map[string]string{"reason": TopologyReasonSidecar}); got != 1 {
		t.Fatalf("topology_warning{sidecar} = %d, want 1", got)
	}
	if got := intDataPointValue(t, metricsByName, MetricTopologyWarning, map[string]string{"reason": TopologyReasonMultiInstance}); got != 0 {
		t.Fatalf("topology_warning{multi_instance} = %d, want stable 0", got)
	}
	if got := floatDataPointValue(t, metricsByName, MetricBackoffIntervalSeconds, nil); got != 45 {
		t.Fatalf("backoff_interval_seconds = %f, want 45", got)
	}
	if got := floatDataPointValue(t, metricsByName, MetricQueryLogEnrichmentMatchRatio, nil); got != 0.5 {
		t.Fatalf("query_log_enrichment_match_ratio = %f, want 0.5", got)
	}
	if got := intDataPointValue(t, metricsByName, MetricNormalizedQuerySupported, nil); got != 1 {
		t.Fatalf("normalized_query_supported = %d, want 1", got)
	}
	if got := intDataPointValue(t, metricsByName, MetricQueryOperationSupported, nil); got != 1 {
		t.Fatalf("query_operation_supported = %d, want 1", got)
	}
}

func TestMetrics_RegisterObservers_LastSuccessRoleAttribute(t *testing.T) {
	cases := []struct {
		name      string
		active    bool
		wantRole  string
		otherRole string
	}{
		{"active exporter", true, "active", "standby"},
		{"leader-gated standby", false, "standby", "active"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMetrics()
			m.RecordCycle(1, 0, 0, 10, nil)
			m.SetLeader(tc.active)

			reader := sdkmetric.NewManualReader()
			provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
			if _, err := m.RegisterObservers(provider.Meter("click-dog-test"), ObserverConfig{}); err != nil {
				t.Fatalf("RegisterObservers: %v", err)
			}

			var rm metricdata.ResourceMetrics
			if err := reader.Collect(context.Background(), &rm); err != nil {
				t.Fatalf("Collect: %v", err)
			}
			metricsByName := flattenOTLPMetrics(rm)
			if got := intDataPointValue(t, metricsByName, MetricLastSuccessTimestampSeconds, map[string]string{"role": tc.wantRole}); got <= 0 {
				t.Fatalf("last_success_timestamp_seconds{role=%s} = %d, want >0", tc.wantRole, got)
			}
			assertNoIntDataPoint(t, metricsByName, MetricLastSuccessTimestampSeconds, map[string]string{"role": tc.otherRole})
		})
	}
}

func TestMetrics_RegisterObservers_LastSuccessAbsentUntilSuccess(t *testing.T) {
	m := NewMetrics()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	_, err := m.RegisterObservers(provider.Meter("click-dog-test"), ObserverConfig{})
	if err != nil {
		t.Fatalf("RegisterObservers: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	metricsByName := flattenOTLPMetrics(rm)
	name := "click_dog." + MetricLastSuccessTimestampSeconds
	if metric, ok := metricsByName[name]; ok {
		points := metric.Data.(metricdata.Gauge[int64]).DataPoints
		if len(points) != 0 {
			t.Fatalf("%s emitted %d point(s) before first success, want absent", name, len(points))
		}
	}
}

func TestMetrics_RecordExportResult(t *testing.T) {
	m := NewMetrics()
	boom := errors.New("boom")

	m.RecordExportResult(model.ExportResult{Sinks: []model.ExportSinkStatus{
		{Name: "otel", Sent: 3, Accepted: 3},
		{Name: "splunk_hec", Sent: 3, Accepted: 1, Error: boom},
		{Name: "quote\"slash\\", Sent: 1, Accepted: 1},
	}})
	m.RecordExportResult(model.ExportResult{Sinks: []model.ExportSinkStatus{
		{Name: "otel", Sent: 2, Accepted: 2},
		{Name: "", Sent: 1, Accepted: 0, Error: boom},
	}})

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	m.Handler()(w, req)
	output := w.Body.String()

	for _, want := range []string{
		`click_dog_export_attempts_total{sink="otel"} 5`,
		`click_dog_export_accepted_total{sink="otel"} 5`,
		`click_dog_export_errors_total{sink="otel"} 0`,
		`click_dog_export_attempts_total{sink="splunk_hec"} 3`,
		`click_dog_export_accepted_total{sink="splunk_hec"} 1`,
		`click_dog_export_errors_total{sink="splunk_hec"} 1`,
		`click_dog_export_attempts_total{sink="unknown"} 1`,
		`click_dog_export_accepted_total{sink="unknown"} 0`,
		`click_dog_export_errors_total{sink="unknown"} 1`,
		`click_dog_export_attempts_total{sink="quote\"slash\\"} 1`,
	} {
		if !strings.Contains(output, want) {
			t.Errorf("metrics body missing %q\n----\n%s\n----", want, output)
		}
	}
}

// TestMetrics_CycleResultBuckets covers the three-way classification done by
// recordCycle: success (no err, not skipped), error (any err — including a
// skipped+err combination, which never happens in production but the routing
// must still favor "error"), and skipped (no err, Skipped=true).
//
// The fixed enum keeps the result label low-cardinality regardless of cycle
// volume; this test pins the routing so a future refactor can't silently
// reclassify, e.g., partial-multi-exporter failures into the success bucket.
func TestMetrics_CycleResultBuckets(t *testing.T) {
	m := NewMetrics()
	m.RecordCycle(10, 1, 0, 50, nil)               // success
	m.RecordCycle(5, 0, 0, 30, nil)                // success
	m.RecordCycle(0, 0, 0, 7, io.ErrUnexpectedEOF) // error (no exports)
	// Partial-success-then-error shape: a cycle that exported some spans
	// and then hit an error (e.g. partial multi-exporter failure as in #92).
	// Routing must favor `error` regardless of non-zero export counts —
	// otherwise an exporter that fails after partial success would silently
	// inflate the success bucket.
	m.RecordCycle(15, 3, 0, 20, io.ErrUnexpectedEOF) // error (with exports)
	m.RecordSkippedCycle(2)                          // skipped
	m.RecordSkippedCycle(2)                          // skipped

	// Assert via the rendered exposition format — the contract operators
	// consume — rather than poking at unexported fields. The one white-box
	// assertion below verifies the accounting relationship directly.
	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	m.Handler()(w, req)
	output := w.Body.String()

	for _, want := range []string{
		`click_dog_cycle_results_total{result="success"} 2`,
		`click_dog_cycle_results_total{result="error"} 2`,
		`click_dog_cycle_results_total{result="skipped"} 2`,
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q\n----\n%s\n----", want, output)
		}
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	if got := m.cyclesSuccess + m.cyclesError + m.cyclesSkipped; got != 6 {
		t.Errorf("cycle results (s=%d e=%d sk=%d) sum to %d, want 6",
			m.cyclesSuccess, m.cyclesError, m.cyclesSkipped, got)
	}
}

// TestMetrics_LastCycleGaugesFollowLatest pins last-cycle gauges to the most
// recent recorded cycle (not the most recent successful cycle). Without this,
// a transient error would freeze the dashboard's "last cycle exported" tile
// at a stale success and the gauge would silently lie about current state.
func TestMetrics_LastCycleGaugesFollowLatest(t *testing.T) {
	m := NewMetrics()
	m.RecordCycle(10, 4, 1, 250, nil)
	snap := m.Snapshot()
	if snap.LastCycle.Exported != 10 || snap.LastCycle.DurationMs != 250 {
		t.Fatalf("first cycle snapshot = %+v", snap.LastCycle)
	}

	// An error cycle reports zero counts; the gauges must reflect that, not
	// the previous success.
	m.RecordCycle(0, 0, 0, 17, io.ErrUnexpectedEOF)
	snap = m.Snapshot()
	if snap.LastCycle.Exported != 0 || snap.LastCycle.Filtered != 0 ||
		snap.LastCycle.Duplicates != 0 {
		t.Errorf("error cycle gauges = %+v, want zeros", snap.LastCycle)
	}
	if snap.LastCycle.DurationMs != 17 {
		t.Errorf("error cycle DurationMs = %d, want 17", snap.LastCycle.DurationMs)
	}
	if snap.LastCycle.Err == "" {
		t.Error("error cycle should populate LastCycle.Err")
	}
}

func TestMetrics_HealthEndpoint(t *testing.T) {
	m := NewMetrics()

	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", m.Handler())
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("health status = %d, want 200", resp.StatusCode)
	}
	if string(body) != `{"status":"ok"}` {
		t.Errorf("health body = %q, want ok", string(body))
	}
}

func TestMetrics_Snapshot(t *testing.T) {
	m := NewMetrics()

	// Fresh metrics: no cycle yet, default-closed CB, zero backoff, upSince set.
	s := m.Snapshot()
	if s.HaveLastCycle {
		t.Error("HaveLastCycle = true before any cycle recorded")
	}
	if s.CircuitBreakerState != "closed" {
		t.Errorf("CircuitBreakerState = %q, want closed (default)", s.CircuitBreakerState)
	}
	if s.BackoffIntervalSecs != 0 {
		t.Errorf("BackoffIntervalSecs = %f, want 0", s.BackoffIntervalSecs)
	}
	if s.UpSince.IsZero() {
		t.Error("UpSince is zero; should be set in NewMetrics")
	}

	// After a successful cycle, LastCycle reports its fields.
	m.RecordCycle(10, 5, 2, 123, nil)
	s = m.Snapshot()
	if !s.HaveLastCycle {
		t.Fatal("HaveLastCycle = false after RecordCycle")
	}
	if s.LastCycle.Exported != 10 || s.LastCycle.Filtered != 5 ||
		s.LastCycle.Duplicates != 2 || s.LastCycle.DurationMs != 123 {
		t.Errorf("LastCycle = %+v", s.LastCycle)
	}
	if s.LastCycle.Err != "" {
		t.Errorf("LastCycle.Err = %q, want empty on success", s.LastCycle.Err)
	}
	if s.LastCycle.At.IsZero() {
		t.Error("LastCycle.At is zero after RecordCycle")
	}

	// After a failed cycle, Err is populated.
	m.RecordCycle(0, 0, 0, 7, io.ErrUnexpectedEOF)
	s = m.Snapshot()
	if s.LastCycle.Err == "" {
		t.Error("LastCycle.Err should carry the error string after a failed cycle")
	}

	// Gauge setters reflect through Snapshot.
	m.SetCircuitBreakerState("half_open")
	m.SetBackoffInterval(45 * time.Second)
	s = m.Snapshot()
	if s.CircuitBreakerState != "half_open" {
		t.Errorf("CircuitBreakerState after set = %q", s.CircuitBreakerState)
	}
	if s.BackoffIntervalSecs != 45 {
		t.Errorf("BackoffIntervalSecs after set = %f, want 45", s.BackoffIntervalSecs)
	}

	// Snapshot must not expose shared state — mutating the returned struct
	// shouldn't affect subsequent reads.
	s.CircuitBreakerState = "open"
	if again := m.Snapshot().CircuitBreakerState; again != "half_open" {
		t.Errorf("Snapshot returned shared state; second read = %q", again)
	}
}

func TestMetrics_RecordSkippedCycle(t *testing.T) {
	m := NewMetrics()

	// A successful cycle first — establishes lastSuccessUnix.
	m.RecordCycle(5, 1, 0, 50, nil)
	before := m.Snapshot()
	if before.LastCycle.Skipped {
		t.Error("successful cycle should not be marked skipped")
	}
	priorSuccess := m.lastSuccessUnix

	// Skipped cycle: must flip the snapshot's Skipped flag, must NOT
	// advance lastSuccessUnix (a skip is not evidence of health).
	m.RecordSkippedCycle(7)
	after := m.Snapshot()
	if !after.LastCycle.Skipped {
		t.Error("RecordSkippedCycle did not set Skipped=true")
	}
	if after.LastCycle.Exported != 0 || after.LastCycle.Filtered != 0 ||
		after.LastCycle.Duplicates != 0 {
		t.Errorf("skipped snapshot had non-zero counts: %+v", after.LastCycle)
	}
	if after.LastCycle.DurationMs != 7 {
		t.Errorf("skipped snapshot duration = %d, want 7", after.LastCycle.DurationMs)
	}
	if m.lastSuccessUnix != priorSuccess {
		t.Errorf("RecordSkippedCycle advanced lastSuccessUnix from %d to %d", priorSuccess, m.lastSuccessUnix)
	}

	m.RecordSkippedCycleWithReason(9, SkipReasonLeaderStandby)
	withReason := m.Snapshot()
	if withReason.LastCycle.SkipReason != SkipReasonLeaderStandby {
		t.Errorf("SkipReason = %q, want %q", withReason.LastCycle.SkipReason, SkipReasonLeaderStandby)
	}
	if m.lastSuccessUnix != priorSuccess {
		t.Errorf("RecordSkippedCycleWithReason advanced lastSuccessUnix from %d to %d", priorSuccess, m.lastSuccessUnix)
	}
}

func TestMetrics_SetLeader(t *testing.T) {
	m := NewMetrics()
	if got := m.Snapshot().Leader; got != 1 {
		t.Fatalf("default Leader = %d, want 1", got)
	}
	m.SetLeader(false)
	if got := m.Snapshot().Leader; got != 0 {
		t.Fatalf("Leader after SetLeader(false) = %d, want 0", got)
	}
	m.SetLeader(true)
	if got := m.Snapshot().Leader; got != 1 {
		t.Fatalf("Leader after SetLeader(true) = %d, want 1", got)
	}
}

func TestStartMetricsServer_BindError(t *testing.T) {
	// An invalid address should surface as a returned error, not a silent
	// goroutine exit. Verifies the sync-bind alignment with health.Start.
	_, err := StartMetricsServer("totally:not:a:port", NewMetrics())
	if err == nil {
		t.Fatal("expected bind error for invalid address, got nil")
	}
}

func TestStartMetricsServer_MuxExtensionMounts(t *testing.T) {
	// Bind on :0 so the OS picks a free port, then confirm that a handler
	// registered via MuxExtension is reachable through the live server.
	// Guards the variadic extension contract used by main.go to mount
	// health handlers on the metrics listener.
	var extCalls int
	ext := func(mux *http.ServeMux) {
		extCalls++
		mux.HandleFunc("/ext-probe", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ext-ok"))
		})
	}

	srv, err := StartMetricsServer("127.0.0.1:0", NewMetrics(), ext)
	if err != nil {
		t.Fatalf("StartMetricsServer: %v", err)
	}
	defer func() {
		_ = srv.Shutdown(testShutdownCtx(t))
	}()
	if extCalls != 1 {
		t.Errorf("extension invoked %d times, want 1", extCalls)
	}

	// Dial the real listener and verify the extension handler responds.
	resp, err := dialPath(t, srv, "/ext-probe")
	if err != nil {
		t.Fatalf("dial /ext-probe: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/ext-probe status = %d, want 200", resp.StatusCode)
	}

	// Also confirm built-in routes are still mounted.
	resp2, err := dialPath(t, srv, "/metrics")
	if err != nil {
		t.Fatalf("dial /metrics: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("/metrics status = %d, want 200", resp2.StatusCode)
	}
}

// TestStartMetricsServer_FlushNotMounted guards the scrape/admin separation:
// /flush must NOT be reachable on the scrape listener. The whole point of
// the two-listener split is that operators who expose /metrics off-host
// don't accidentally expose /flush. If a future refactor reintroduces /flush
// here, this test fails loudly.
//
// srv.Addr carries the resolved listener address (not the configured ":0")
// because startHTTPServer back-fills it from ln.Addr() after net.Listen.
// http.Server.Addr is normally the input you pass to ListenAndServe; this
// package overwrites it with the bound address so :0-using tests can dial.
func TestStartMetricsServer_FlushNotMounted(t *testing.T) {
	srv, err := StartMetricsServer("127.0.0.1:0", NewMetrics())
	if err != nil {
		t.Fatalf("StartMetricsServer: %v", err)
	}
	defer func() { _ = srv.Shutdown(testShutdownCtx(t)) }()

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Post("http://"+srv.Addr+"/flush", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /flush on scrape listener: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/flush on scrape listener: status = %d, want 404", resp.StatusCode)
	}
}

// TestStartAdminServer_Flush verifies the admin listener accepts POST /flush
// and signals the channel, and that a second POST while the channel is full
// returns 409 instead of blocking.
func TestStartAdminServer_Flush(t *testing.T) {
	flushChan := make(chan struct{}, 1)
	srv, err := StartAdminServer("127.0.0.1:0", flushChan)
	if err != nil {
		t.Fatalf("StartAdminServer: %v", err)
	}
	defer func() { _ = srv.Shutdown(testShutdownCtx(t)) }()

	client := &http.Client{Timeout: 2 * time.Second}

	resp, err := client.Post("http://"+srv.Addr+"/flush", "application/json", nil)
	if err != nil {
		t.Fatalf("first POST /flush: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("first /flush status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	select {
	case <-flushChan:
	default:
		t.Fatal("flushChan did not receive signal after POST /flush")
	}

	// Re-fill the channel, then confirm the next POST returns 409 rather
	// than blocking — this is the behavior main relies on so a runaway
	// caller can't queue work behind the processor.
	flushChan <- struct{}{}
	resp2, err := client.Post("http://"+srv.Addr+"/flush", "application/json", nil)
	if err != nil {
		t.Fatalf("second POST /flush: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("second /flush status = %d, want 409", resp2.StatusCode)
	}

	// GET on /flush is rejected — admin write actions stay POST-only.
	respGet, err := client.Get("http://" + srv.Addr + "/flush")
	if err != nil {
		t.Fatalf("GET /flush: %v", err)
	}
	defer func() { _ = respGet.Body.Close() }()
	if respGet.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /flush status = %d, want 405", respGet.StatusCode)
	}
}

// TestStartAdminServer_NilFlushChan rejects a nil flushChan at construction
// time. /flush only makes sense if there's something to signal, and a nil
// channel here would be a programming error worth surfacing immediately
// rather than at first request time.
func TestStartAdminServer_NilFlushChan(t *testing.T) {
	_, err := StartAdminServer("127.0.0.1:0", nil)
	if err == nil {
		t.Fatal("expected error for nil flushChan, got nil")
	}
}

// TestStartAdminServer_BindError mirrors TestStartMetricsServer_BindError —
// invalid address surfaces as a returned error, not a silent goroutine exit.
// Both servers go through startHTTPServer today, but a direct test catches
// future error-wrapping divergence.
//
// The prefix check uses HasPrefix (not Contains) on the exact "admin: bind "
// shape so a rename like "admin_listener:" or "admin server:" would fail
// rather than silently match a substring.
func TestStartAdminServer_BindError(t *testing.T) {
	_, err := StartAdminServer("totally:not:a:port", make(chan struct{}, 1))
	if err == nil {
		t.Fatal("expected bind error for invalid address, got nil")
	}
	if !strings.HasPrefix(err.Error(), "admin: bind ") {
		t.Errorf("error missing 'admin: bind ' prefix: %v", err)
	}
}

// captureClicklogWarn wires log.SetOutput to a buffer so clicklog.Warn
// output can be asserted against, lowers the log level so warns aren't
// suppressed, and verifies the wiring with a canary warn before returning.
//
// **Not safe under t.Parallel.** This mutates two pieces of global state —
// clicklog's package-level log level (via InitLogger) and stdlib log's
// output writer (via log.SetOutput). Calling t.Parallel in a test that
// uses this helper would race against any other test that touches either
// global. Tests that use this helper must run serially.
//
// The canary is the load-bearing piece. clicklog.Warn currently routes
// through the stdlib log package; log.SetOutput(&buf) captures that. If a
// future clicklog refactor switches to writing os.Stderr or a private
// writer directly, this helper's assertions still depend on the old path
// — without the canary, an absence-style test (e.g. "log must NOT contain
// 'non-loopback'") would pass vacuously against an empty buffer. The
// canary fails the test loudly the instant capture goes silent.
//
// Buffer is reset after the canary so callers see only the output of the
// code under test.
func captureClicklogWarn(t *testing.T) *bytes.Buffer {
	t.Helper()
	if err := clicklog.InitLogger("warn", "", "text", 0, 0); err != nil {
		t.Fatalf("InitLogger: %v", err)
	}
	t.Cleanup(clicklog.CloseLogger)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	const canary = "__capture_canary__"
	clicklog.Warn(canary)
	if !strings.Contains(buf.String(), canary) {
		t.Fatalf("log capture broken: clicklog.Warn no longer routes through stdlib log. buf=%q", buf.String())
	}
	buf.Reset()
	return &buf
}

// TestStartAdminServer_NonLoopbackWarn asserts the WARN fires when admin
// is bound off-loopback. The warning is the operator's signal that they've
// taken on the off-host /flush exposure trade-off; if it ever stops firing,
// that signal goes silent.
func TestStartAdminServer_NonLoopbackWarn(t *testing.T) {
	buf := captureClicklogWarn(t)

	flushChan := make(chan struct{}, 1)
	srv, err := StartAdminServer("0.0.0.0:0", flushChan)
	if err != nil {
		t.Fatalf("StartAdminServer: %v", err)
	}
	defer func() { _ = srv.Shutdown(testShutdownCtx(t)) }()

	out := buf.String()
	if !strings.Contains(out, "non-loopback") {
		t.Errorf("expected non-loopback warning in log output, got:\n%s", out)
	}
}

// TestStartAdminServer_LoopbackNoWarn is the positive control for the
// non-loopback test above: loopback binds (the default posture) must NOT
// emit the warning. Together they pin both directions of the gate so a
// future regression that always-fires or never-fires is caught.
//
// captureClicklogWarn's canary check guards this test against a vacuous
// pass: if log capture ever stops working, the absence check below would
// trivially "succeed" against an empty buffer. The canary catches that.
func TestStartAdminServer_LoopbackNoWarn(t *testing.T) {
	buf := captureClicklogWarn(t)

	flushChan := make(chan struct{}, 1)
	srv, err := StartAdminServer("127.0.0.1:0", flushChan)
	if err != nil {
		t.Fatalf("StartAdminServer: %v", err)
	}
	defer func() { _ = srv.Shutdown(testShutdownCtx(t)) }()

	if strings.Contains(buf.String(), "non-loopback") {
		t.Errorf("loopback bind should not emit non-loopback warning; got:\n%s", buf.String())
	}
}

func TestIsLoopbackAddress(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:9091", true},
		{"127.0.0.2:9091", true}, // full 127.0.0.0/8 range is loopback per ip.IsLoopback
		{"localhost:9091", true},
		{"[::1]:9091", true},
		{"0.0.0.0:9091", false},
		{":9091", false},     // wildcard bind
		{"[::]:9091", false}, // IPv6 wildcard — not loopback
		{"10.0.0.5:9091", false},
		{"example.internal:9091", false}, // hostnames not resolved at startup
		{"not a real address", false},
	}
	for _, c := range cases {
		if got := isLoopbackAddress(c.addr); got != c.want {
			t.Errorf("isLoopbackAddress(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

func TestMetrics_CircuitBreakerStateValues(t *testing.T) {
	m := NewMetrics()

	tests := []struct {
		state    string
		expected string
	}{
		{"closed", "click_dog_circuit_breaker_state 0"},
		{"half_open", "click_dog_circuit_breaker_state 1"},
		{"half-open", "click_dog_circuit_breaker_state 1"},
		{"open", "click_dog_circuit_breaker_state 2"},
	}

	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			m.SetCircuitBreakerState(tt.state)

			req := httptest.NewRequest("GET", "/metrics", nil)
			w := httptest.NewRecorder()
			m.Handler()(w, req)

			body, _ := io.ReadAll(w.Result().Body)
			if !strings.Contains(string(body), tt.expected) {
				t.Errorf("expected %q in output for state %q", tt.expected, tt.state)
			}
		})
	}
}

// TestMetrics_TopologyWarning covers the stable-0 series, the per-reason
// setter, mutual-exclusion of the /status reason, and the clear path.
func TestMetrics_TopologyWarning(t *testing.T) {
	m := NewMetrics()

	// Stable-0: both known reasons emit 0 from startup, with no SetTopologyWarning
	// call — the auditor-skipped (use_cluster_queries off) case must still render
	// a deterministic 0, not no-data.
	body := renderBody(t, m)
	for _, reason := range KnownTopologyReasons {
		want := "click_dog_topology_warning{reason=\"" + reason + "\"} 0"
		if !strings.Contains(body, want) {
			t.Errorf("fresh /metrics missing stable-0 series %q:\n%s", want, body)
		}
	}
	if s := m.Snapshot(); s.TopologyWarning != "" {
		t.Errorf("fresh Snapshot.TopologyWarning = %q, want empty", s.TopologyWarning)
	}

	// Auditor writes the full known set every tick: active reason → 1, others → 0.
	m.SetTopologyWarning(TopologyReasonSidecar, true)
	m.SetTopologyWarning(TopologyReasonMultiInstance, false)
	body = renderBody(t, m)
	if !strings.Contains(body, "click_dog_topology_warning{reason=\""+TopologyReasonSidecar+"\"} 1") {
		t.Errorf("sidecar reason not rendered as 1:\n%s", body)
	}
	if !strings.Contains(body, "click_dog_topology_warning{reason=\""+TopologyReasonMultiInstance+"\"} 0") {
		t.Errorf("multi_instance reason not rendered as 0:\n%s", body)
	}
	if s := m.Snapshot(); s.TopologyWarning != TopologyReasonSidecar {
		t.Errorf("Snapshot.TopologyWarning = %q, want %q", s.TopologyWarning, TopologyReasonSidecar)
	}

	// Clear: all reasons back to 0, /status reason empty again.
	m.SetTopologyWarning(TopologyReasonSidecar, false)
	if s := m.Snapshot(); s.TopologyWarning != "" {
		t.Errorf("after clear, Snapshot.TopologyWarning = %q, want empty", s.TopologyWarning)
	}
}

// renderBody is a tiny test helper that scrapes /metrics through Handler
// and returns the body as a string. The new health-metric tests below
// all need this — pulling it into a helper keeps each test focused on
// the assertions rather than the HTTP plumbing.
func renderBody(t *testing.T, m *Metrics) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	m.Handler()(w, req)
	body, _ := io.ReadAll(w.Result().Body)
	return string(body)
}

func flattenOTLPMetrics(rm metricdata.ResourceMetrics) map[string]metricdata.Metrics {
	out := make(map[string]metricdata.Metrics)
	for _, scope := range rm.ScopeMetrics {
		for _, metric := range scope.Metrics {
			out[metric.Name] = metric
		}
	}
	return out
}

func intDataPointValue(t *testing.T, metricsByName map[string]metricdata.Metrics, key string, attrs map[string]string) int64 {
	t.Helper()
	metric, ok := metricsByName["click_dog."+key]
	if !ok {
		t.Fatalf("missing metric %s", key)
	}
	switch data := metric.Data.(type) {
	case metricdata.Sum[int64]:
		for _, point := range data.DataPoints {
			if pointMatchesAttrs(point.Attributes, attrs) {
				return point.Value
			}
		}
	case metricdata.Gauge[int64]:
		for _, point := range data.DataPoints {
			if pointMatchesAttrs(point.Attributes, attrs) {
				return point.Value
			}
		}
	default:
		t.Fatalf("metric %s data type = %T, want int64 sum or gauge", key, data)
	}
	t.Fatalf("metric %s missing attrs %+v", key, attrs)
	return 0
}

func assertNoIntDataPoint(t *testing.T, metricsByName map[string]metricdata.Metrics, key string, attrs map[string]string) {
	t.Helper()
	metric, ok := metricsByName["click_dog."+key]
	if !ok {
		return
	}
	switch data := metric.Data.(type) {
	case metricdata.Sum[int64]:
		for _, point := range data.DataPoints {
			if pointMatchesAttrs(point.Attributes, attrs) {
				t.Fatalf("metric %s unexpectedly had attrs %+v with value %d", key, attrs, point.Value)
			}
		}
	case metricdata.Gauge[int64]:
		for _, point := range data.DataPoints {
			if pointMatchesAttrs(point.Attributes, attrs) {
				t.Fatalf("metric %s unexpectedly had attrs %+v with value %d", key, attrs, point.Value)
			}
		}
	default:
		t.Fatalf("metric %s data type = %T, want int64 sum or gauge", key, data)
	}
}

func floatDataPointValue(t *testing.T, metricsByName map[string]metricdata.Metrics, key string, attrs map[string]string) float64 {
	t.Helper()
	metric, ok := metricsByName["click_dog."+key]
	if !ok {
		t.Fatalf("missing metric %s", key)
	}
	data, ok := metric.Data.(metricdata.Gauge[float64])
	if !ok {
		t.Fatalf("metric %s data type = %T, want float64 gauge", key, metric.Data)
	}
	for _, point := range data.DataPoints {
		if pointMatchesAttrs(point.Attributes, attrs) {
			return point.Value
		}
	}
	t.Fatalf("metric %s missing attrs %+v", key, attrs)
	return 0
}

func pointMatchesAttrs(set attribute.Set, attrs map[string]string) bool {
	if len(attrs) == 0 {
		return set.Len() == 0
	}
	if set.Len() != len(attrs) {
		return false
	}
	for key, want := range attrs {
		got, ok := set.Value(attribute.Key(key))
		if !ok || got.AsString() != want {
			return false
		}
	}
	return true
}

// TestMetrics_RecordSpanLogPoll_Empty proves the empty-result path (#183
// "click-dog is reachable but ClickHouse is emitting nothing"):
//   - last_poll_timestamp advances so the operator sees click-dog is alive,
//   - rows_last_cycle goes to 0 so a dashboard panel for "raw row count"
//     drops to zero immediately,
//   - newest_row_age does NOT reset — the staleness signal must keep
//     growing across consecutive empty polls, otherwise we'd hide exactly
//     the problem we're trying to surface.
func TestMetrics_RecordSpanLogPoll_Empty(t *testing.T) {
	m := NewMetrics()
	// Seed a known newest-row time five minutes in the past so we can
	// assert the empty poll did not overwrite it.
	seed := time.Now().Add(-5 * time.Minute)
	m.RecordSpanLogPoll(3, seed)

	// Empty poll: rowCount=0, newestRow=time.Time{} (the documented
	// "no rows observed" sentinel for the second arg).
	before := time.Now().Unix()
	m.RecordSpanLogPoll(0, time.Time{})
	after := time.Now().Unix()

	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.spanLogRowsLastCycle != 0 {
		t.Errorf("rowsLastCycle = %d, want 0", m.spanLogRowsLastCycle)
	}
	if m.spanLogLastPollUnix < before || m.spanLogLastPollUnix > after {
		t.Errorf("lastPollUnix = %d, want between %d and %d",
			m.spanLogLastPollUnix, before, after)
	}
	if m.spanLogNewestRowTimeUnix != seed.Unix() {
		t.Errorf("newestRowTimeUnix = %d, want preserved seed %d",
			m.spanLogNewestRowTimeUnix, seed.Unix())
	}
}

// TestMetrics_RecordSpanLogPoll_Render checks that the rendered output
// reflects the recorded state — that age is computed from now (not a
// frozen "as of poll" value), and that the metric set is stable from the
// first scrape (zero-value lines exist before any poll).
func TestMetrics_RecordSpanLogPoll_Render(t *testing.T) {
	m := NewMetrics()

	// Before any RecordSpanLogPoll call, the gauges still appear in
	// output — stable metric set policy. age_seconds is 0 (documented
	// as "no observation yet").
	output := renderBody(t, m)
	for _, want := range []string{
		"click_dog_span_log_last_poll_timestamp_seconds 0\n",
		"click_dog_span_log_newest_row_age_seconds 0\n",
		"click_dog_span_log_rows_last_cycle 0\n",
		"# HELP click_dog_span_log_last_poll_timestamp_seconds",
		"# TYPE click_dog_span_log_last_poll_timestamp_seconds gauge",
		"# HELP click_dog_span_log_newest_row_age_seconds",
		"# TYPE click_dog_span_log_newest_row_age_seconds gauge",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("expected %q in pre-observation output, got:\n%s", want, output)
		}
	}

	// Record a poll whose newest row is ~30 seconds old. Age is computed
	// in render() (not at record time), so the rendered value should be
	// close to 30 — within a small wall-clock fudge for scheduler delay.
	m.RecordSpanLogPoll(42, time.Now().Add(-30*time.Second))
	output = renderBody(t, m)

	if !strings.Contains(output, "click_dog_span_log_rows_last_cycle 42\n") {
		t.Errorf("rows_last_cycle line missing or wrong, output:\n%s", output)
	}

	// Extract the age and check it's in [28, 33]. The seed is 30s in
	// the past; the window is wider than strictly necessary so a
	// loaded CI runner doesn't flap. String scanning keeps the test
	// simple and obviously correct.
	for _, want := range []string{
		"click_dog_span_log_newest_row_age_seconds 28\n",
		"click_dog_span_log_newest_row_age_seconds 29\n",
		"click_dog_span_log_newest_row_age_seconds 30\n",
		"click_dog_span_log_newest_row_age_seconds 31\n",
		"click_dog_span_log_newest_row_age_seconds 32\n",
		"click_dog_span_log_newest_row_age_seconds 33\n",
	} {
		if strings.Contains(output, want) {
			return
		}
	}
	t.Errorf("newest_row_age_seconds not in expected 28-33 range, output:\n%s", output)
}

// TestMetrics_RecordQueryLogEnrichmentCycle covers all three branches:
// no-op when total==0 (guard prevents inflating attempt counter on
// healthy-but-quiet windows), failure (attempts++ and failures++, but
// match_ratio is NOT updated), success (attempts++, successes++,
// match_ratio = matched/total).
func TestMetrics_RecordQueryLogEnrichmentCycle(t *testing.T) {
	m := NewMetrics()

	// No-op: total<=0 must not bump any counters.
	m.RecordQueryLogEnrichmentCycle(0, 0, nil)
	m.mu.RLock()
	if m.queryLogEnrichAttempts != 0 || m.queryLogEnrichSuccesses != 0 || m.queryLogEnrichFailures != 0 {
		t.Errorf("zero-total call should be no-op, got attempts=%d successes=%d failures=%d",
			m.queryLogEnrichAttempts, m.queryLogEnrichSuccesses, m.queryLogEnrichFailures)
	}
	m.mu.RUnlock()

	// Failure: attempt + failure increment; match_ratio stays untouched.
	m.RecordQueryLogEnrichmentCycle(5, 10, errors.New("query_log unavailable"))
	m.mu.RLock()
	if m.queryLogEnrichAttempts != 1 || m.queryLogEnrichFailures != 1 || m.queryLogEnrichSuccesses != 0 {
		t.Errorf("after failure: attempts=%d successes=%d failures=%d, want 1/0/1",
			m.queryLogEnrichAttempts, m.queryLogEnrichSuccesses, m.queryLogEnrichFailures)
	}
	if m.queryLogEnrichMatchRatio != 0 {
		t.Errorf("match_ratio = %f after failure, want 0 (unchanged)", m.queryLogEnrichMatchRatio)
	}
	m.mu.RUnlock()

	// Success: 3/4 = 0.75 match ratio.
	m.RecordQueryLogEnrichmentCycle(3, 4, nil)
	m.mu.RLock()
	if m.queryLogEnrichAttempts != 2 || m.queryLogEnrichSuccesses != 1 || m.queryLogEnrichFailures != 1 {
		t.Errorf("after success: attempts=%d successes=%d failures=%d, want 2/1/1",
			m.queryLogEnrichAttempts, m.queryLogEnrichSuccesses, m.queryLogEnrichFailures)
	}
	if m.queryLogEnrichMatchRatio != 0.75 {
		t.Errorf("match_ratio = %f, want 0.75", m.queryLogEnrichMatchRatio)
	}
	m.mu.RUnlock()

	// Render once and assert all four enrichment lines surface correctly.
	output := renderBody(t, m)
	for _, want := range []string{
		"click_dog_query_log_enrichment_attempts_total 2\n",
		"click_dog_query_log_enrichment_successes_total 1\n",
		"click_dog_query_log_enrichment_failures_total 1\n",
		"click_dog_query_log_enrichment_match_ratio 0.7500\n",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("expected %q in output, got:\n%s", want, output)
		}
	}
}

// TestMetrics_RecordSpansWithQueryIDRatio asserts the zero-total guard
// (an empty cycle does NOT zero the gauge, which would falsely alarm
// dashboards) and the ratio math on a populated cycle.
func TestMetrics_RecordSpansWithQueryIDRatio(t *testing.T) {
	m := NewMetrics()

	// First populate the gauge with a known value.
	m.RecordSpansWithQueryIDRatio(7, 10) // 0.7
	m.mu.RLock()
	if m.spansWithQueryIDRatio != 0.7 {
		t.Errorf("ratio = %f, want 0.7", m.spansWithQueryIDRatio)
	}
	m.mu.RUnlock()

	// Empty cycle: total=0 must be a no-op.
	m.RecordSpansWithQueryIDRatio(0, 0)
	m.mu.RLock()
	if m.spansWithQueryIDRatio != 0.7 {
		t.Errorf("ratio = %f after empty cycle, want 0.7 (unchanged)", m.spansWithQueryIDRatio)
	}
	m.mu.RUnlock()

	// New observation overwrites.
	m.RecordSpansWithQueryIDRatio(2, 5) // 0.4
	output := renderBody(t, m)
	if !strings.Contains(output, "click_dog_spans_with_query_id_ratio 0.4000\n") {
		t.Errorf("expected ratio=0.4000, output:\n%s", output)
	}
}

// TestMetrics_SetNormalizedQuerySupported exercises both values and the
// render output. The gauge is intentionally low-cardinality (no labels)
// because the capability is a process-wide constant.
func TestMetrics_SetNormalizedQuerySupported(t *testing.T) {
	m := NewMetrics()

	// Pre-set: default zero. Verifies the stable-metric-set policy.
	output := renderBody(t, m)
	if !strings.Contains(output, "click_dog_normalized_query_supported 0\n") {
		t.Errorf("default gauge missing, output:\n%s", output)
	}

	m.SetNormalizedQuerySupported(true)
	output = renderBody(t, m)
	if !strings.Contains(output, "click_dog_normalized_query_supported 1\n") {
		t.Errorf("after SetNormalizedQuerySupported(true), output:\n%s", output)
	}

	m.SetNormalizedQuerySupported(false)
	output = renderBody(t, m)
	if !strings.Contains(output, "click_dog_normalized_query_supported 0\n") {
		t.Errorf("after SetNormalizedQuerySupported(false), output:\n%s", output)
	}
}

func TestMetrics_SetQueryOperationSupported(t *testing.T) {
	m := NewMetrics()

	output := renderBody(t, m)
	if !strings.Contains(output, "click_dog_query_operation_supported 0\n") {
		t.Errorf("default gauge missing, output:\n%s", output)
	}

	m.SetQueryOperationSupported(true)
	output = renderBody(t, m)
	if !strings.Contains(output, "click_dog_query_operation_supported 1\n") {
		t.Errorf("after SetQueryOperationSupported(true), output:\n%s", output)
	}

	m.SetQueryOperationSupported(false)
	output = renderBody(t, m)
	if !strings.Contains(output, "click_dog_query_operation_supported 0\n") {
		t.Errorf("after SetQueryOperationSupported(false), output:\n%s", output)
	}
}
