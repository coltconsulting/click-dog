package webhook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/coltconsulting/click-dog/internal/clicklog"
	"github.com/coltconsulting/click-dog/internal/config"
)

// WebhookNotifier sends HTTP POST notifications on critical events.
// The payload is Slack incoming webhook compatible: {"text": "..."}.
type WebhookNotifier struct {
	url     string
	client  *http.Client
	events  map[string]bool // which events to fire on; empty = all
	sem     chan struct{}   // bounds concurrent send goroutines
	dropped atomic.Int64    // cumulative count of dropped notifications
}

// WebhookEvent names used to filter which notifications are sent.
const (
	EventCircuitBreakerOpened = "circuit_breaker_opened"
	EventCircuitBreakerClosed = "circuit_breaker_closed"
	EventBackfillComplete     = "backfill_complete"
	EventBackfillFailed       = "backfill_failed"
	EventErrorSpike           = "error_spike"
	EventStartup              = "startup"
	EventShutdown             = "shutdown"

	// maxConcurrentWebhooks limits the number of in-flight webhook sends.
	maxConcurrentWebhooks = 4
)

// NewWebhookNotifier creates a notifier from config.
// If the config is not enabled or URL is empty, returns nil.
func NewWebhookNotifier(cfg config.WebhookConfig) *WebhookNotifier {
	if !cfg.Enabled || cfg.URL == "" {
		return nil
	}

	timeout := time.Duration(cfg.TimeoutS) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	events := make(map[string]bool)
	for _, e := range cfg.Events {
		events[e] = true
	}

	return &WebhookNotifier{
		url: cfg.URL,
		client: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		events: events,
		sem:    make(chan struct{}, maxConcurrentWebhooks),
	}
}

// shouldFire returns true if the event should trigger a notification.
// If no events filter is configured, all events fire.
func (w *WebhookNotifier) shouldFire(event string) bool {
	if len(w.events) == 0 {
		return true
	}
	return w.events[event]
}

// Notify sends a webhook notification for the given event.
// Non-blocking: fires in a goroutine so it never delays the main loop.
// Bounded by a semaphore to prevent goroutine accumulation during error spikes.
func (w *WebhookNotifier) Notify(event, message string) {
	if w == nil || !w.shouldFire(event) {
		return
	}
	select {
	case w.sem <- struct{}{}:
		go func() {
			defer func() { <-w.sem }()
			w.send(event, message)
		}()
	default:
		total := w.dropped.Add(1)
		clicklog.Error("Webhook: dropping %s notification, send queue full (%d total dropped)", event, total)
	}
}

// send performs the actual HTTP POST. Errors are logged, not returned.
func (w *WebhookNotifier) send(event, message string) {
	payload := map[string]string{
		"text": fmt.Sprintf("[click-dog] %s", message),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		clicklog.Error("Webhook: failed to marshal payload: %v", err)
		return
	}

	resp, err := w.client.Post(w.url, "application/json", bytes.NewReader(body))
	if err != nil {
		clicklog.Error("Webhook: POST failed for event %s: %v", event, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		clicklog.Warn("Webhook: unexpected status %d for event %s", resp.StatusCode, event)
	}
}
