package export

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/model"
)

var testTraceID = uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")

func TestSplunkHEC_ExportSpans(t *testing.T) {
	var receivedBody string
	var receivedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		buf, _ := io.ReadAll(r.Body)
		receivedBody = string(buf)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"text":"Success","code":0}`))
	}))
	defer server.Close()

	exp, err := NewSplunkHECExporter(config.SplunkHECConfig{
		Endpoint: server.URL,
		Token:    "test-token-123",
		Index:    "main",
		Source:   "click-dog-test",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := []model.OpenTelemetrySpan{
		{
			SpanID:        42,
			TraceID:       testTraceID,
			OperationName: "test.op",
			Kind:          "INTERNAL",
			Hostname:      "node1",
			StartTimeUs:   1000000,
			FinishTimeUs:  2000000,
			Attributes:    map[string]string{"key": "value"},
		},
	}

	result, err := exp.ExportSpans(context.Background(), spans)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Accepted) != 1 || result.Accepted[0] != (model.SpanKey{TraceID: testTraceID, SpanID: 42}) {
		t.Errorf("expected [{%s,42}], got %v", testTraceID, result.Accepted)
	}
	if result.TotalSent != 1 || result.TotalAccepted != 1 {
		t.Errorf("result counts = sent %d accepted %d, want 1/1", result.TotalSent, result.TotalAccepted)
	}
	if receivedAuth != "Splunk test-token-123" {
		t.Errorf("auth header = %q, want 'Splunk test-token-123'", receivedAuth)
	}

	// Verify JSON payload
	var ev hecEvent
	if err := json.Unmarshal([]byte(receivedBody), &ev); err != nil {
		t.Fatalf("failed to parse HEC event: %v", err)
	}
	if ev.Source != "click-dog-test" {
		t.Errorf("source = %q, want click-dog-test", ev.Source)
	}
	if ev.Index != "main" {
		t.Errorf("index = %q, want main", ev.Index)
	}
	eventMap, ok := ev.Event.(map[string]interface{})
	if !ok {
		t.Fatalf("event is not a map")
	}
	if eventMap["operation_name"] != "test.op" {
		t.Errorf("operation_name = %v, want test.op", eventMap["operation_name"])
	}
}

func TestSplunkHEC_ExportSpans_LiveSourceAttribute(t *testing.T) {
	tests := []struct {
		name       string
		attributes map[string]string
		wantSource string
	}{
		{
			name: "supplied source is authoritative",
			attributes: map[string]string{
				liveSpanSourceKey: "test-span",
				"custom":          "preserved",
			},
			wantSource: "test-span",
		},
		{
			name:       "absent source defaults to span log",
			attributes: map[string]string{"custom": "preserved"},
			wantSource: liveSpanDefaultSource,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var receivedBody string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				buf, _ := io.ReadAll(r.Body)
				receivedBody = string(buf)
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			exp, err := NewSplunkHECExporter(config.SplunkHECConfig{
				Endpoint: server.URL,
				Token:    "test",
			})
			if err != nil {
				t.Fatalf("NewSplunkHECExporter returned err: %v", err)
			}

			span := model.OpenTelemetrySpan{
				SpanID:      42,
				TraceID:     testTraceID,
				StartTimeUs: 1_000_000,
				Attributes:  tt.attributes,
			}
			if _, err := exp.ExportSpans(context.Background(), []model.OpenTelemetrySpan{span}); err != nil {
				t.Fatalf("ExportSpans returned err: %v", err)
			}

			var ev hecEvent
			if err := json.Unmarshal([]byte(receivedBody), &ev); err != nil {
				t.Fatalf("failed to parse HEC event: %v", err)
			}
			eventMap, ok := ev.Event.(map[string]interface{})
			if !ok {
				t.Fatalf("event is %T, want map", ev.Event)
			}
			if got := eventMap["click_dog_source"]; got != tt.wantSource {
				t.Fatalf("click_dog_source = %v, want %q", got, tt.wantSource)
			}
			attributes, ok := eventMap["attributes"].(map[string]interface{})
			if !ok {
				t.Fatalf("attributes is %T, want map", eventMap["attributes"])
			}
			if _, ok := attributes[liveSpanSourceKey]; ok {
				t.Fatalf("nested attributes unexpectedly contain %q", liveSpanSourceKey)
			}
			if got := attributes["custom"]; got != "preserved" {
				t.Fatalf("custom = %v, want preserved", got)
			}
		})
	}
}

func TestSplunkHEC_ExportSpans_LiveDBStatementTruncation(t *testing.T) {
	tests := []struct {
		name           string
		maxQueryLength int
		query          string
		want           string
	}{
		{name: "boundary", maxQueryLength: 8, query: "SELECT 1", want: "SELECT 1"},
		{name: "over limit", maxQueryLength: 8, query: "SELECT 12", want: "SELECT 1..."},
		{name: "no limit", maxQueryLength: 0, query: "SELECT 12", want: "SELECT 12"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var receivedBody string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				buf, _ := io.ReadAll(r.Body)
				receivedBody = string(buf)
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			exp, err := NewSplunkHECExporter(config.SplunkHECConfig{
				Endpoint:       server.URL,
				Token:          "test",
				MaxQueryLength: tt.maxQueryLength,
			})
			if err != nil {
				t.Fatalf("NewSplunkHECExporter returned err: %v", err)
			}
			// Config loading supplies the production default. Setting the field here
			// exercises the serializer's documented non-positive no-limit contract.
			exp.maxQueryLength = tt.maxQueryLength

			span := model.OpenTelemetrySpan{
				SpanID:      42,
				TraceID:     testTraceID,
				StartTimeUs: 1_000_000,
				Attributes: map[string]string{
					liveSpanQueryKey: tt.query,
					"custom":         "unchanged-long-value",
				},
			}
			if _, err := exp.ExportSpans(context.Background(), []model.OpenTelemetrySpan{span}); err != nil {
				t.Fatalf("ExportSpans returned err: %v", err)
			}

			var ev hecEvent
			if err := json.Unmarshal([]byte(receivedBody), &ev); err != nil {
				t.Fatalf("failed to parse HEC event: %v", err)
			}
			eventMap, ok := ev.Event.(map[string]interface{})
			if !ok {
				t.Fatalf("event is %T, want map", ev.Event)
			}
			attributes, ok := eventMap["attributes"].(map[string]interface{})
			if !ok {
				t.Fatalf("attributes is %T, want map", eventMap["attributes"])
			}
			if got := attributes[liveSpanQueryKey]; got != tt.want {
				t.Fatalf("%s = %v, want %q", liveSpanQueryKey, got, tt.want)
			}
			if got := attributes["custom"]; got != "unchanged-long-value" {
				t.Fatalf("custom = %v, want unchanged-long-value", got)
			}
			if got := span.Attributes[liveSpanQueryKey]; got != tt.query {
				t.Fatalf("input %s mutated to %q, want %q", liveSpanQueryKey, got, tt.query)
			}
		})
	}
}

func TestSplunkHEC_ExportSpans_Empty(t *testing.T) {
	exp, err := NewSplunkHECExporter(config.SplunkHECConfig{
		Endpoint: "http://localhost:9999",
		Token:    "test",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	result, err := exp.ExportSpans(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Accepted != nil {
		t.Errorf("expected nil keys for empty input, got %v", result.Accepted)
	}
}

func TestSplunkHEC_AuthFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"text":"Invalid token","code":4}`))
	}))
	defer server.Close()

	exp, err := NewSplunkHECExporter(config.SplunkHECConfig{
		Endpoint: server.URL,
		Token:    "bad-token",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := []model.OpenTelemetrySpan{{SpanID: 1, TraceID: testTraceID}}
	result, err := exp.ExportSpans(context.Background(), spans)
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should mention 403, got: %v", err)
	}
	if result.TotalSent != 1 || result.TotalAccepted != 0 {
		t.Fatalf("result counts = sent %d accepted %d, want 1/0", result.TotalSent, result.TotalAccepted)
	}
	if len(result.Sinks) != 1 {
		t.Fatalf("len(Sinks) = %d, want 1", len(result.Sinks))
	}
	if result.Sinks[0].Name != "splunk_hec" || result.Sinks[0].Sent != 1 || result.Sinks[0].Accepted != 0 || result.Sinks[0].Error == nil {
		t.Fatalf("sink status = %+v, want splunk_hec sent=1 accepted=0 with error", result.Sinks[0])
	}
}

func TestSplunkHEC_ServiceUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	exp, err := NewSplunkHECExporter(config.SplunkHECConfig{
		Endpoint: server.URL,
		Token:    "test",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := []model.OpenTelemetrySpan{{SpanID: 1, TraceID: testTraceID}}
	result, err := exp.ExportSpans(context.Background(), spans)
	if err == nil {
		t.Fatal("expected error for 503 response")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error should mention 503, got: %v", err)
	}
	if result.TotalSent != 1 || result.TotalAccepted != 0 {
		t.Fatalf("result counts = sent %d accepted %d, want 1/0", result.TotalSent, result.TotalAccepted)
	}
	if len(result.Sinks) != 1 {
		t.Fatalf("len(Sinks) = %d, want 1", len(result.Sinks))
	}
	if result.Sinks[0].Name != "splunk_hec" || result.Sinks[0].Sent != 1 || result.Sinks[0].Accepted != 0 || result.Sinks[0].Error == nil {
		t.Fatalf("sink status = %+v, want splunk_hec sent=1 accepted=0 with error", result.Sinks[0])
	}
}

func TestSplunkHEC_ExportQuery(t *testing.T) {
	var receivedBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		receivedBody = string(buf)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	exp, err := NewSplunkHECExporter(config.SplunkHECConfig{
		Endpoint: server.URL,
		Token:    "test",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	result, err := exp.ExportQuery(context.Background(), model.QueryLog{
		QueryID:             "q-123",
		QueryKind:           "QueryFinish",
		Query:               "SELECT 1",
		NormalizedQueryHash: 123456,
		NormalizedQuery:     "SELECT ?",
		QueryDurationMs:     500,
		User:                "default",
		EventTime:           time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.TotalSent != 1 || result.TotalAccepted != 1 {
		t.Fatalf("result counts = sent %d accepted %d, want 1/1", result.TotalSent, result.TotalAccepted)
	}
	if len(result.Sinks) != 1 {
		t.Fatalf("len(Sinks) = %d, want 1", len(result.Sinks))
	}
	if result.Sinks[0].Name != "splunk_hec" || result.Sinks[0].Sent != 1 || result.Sinks[0].Accepted != 1 || result.Sinks[0].Error != nil {
		t.Fatalf("sink status = %+v, want splunk_hec sent=1 accepted=1 with nil error", result.Sinks[0])
	}

	var ev hecEvent
	if err := json.Unmarshal([]byte(receivedBody), &ev); err != nil {
		t.Fatalf("failed to parse HEC event: %v", err)
	}
	eventMap, ok := ev.Event.(map[string]interface{})
	if !ok {
		t.Fatalf("event is not a map")
	}
	if eventMap["query_id"] != "q-123" {
		t.Errorf("query_id = %v, want q-123", eventMap["query_id"])
	}
	if eventMap["click_dog_source"] != "query_log" {
		t.Errorf("click_dog_source = %v, want query_log", eventMap["click_dog_source"])
	}
	if eventMap["normalized_query_hash"] != "123456" {
		t.Errorf("normalized_query_hash = %v, want 123456", eventMap["normalized_query_hash"])
	}
	if eventMap["normalized_query"] != "SELECT ?" {
		t.Errorf("normalized_query = %v, want SELECT ?", eventMap["normalized_query"])
	}
}

func TestSplunkHEC_ExportQueryFailureResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	exp, err := NewSplunkHECExporter(config.SplunkHECConfig{
		Endpoint: server.URL,
		Token:    "test",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	result, err := exp.ExportQuery(context.Background(), model.QueryLog{
		QueryID:   "q-123",
		QueryKind: "QueryFinish",
		Query:     "SELECT 1",
		EventTime: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err == nil {
		t.Fatal("expected error for 503 response")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error should mention 503, got: %v", err)
	}
	if result.TotalSent != 1 || result.TotalAccepted != 0 {
		t.Fatalf("result counts = sent %d accepted %d, want 1/0", result.TotalSent, result.TotalAccepted)
	}
	if len(result.Sinks) != 1 {
		t.Fatalf("len(Sinks) = %d, want 1", len(result.Sinks))
	}
	if result.Sinks[0].Name != "splunk_hec" || result.Sinks[0].Sent != 1 || result.Sinks[0].Accepted != 0 || result.Sinks[0].Error == nil {
		t.Fatalf("sink status = %+v, want splunk_hec sent=1 accepted=0 with error", result.Sinks[0])
	}
}

func TestSplunkHEC_ExportQueryOmitsNormalizedWhenAbsent(t *testing.T) {
	var receivedBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		receivedBody = string(buf)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	exp, err := NewSplunkHECExporter(config.SplunkHECConfig{
		Endpoint: server.URL,
		Token:    "test",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = exp.ExportQuery(context.Background(), model.QueryLog{
		QueryID:   "q-123",
		QueryKind: "QueryFinish",
		Query:     "SELECT 1",
		EventTime: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var ev hecEvent
	if err := json.Unmarshal([]byte(receivedBody), &ev); err != nil {
		t.Fatalf("failed to parse HEC event: %v", err)
	}
	eventMap, ok := ev.Event.(map[string]interface{})
	if !ok {
		t.Fatalf("event is not a map")
	}
	for _, key := range []string{"normalized_query_hash", "normalized_query"} {
		if _, ok := eventMap[key]; ok {
			t.Errorf("%s should be omitted when absent", key)
		}
	}
}

func TestSplunkHEC_ExportQueryOmitsAbsentRawQuery(t *testing.T) {
	var received map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev hecEvent
		if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
			t.Errorf("decode HEC event: %v", err)
		}
		received, _ = ev.Event.(map[string]interface{})
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	exp, err := NewSplunkHECExporter(config.SplunkHECConfig{Endpoint: server.URL, Token: "token"})
	if err != nil {
		t.Fatalf("NewSplunkHECExporter: %v", err)
	}
	_, err = exp.ExportQuery(context.Background(), model.QueryLog{
		QueryID:             "q-normalized-only",
		EventTime:           time.Now(),
		NormalizedQueryHash: 123,
		NormalizedQuery:     "SELECT ?",
		ExceptionCode:       62,
	})
	if err != nil {
		t.Fatalf("ExportQuery: %v", err)
	}
	if _, ok := received["query"]; ok {
		t.Fatal("Splunk HEC exporter emitted query for a shaped record without raw text")
	}
	if received["normalized_query"] != "SELECT ?" || received["error"] != true {
		t.Fatalf("safe normalized/error metadata missing: %#v", received)
	}
}

func TestSplunkHEC_TLSSkipVerify(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Without insecure_skip_verify, this should fail (self-signed cert)
	exp, err := NewSplunkHECExporter(config.SplunkHECConfig{
		Endpoint:           server.URL,
		Token:              "test",
		InsecureSkipVerify: false,
	})
	if err != nil {
		t.Fatalf("unexpected error creating exporter: %v", err)
	}
	_, err = exp.ExportSpans(context.Background(), []model.OpenTelemetrySpan{{SpanID: 1, TraceID: testTraceID}})
	if err == nil {
		t.Error("expected TLS error for self-signed cert without skip verify")
	}

	// With insecure_skip_verify, it should succeed
	exp2, err := NewSplunkHECExporter(config.SplunkHECConfig{
		Endpoint:           server.URL,
		Token:              "test",
		InsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatalf("unexpected error creating exporter: %v", err)
	}
	result, err := exp2.ExportSpans(context.Background(), []model.OpenTelemetrySpan{{SpanID: 1, TraceID: testTraceID}})
	if err != nil {
		t.Fatalf("expected success with skip verify, got: %v", err)
	}
	if len(result.Accepted) != 1 {
		t.Errorf("expected 1 exported key, got %d", len(result.Accepted))
	}
}

func TestSplunkHEC_Close(t *testing.T) {
	exp, err := NewSplunkHECExporter(config.SplunkHECConfig{
		Endpoint: "http://localhost:9999",
		Token:    "test",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	err = exp.Close(context.Background())
	if err != nil {
		t.Fatalf("unexpected error on close: %v", err)
	}
}

func TestSplunkHEC_NewExporter_Validation(t *testing.T) {
	_, err := NewSplunkHECExporter(config.SplunkHECConfig{
		Endpoint: "",
		Token:    "test",
	})
	if err == nil || !strings.Contains(err.Error(), "endpoint") {
		t.Errorf("expected endpoint required error, got: %v", err)
	}

	_, err = NewSplunkHECExporter(config.SplunkHECConfig{
		Endpoint: "http://localhost:8088",
		Token:    "",
	})
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("expected token required error, got: %v", err)
	}
}
