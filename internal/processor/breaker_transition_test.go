package processor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/metrics"
	"github.com/coltconsulting/click-dog/internal/model"
	"github.com/coltconsulting/click-dog/internal/resilience"
	"github.com/coltconsulting/click-dog/internal/webhook"
)

// webhookRecorder collects the text of every webhook POST it receives.
type webhookRecorder struct {
	texts chan string
}

func newWebhookRecorder(t *testing.T) (*webhookRecorder, *webhook.WebhookNotifier) {
	t.Helper()
	rec := &webhookRecorder{texts: make(chan string, 16)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		rec.texts <- payload.Text
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	wh := webhook.NewWebhookNotifier(config.WebhookConfig{Enabled: true, URL: server.URL, TimeoutS: 2})
	return rec, wh
}

// wait returns the sorted texts of the next n webhooks.
func (r *webhookRecorder) wait(t *testing.T, n int) []string {
	t.Helper()
	var got []string
	deadline := time.After(2 * time.Second)
	for len(got) < n {
		select {
		case text := <-r.texts:
			got = append(got, text)
		case <-deadline:
			t.Fatalf("received %d webhooks %q, want %d", len(got), got, n)
		}
	}
	sort.Strings(got)
	return got
}

// testClock is a resilience.Clock the test moves by hand.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// TestPipeline_CanaryFailureThatOpensTheBreakerIsPublished pins that a canary
// failure which opens the breaker is published like any other opening: the
// state gauge that /readyz and /status read says open, and the opened and
// error-spike webhooks fire. Before the fix the log said the breaker opened
// while /readyz stayed 200 and /status and the gauge said closed.
func TestPipeline_CanaryFailureThatOpensTheBreakerIsPublished(t *testing.T) {
	tests := []struct {
		name       string
		querier    *mockCanaryQuerier
		exporter   *mockExporter
		halfOpen   bool
		wantOpened string // the opened webhook's text
	}{
		{
			name:       "canary query fails",
			querier:    &mockCanaryQuerier{err: errors.New("clickhouse unreachable")},
			exporter:   &mockExporter{},
			wantOpened: "[click-dog] Circuit breaker opened after 1 failures (canary query failed)",
		},
		{
			name:    "canary span export fails",
			querier: &mockCanaryQuerier{result: model.CanaryResult{Count: 1, LongQueriesExist: true}},
			exporter: &mockExporter{exportSpansFunc: func(context.Context, []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
				return nil, errors.New("collector down")
			}},
			wantOpened: "[click-dog] Circuit breaker opened after 1 failures (canary span export failed)",
		},
		{
			name:     "canary fails while half-open",
			querier:  &mockCanaryQuerier{err: errors.New("clickhouse unreachable")},
			exporter: &mockExporter{},
			halfOpen: true,
			// The half-open probe does not reset the failure count.
			wantOpened: "[click-dog] Circuit breaker opened after 2 failures (canary query failed)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := &testClock{now: time.Unix(1_700_000_000, 0)}
			cb := resilience.NewCircuitBreakerWithClock(config.CircuitBreakerConfig{
				FailureThreshold: 1,
				SuccessThreshold: 1,
				ResetTimeoutS:    60,
			}, clock)
			// One failure at factor 3 lifts the interval to 30s, over twice the
			// 10s base, so the cycle runs the canary instead of a fetch.
			poller := resilience.NewAdaptivePoller(10*time.Second, config.BackoffConfig{MaxIntervalS: 300, BackoffFactor: 3.0})
			poller.RecordFailure()

			rec, wh := newWebhookRecorder(t)
			m := metrics.NewMetrics()
			cfg := &config.Config{Monitor: config.MonitorConfig{
				CheckIntervalS: 10,
				Canary:         config.CanaryConfig{Enabled: true, ThresholdDurationMs: 60000},
			}}
			qf, _ := filter.NewQueryFilter(cfg.Filters)
			p := &Pipeline{
				Reader:         &mockLiveReader{healthy: true},
				Exporter:       tt.exporter,
				Filter:         qf,
				Config:         cfg,
				CircuitBreaker: cb,
				Poller:         poller,
				CanaryQuerier:  tt.querier,
				Metrics:        m,
				Webhook:        wh,
			}

			if tt.halfOpen {
				// Open the breaker through a published path, then let the
				// reset timeout pass so the next cycle probes half-open.
				recordBreakerFailure(cb, m, wh, "cycle failed")
				rec.wait(t, 2)
				clock.advance(61 * time.Second)
			}

			err := p.Process(context.Background())
			if err == nil || errors.Is(err, ErrCanaryRan) {
				t.Fatalf("Process = %v, want the canary's failure", err)
			}
			if tt.halfOpen && tt.querier.calls != 1 {
				t.Fatalf("canary calls = %d, want the half-open cycle to run the canary", tt.querier.calls)
			}
			if got := cb.State(); got != resilience.CircuitOpen {
				t.Fatalf("breaker state = %v, want open", got)
			}
			// /readyz and /status read this field through the health adapter.
			if got := m.Snapshot().CircuitBreakerState; got != "open" {
				t.Errorf("published breaker state = %q, want open", got)
			}
			mustContain(t, scrapeMetrics(t, m), "click_dog_circuit_breaker_state 2\n")

			texts := rec.wait(t, 2)
			if texts[0] != tt.wantOpened {
				t.Errorf("opened webhook = %q, want %q", texts[0], tt.wantOpened)
			}
			if !strings.HasPrefix(texts[1], "[click-dog] Error spike: ") {
				t.Errorf("error-spike webhook = %q", texts[1])
			}
		})
	}
}

// TestRecordBreakerSuccess_PublishesClosing pins the closing side of the shared
// helpers: a success that closes a half-open breaker sets the gauge back to
// closed and sends circuit_breaker_closed, and a success on a closed breaker
// publishes nothing.
func TestRecordBreakerSuccess_PublishesClosing(t *testing.T) {
	clock := &testClock{now: time.Unix(1_700_000_000, 0)}
	cb := resilience.NewCircuitBreakerWithClock(config.CircuitBreakerConfig{
		FailureThreshold: 1,
		SuccessThreshold: 1,
		ResetTimeoutS:    60,
	}, clock)
	rec, wh := newWebhookRecorder(t)
	m := metrics.NewMetrics()

	recordBreakerFailure(cb, m, wh, "cycle failed")
	rec.wait(t, 2)
	clock.advance(61 * time.Second)
	if !cb.Allow() {
		t.Fatal("breaker did not move to half-open after the reset timeout")
	}
	m.SetCircuitBreakerState("half_open")

	recordBreakerSuccess(cb, m, wh)
	if got := m.Snapshot().CircuitBreakerState; got != "closed" {
		t.Errorf("published breaker state = %q, want closed", got)
	}
	if texts := rec.wait(t, 1); !strings.Contains(texts[0], "Circuit breaker closed") {
		t.Errorf("closed webhook = %q", texts[0])
	}

	recordBreakerSuccess(cb, m, wh)
	select {
	case text := <-rec.texts:
		t.Errorf("success on a closed breaker sent a webhook: %q", text)
	case <-time.After(50 * time.Millisecond):
	}
}
