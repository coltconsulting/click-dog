package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/config"
)

func TestNewWebhookNotifier_Disabled(t *testing.T) {
	w := NewWebhookNotifier(config.WebhookConfig{Enabled: false, URL: "http://example.com"})
	if w != nil {
		t.Error("expected nil when disabled")
	}
}

func TestNewWebhookNotifier_EmptyURL(t *testing.T) {
	w := NewWebhookNotifier(config.WebhookConfig{Enabled: true, URL: ""})
	if w != nil {
		t.Error("expected nil when URL is empty")
	}
}

func TestNewWebhookNotifier_Enabled(t *testing.T) {
	w := NewWebhookNotifier(config.WebhookConfig{
		Enabled:  true,
		URL:      "http://example.com/webhook",
		TimeoutS: 5,
		Events:   []string{"startup", "shutdown"},
	})
	if w == nil {
		t.Fatal("expected non-nil notifier")
	}
	if w.url != "http://example.com/webhook" {
		t.Errorf("url = %q, want http://example.com/webhook", w.url)
	}
	if len(w.events) != 2 {
		t.Errorf("events count = %d, want 2", len(w.events))
	}
}

func TestWebhookNotifier_ShouldFire(t *testing.T) {
	tests := []struct {
		name     string
		events   []string
		event    string
		expected bool
	}{
		{"empty events fires all", nil, "startup", true},
		{"matching event fires", []string{"startup"}, "startup", true},
		{"non-matching event blocked", []string{"startup"}, "shutdown", false},
		{"multiple events matching", []string{"startup", "shutdown"}, "shutdown", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := NewWebhookNotifier(config.WebhookConfig{
				Enabled: true,
				URL:     "http://example.com",
				Events:  tt.events,
			})
			if w.shouldFire(tt.event) != tt.expected {
				t.Errorf("shouldFire(%q) = %v, want %v", tt.event, !tt.expected, tt.expected)
			}
		})
	}
}

func TestWebhookNotifier_NilSafe(t *testing.T) {
	// Calling either notification path on nil should not panic.
	var w *WebhookNotifier
	w.Notify("startup", "test message") // should be no-op
	w.NotifySync(context.Background(), "shutdown", "test message")
}

func TestWebhookNotifier_Send(t *testing.T) {
	var mu sync.Mutex
	var receivedBody string
	var receivedContentType string
	var wg sync.WaitGroup
	wg.Add(1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer wg.Done()
		mu.Lock()
		defer mu.Unlock()
		receivedContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	w := NewWebhookNotifier(config.WebhookConfig{
		Enabled:  true,
		URL:      server.URL,
		TimeoutS: 5,
	})

	w.Notify("startup", "Click-Dog starting up")
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()

	if receivedContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", receivedContentType)
	}

	if receivedBody == "" {
		t.Fatal("expected to receive webhook body")
	}

	var payload map[string]string
	if err := json.Unmarshal([]byte(receivedBody), &payload); err != nil {
		t.Fatalf("failed to parse webhook body: %v", err)
	}

	if !strings.Contains(payload["text"], "Click-Dog starting up") {
		t.Errorf("text = %q, want to contain 'Click-Dog starting up'", payload["text"])
	}
	if !strings.HasPrefix(payload["text"], "[click-dog]") {
		t.Errorf("text = %q, want to start with [click-dog]", payload["text"])
	}
}

func TestWebhookNotifier_DoesNotFollowRedirects(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()

	w := NewWebhookNotifier(config.WebhookConfig{
		Enabled:  true,
		URL:      redirector.URL,
		TimeoutS: 5,
	})
	if w == nil {
		t.Fatal("expected non-nil notifier")
	}

	w.send(context.Background(), "startup", "test")

	if got := targetHits.Load(); got != 0 {
		t.Fatalf("webhook followed redirect to target server (%d hits)", got)
	}
}

func TestWebhookNotifier_EventFilter(t *testing.T) {
	var mu sync.Mutex
	callCount := 0
	var wg sync.WaitGroup
	wg.Add(1) // only the "startup" event should hit the server

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer wg.Done()
		mu.Lock()
		callCount++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	w := NewWebhookNotifier(config.WebhookConfig{
		Enabled:  true,
		URL:      server.URL,
		TimeoutS: 5,
		Events:   []string{"startup"},
	})

	// Should fire
	w.Notify("startup", "test")
	// Should NOT fire (filtered client-side, never hits goroutine/server)
	w.Notify("shutdown", "test")
	w.Notify("circuit_breaker_opened", "test")

	wg.Wait()

	mu.Lock()
	defer mu.Unlock()

	if callCount != 1 {
		t.Errorf("expected 1 webhook call (only startup), got %d", callCount)
	}
}

func TestWebhookNotifier_ServerError(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer wg.Done()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	w := NewWebhookNotifier(config.WebhookConfig{
		Enabled:  true,
		URL:      server.URL,
		TimeoutS: 5,
	})

	// Should not panic on server error
	w.Notify("startup", "test")
	wg.Wait()
}

func TestWebhookNotifier_NotifySyncWaitsForDelivery(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseResponse := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseResponse) }) }

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		<-releaseResponse
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	defer release()

	w := NewWebhookNotifier(config.WebhookConfig{
		Enabled:  true,
		URL:      server.URL,
		TimeoutS: 5,
	})

	returned := make(chan struct{})
	go func() {
		w.NotifySync(context.Background(), EventShutdown, "Click-Dog shutting down")
		close(returned)
	}()

	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("synchronous webhook request did not reach server")
	}

	select {
	case <-returned:
		t.Fatal("NotifySync returned before the server completed the delivery attempt")
	case <-time.After(25 * time.Millisecond):
	}

	release()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("NotifySync did not return after the server responded")
	}
}

func TestWebhookNotifier_NotifySyncHonorsContextDeadline(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseResponse := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		<-releaseResponse
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	defer close(releaseResponse)

	w := NewWebhookNotifier(config.WebhookConfig{
		Enabled:  true,
		URL:      server.URL,
		TimeoutS: 5,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	startedAt := time.Now()
	w.NotifySync(ctx, EventShutdown, "Click-Dog shutting down")
	elapsed := time.Since(startedAt)

	select {
	case <-requestStarted:
	default:
		t.Fatal("expected a delivery attempt before the context deadline")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("NotifySync took %s, want a bounded return near the 50ms context deadline", elapsed)
	}
}

func TestWebhookNotifier_NotifyKeepsBoundedQueue(t *testing.T) {
	requestStarted := make(chan struct{}, maxConcurrentWebhooks)
	releaseRequests := make(chan struct{})
	var requests sync.WaitGroup
	requests.Add(maxConcurrentWebhooks)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer requests.Done()
		requestStarted <- struct{}{}
		<-releaseRequests
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	w := NewWebhookNotifier(config.WebhookConfig{
		Enabled:  true,
		URL:      server.URL,
		TimeoutS: 5,
	})

	for range maxConcurrentWebhooks {
		w.Notify(EventStartup, "test")
	}
	for range maxConcurrentWebhooks {
		select {
		case <-requestStarted:
		case <-time.After(time.Second):
			close(releaseRequests)
			t.Fatal("asynchronous webhook request did not reach server")
		}
	}

	w.Notify(EventStartup, "dropped")
	if got := w.dropped.Load(); got != 1 {
		close(releaseRequests)
		t.Fatalf("dropped notifications = %d, want 1", got)
	}

	close(releaseRequests)
	requests.Wait()
}
