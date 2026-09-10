package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coltconsulting/click-dog/internal/analysis"
	"github.com/coltconsulting/click-dog/internal/clicklog"
	"github.com/coltconsulting/click-dog/internal/config"
)

// WebhookNotifier sends HTTP POST notifications for configured operational
// and explicit analysis events.
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
	EventAnalysisFindings     = "analysis_findings"

	// maxConcurrentWebhooks limits the number of in-flight webhook sends.
	maxConcurrentWebhooks = 4

	// maxWebhookResponseBytes bounds response draining. Response content is
	// never surfaced because arbitrary endpoints can echo secrets.
	maxWebhookResponseBytes int64 = 64 << 10
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

// Handles reports whether this configured notifier is an eligible destination
// for event. It does not perform a network call.
func (w *WebhookNotifier) Handles(event string) bool {
	return w != nil && w.shouldFire(event)
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
			if err := w.send(context.Background(), event, message); err != nil {
				clicklog.Error("Webhook: %v", err)
			}
		}()
	default:
		total := w.dropped.Add(1)
		clicklog.Error("Webhook: dropping %s notification, send queue full (%d total dropped)", event, total)
	}
}

// NotifySync sends a webhook notification and waits for the delivery attempt
// to finish. The request remains bounded by both ctx and the client's configured
// timeout. Unlike Notify, this path is not dropped when the asynchronous send
// queue is full, so it is suitable for terminal notifications from shutdown
// and short-lived one-shot commands.
func (w *WebhookNotifier) NotifySync(ctx context.Context, event, message string) {
	if err := w.NotifySyncResult(ctx, event, message); err != nil {
		clicklog.Error("Webhook: %v", err)
	}
}

// NotifySyncResult sends one notification and returns a secret-safe delivery
// result. The short-lived analyze command uses this path so delivery failure
// can take precedence over policy exit status without racing process exit.
func (w *WebhookNotifier) NotifySyncResult(ctx context.Context, event, message string) error {
	if w == nil || !w.shouldFire(event) {
		return nil
	}
	return w.send(ctx, event, message)
}

// NotifyAnalysis sends the Slack-compatible rendering of the shared bounded
// notification DTO.
func (w *WebhookNotifier) NotifyAnalysis(ctx context.Context, summary analysis.NotificationSummary) error {
	return w.NotifySyncResult(ctx, EventAnalysisFindings, renderAnalysisSummary(summary))
}

func renderAnalysisSummary(summary analysis.NotificationSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Query analysis findings: %d eligible; total=%d critical/%d warning/%d info; new=%d critical/%d warning/%d info; highest=%s; window=%s..%s; schemas=%s/%s.",
		summary.EligibleFindingCount,
		summary.TotalFindingCounts.Critical,
		summary.TotalFindingCounts.Warning,
		summary.TotalFindingCounts.Info,
		summary.NewFindingCounts.Critical,
		summary.NewFindingCounts.Warning,
		summary.NewFindingCounts.Info,
		summary.HighestEligibleSeverity,
		summary.Window.Start.UTC().Format(time.RFC3339),
		summary.Window.End.UTC().Format(time.RFC3339),
		summary.ReportSchemaVersion,
		summary.SchemaVersion)
	for _, condition := range summary.Conditions {
		fmt.Fprintf(&b, "\n- %s %s %s", strings.ToUpper(string(condition.Severity)), condition.Analyzer, condition.ConditionKey)
		if condition.FamilyID != "" {
			fmt.Fprintf(&b, " family=%s", condition.FamilyID)
		}
		if len(condition.NormalizedQueryHashes) > 0 {
			fmt.Fprintf(&b, " hashes=%s", strings.Join(condition.NormalizedQueryHashes, ","))
		}
		if condition.HashesTruncated > 0 {
			fmt.Fprintf(&b, " hashes_omitted=%d", condition.HashesTruncated)
		}
		if condition.New {
			b.WriteString(" new=true")
		}
	}
	if summary.TruncatedFindingCount > 0 {
		fmt.Fprintf(&b, "\n- %d additional eligible conditions omitted by payload cap", summary.TruncatedFindingCount)
	}
	b.WriteString("\nReview the local JSON report for evidence. Use click-dog analyze trace with a listed normalized query hash for drilldown.")
	return b.String()
}

// send performs the actual HTTP POST and returns only secret-safe errors.
func (w *WebhookNotifier) send(ctx context.Context, event, message string) error {
	payload := map[string]string{
		"text": fmt.Sprintf("[click-dog] %s", message),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create request for event %s", event)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("delivery timed out for event %s: %w", event, ctx.Err())
		}
		return fmt.Errorf("delivery failed for event %s", event)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxWebhookResponseBytes))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d for event %s", resp.StatusCode, event)
	}
	return nil
}
