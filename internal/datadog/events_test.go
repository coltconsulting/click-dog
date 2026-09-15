package datadog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/analysis"
	"github.com/coltconsulting/click-dog/internal/config"
)

func TestNewEventClient_EndpointSiteAndAuthenticationValidation(t *testing.T) {
	cfg := testEventConfig()
	cfg.Site = "DATADOGHQ.EU"
	client, err := NewEventClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if client.endpoint != "https://event-management-intake.datadoghq.eu/api/v2/events" {
		t.Fatalf("endpoint = %q", client.endpoint)
	}

	tests := []struct {
		name   string
		mutate func(*config.DatadogEventsConfig)
	}{
		{name: "site injection", mutate: func(c *config.DatadogEventsConfig) { c.Site = "datadoghq.com@attacker.example" }},
		{name: "missing api key", mutate: func(c *config.DatadogEventsConfig) { c.APIKey = "" }},
		{name: "blank api key", mutate: func(c *config.DatadogEventsConfig) { c.APIKey = "  " }},
		{name: "missing app key", mutate: func(c *config.DatadogEventsConfig) { c.ApplicationKey = "" }},
		{name: "unbounded timeout", mutate: func(c *config.DatadogEventsConfig) { c.TimeoutS = config.MaxDatadogEventsTimeoutS + 1 }},
		{name: "high cardinality environment", mutate: func(c *config.DatadogEventsConfig) { c.Environment = "production west" }},
		{name: "missing service", mutate: func(c *config.DatadogEventsConfig) { c.Service = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			invalid := testEventConfig()
			tt.mutate(&invalid)
			if _, err := NewEventClient(invalid); err == nil {
				t.Fatal("expected constructor error")
			}
		})
	}
}

func TestEventClient_SendV2PayloadAndHeaders(t *testing.T) {
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v2/events" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("DD-API-KEY") != "api-secret" || r.Header.Get("DD-APPLICATION-KEY") != "app-secret" {
			t.Errorf("authentication headers missing")
		}
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client, err := NewEventClient(testEventConfig())
	if err != nil {
		t.Fatal(err)
	}
	client.endpoint = server.URL + "/api/v2/events"
	summary := testNotificationSummary(t)
	if err := client.Send(context.Background(), summary); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(received, &payload); err != nil {
		t.Fatalf("payload JSON: %v", err)
	}
	data := payload["data"].(map[string]any)
	if data["type"] != "event" {
		t.Fatalf("data.type = %v", data["type"])
	}
	attributes := data["attributes"].(map[string]any)
	if attributes["category"] != "alert" || attributes["integration_id"] != "custom-events" {
		t.Fatalf("event attributes = %+v", attributes)
	}
	alertAttributes := attributes["attributes"].(map[string]any)
	if alertAttributes["status"] != "error" || alertAttributes["priority"] != "1" {
		t.Fatalf("critical alert attributes = %+v", alertAttributes)
	}
	custom := alertAttributes["custom"].(map[string]any)
	for key, want := range map[string]any{
		"source":                      "click-dog",
		"event_type":                  "analysis_findings",
		"report_schema_version":       analysis.ReportSchemaVersion,
		"notification_schema_version": analysis.NotificationSchemaVersion,
		"window_start":                "1970-01-01T00:01:40Z",
		"window_end":                  "1970-01-01T00:03:20Z",
		"highest_eligible_severity":   "critical",
		"environment":                 "production",
		"service":                     "click-dog-monitor",
	} {
		if custom[key] != want {
			t.Errorf("custom[%s] = %v, want %v", key, custom[key], want)
		}
	}
	for _, key := range []string{"total_critical", "total_warning", "total_info", "new_critical", "new_warning", "new_info", "conditions"} {
		if _, ok := custom[key]; !ok {
			t.Errorf("custom field %q missing", key)
		}
	}
	if message, _ := attributes["message"].(string); !strings.Contains(message, "new critical=0 warning=0 info=0") || !strings.Contains(message, "local click-dog JSON report") {
		t.Fatalf("event message = %q", message)
	}
	tags := fmt.Sprint(attributes["tags"])
	for _, want := range []string{"source:click-dog", "event_type:analysis_findings", "environment:production", "service:click-dog-monitor", "highest_eligible_severity:critical"} {
		if !strings.Contains(tags, want) {
			t.Errorf("event tags missing %q: %s", want, tags)
		}
	}
}

func TestBuildEventPayload_WarningStatusAndPriority(t *testing.T) {
	summary := testNotificationSummary(t)
	summary.HighestEligibleSeverity = analysis.SeverityWarning
	payload := buildEventPayload(summary, "test", "click-dog")
	alert := payload.Data.Attributes.Attributes
	if alert.Status != "warn" || alert.Priority != "3" {
		t.Fatalf("warning alert = status %q priority %q", alert.Status, alert.Priority)
	}
}

func TestEventClient_PayloadPrivacy(t *testing.T) {
	const (
		secretSQL  = "SELECT secret_card FROM payments"
		secretPath = "/etc/click-dog/private.yaml"
		secretUser = "db-admin"
	)
	report := analysis.AnalysisReport{
		SchemaVersion: analysis.ReportSchemaVersion,
		Window:        analysis.AnalysisWindow{Start: time.Unix(100, 0).UTC(), End: time.Unix(200, 0).UTC()},
		Config:        analysis.ReportConfig{ConfigPath: secretPath},
		Findings: []analysis.Finding{{
			ID:                    "resource_hog:family:windowed",
			Analyzer:              "resource_hog",
			ConditionScope:        "family",
			Severity:              analysis.SeverityCritical,
			FamilyID:              "qf_safe",
			NormalizedQueryHashes: []string{"100"},
			RepresentativeQuery:   secretSQL,
			Title:                 secretUser,
			Summary:               "api_key=payload-secret",
			Evidence:              map[string]any{"host": "customer-db.internal"},
			Recommendation:        "webhook=https://hooks.example/private",
		}},
	}
	summary, err := analysis.BuildNotificationSummary(report, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(buildEventPayload(summary, "production", "click-dog-monitor"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{secretSQL, secretPath, secretUser, "payload-secret", "customer-db.internal", "hooks.example"} {
		if bytes.Contains(payload, []byte(forbidden)) {
			t.Fatalf("Datadog payload leaked %q: %s", forbidden, payload)
		}
	}
}

func TestEventClient_RejectsRedirectAndDoesNotForwardCredentials(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		if r.Header.Get("DD-API-KEY") != "" || r.Header.Get("DD-APPLICATION-KEY") != "" {
			t.Error("redirect target received credentials")
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	client, _ := NewEventClient(testEventConfig())
	client.endpoint = redirector.URL
	err := client.Send(context.Background(), testNotificationSummary(t))
	if err == nil || !strings.Contains(err.Error(), "HTTP 307") {
		t.Fatalf("redirect error = %v", err)
	}
	if targetHits.Load() != 0 {
		t.Fatal("client followed redirect")
	}
}

func TestEventClient_TimeoutAndSecretSafeErrors(t *testing.T) {
	releaseHandler := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-releaseHandler:
		}
	}))
	t.Cleanup(func() {
		close(releaseHandler)
		server.Close()
	})
	client, _ := NewEventClient(testEventConfig())
	client.endpoint = server.URL + "/api-secret-in-url"
	client.http.Timeout = 30 * time.Millisecond
	err := client.Send(context.Background(), testNotificationSummary(t))
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("timeout error = %v", err)
	}
	for _, secret := range []string{"api-secret", "app-secret", "api-secret-in-url", server.URL} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked %q: %v", secret, err)
		}
	}

	client.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("transport echoed api-secret app-secret %s", client.endpoint)
	})
	err = client.Send(context.Background(), testNotificationSummary(t))
	if err == nil {
		t.Fatal("expected transport error")
	}
	for _, secret := range []string{"api-secret", "app-secret", client.endpoint} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("transport error leaked %q: %v", secret, err)
		}
	}
}

func TestEventClient_Non2xxAndResponseReadCap(t *testing.T) {
	client, _ := NewEventClient(testEventConfig())
	client.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Body:       io.NopCloser(strings.NewReader("credentials rejected: api-secret app-secret")),
			Header:     make(http.Header),
		}, nil
	})
	err := client.Send(context.Background(), testNotificationSummary(t))
	if err == nil || err.Error() != "unexpected HTTP 401 from Datadog Events API" {
		t.Fatalf("non-2xx error = %v", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("response body leaked: %v", err)
	}

	reader := &countingReader{remaining: maxEventResponseBytes * 4}
	client.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusAccepted, Body: reader, Header: make(http.Header)}, nil
	})
	if err := client.Send(context.Background(), testNotificationSummary(t)); err != nil {
		t.Fatal(err)
	}
	if reader.read > maxEventResponseBytes {
		t.Fatalf("response bytes read = %d, cap = %d", reader.read, maxEventResponseBytes)
	}
	if !reader.closed {
		t.Fatal("response body was not closed")
	}
}

func testEventConfig() config.DatadogEventsConfig {
	return config.DatadogEventsConfig{
		Enabled:        true,
		Site:           "datadoghq.com",
		APIKey:         "api-secret",
		ApplicationKey: "app-secret",
		TimeoutS:       10,
		Environment:    "production",
		Service:        "click-dog-monitor",
	}
}

func testNotificationSummary(t *testing.T) analysis.NotificationSummary {
	t.Helper()
	report := analysis.AnalysisReport{
		SchemaVersion: analysis.ReportSchemaVersion,
		Window:        analysis.AnalysisWindow{Start: time.Unix(100, 0).UTC(), End: time.Unix(200, 0).UTC()},
		Findings: []analysis.Finding{{
			ID:                    "resource_hog:family:windowed",
			Analyzer:              "resource_hog",
			ConditionScope:        "family",
			Severity:              analysis.SeverityCritical,
			FamilyID:              "qf_safe",
			NormalizedQueryHashes: []string{"100"},
			Evidence:              map[string]any{},
		}},
	}
	summary, err := analysis.BuildNotificationSummary(report, nil)
	if err != nil {
		t.Fatal(err)
	}
	return summary
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type countingReader struct {
	remaining int64
	read      int64
	closed    bool
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	for i := range p {
		p[i] = 'x'
	}
	r.remaining -= int64(len(p))
	r.read += int64(len(p))
	return len(p), nil
}

func (r *countingReader) Close() error {
	r.closed = true
	return nil
}
