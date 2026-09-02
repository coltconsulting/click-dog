package export

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coltconsulting/click-dog/internal/clicklog"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/model"
)

// Compile-time checks that *SplunkHECExporter satisfies model.SpanExporter
// and model.ConnectivityChecker (see otel.go for the package convention).
var (
	_ model.SpanExporter        = (*SplunkHECExporter)(nil)
	_ model.ConnectivityChecker = (*SplunkHECExporter)(nil)
)

// SplunkHECExporter exports spans and queries to a Splunk HTTP Event Collector.
type SplunkHECExporter struct {
	client         *http.Client
	endpoint       string // full URL including /services/collector/event
	token          string
	index          string
	source         string
	sourceType     string
	maxQueryLength int
}

// hecEvent is the Splunk HEC JSON event envelope.
type hecEvent struct {
	Time       float64     `json:"time"`
	Host       string      `json:"host,omitempty"`
	Source     string      `json:"source,omitempty"`
	SourceType string      `json:"sourcetype,omitempty"`
	Index      string      `json:"index,omitempty"`
	Event      interface{} `json:"event"`
}

// NewSplunkHECExporter creates a SplunkHECExporter from the given config.
func NewSplunkHECExporter(cfg config.SplunkHECConfig) (*SplunkHECExporter, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("splunk_hec.endpoint is required")
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("splunk_hec.token is required")
	}

	endpoint := strings.TrimRight(cfg.Endpoint, "/") + "/services/collector/event"

	source := cfg.Source
	if source == "" {
		source = "click-dog"
	}
	sourceType := cfg.SourceType
	if sourceType == "" {
		sourceType = "_json"
	}
	maxQueryLength := cfg.MaxQueryLength
	if maxQueryLength == 0 {
		maxQueryLength = 100000
	}

	// MinVersion is Go's current client default, stated explicitly so the floor
	// is a property of this config rather than of the toolchain. Not a behavior
	// change; see internal/export/otel.go for the same note.
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: cfg.InsecureSkipVerify,
		},
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}

	if cfg.InsecureSkipVerify {
		clicklog.Warn("Splunk HEC TLS certificate verification is disabled - this is insecure")
	}

	clicklog.Info("Splunk HEC exporter initialized: %s", cfg.Endpoint)

	return &SplunkHECExporter{
		client:         client,
		endpoint:       endpoint,
		token:          cfg.Token,
		index:          cfg.Index,
		source:         source,
		sourceType:     sourceType,
		maxQueryLength: maxQueryLength,
	}, nil
}

// ExportSpans converts spans to HEC events and POSTs them.
func (s *SplunkHECExporter) ExportSpans(ctx context.Context, spans []model.OpenTelemetrySpan) (model.ExportResult, error) {
	if len(spans) == 0 {
		return model.ExportResult{}, nil
	}

	var buf bytes.Buffer
	exportedKeys := make([]model.SpanKey, 0, len(spans))

	for _, span := range spans {
		durationMs := int64((span.FinishTimeUs - span.StartTimeUs) / 1000)

		eventData := map[string]interface{}{
			"click_dog_source": liveSpanSource(span.Attributes),
			"trace_id":         span.TraceID,
			"span_id":          span.SpanID,
			"parent_span_id":   span.ParentSpanID,
			"operation_name":   span.OperationName,
			"kind":             span.Kind,
			"hostname":         span.Hostname,
			"start_time_us":    span.StartTimeUs,
			"finish_time_us":   span.FinishTimeUs,
			"duration_ms":      durationMs,
			"attributes":       liveSpanAttributesForHEC(span.Attributes, s.maxQueryLength),
		}

		ev := hecEvent{
			Time:       float64(span.StartTimeUs) / 1e6, // microseconds to seconds
			Host:       span.Hostname,
			Source:     s.source,
			SourceType: s.sourceType,
			Index:      s.index,
			Event:      eventData,
		}

		data, err := json.Marshal(ev)
		if err != nil {
			clicklog.Error("Splunk HEC: failed to marshal span %s/%d: %v", span.TraceID, span.SpanID, err)
			continue
		}
		buf.Write(data)
		exportedKeys = append(exportedKeys, model.KeyOf(span))
	}

	if buf.Len() == 0 {
		err := fmt.Errorf("failed to marshal all %d spans", len(spans))
		return failedResult("splunk_hec", len(spans), err), err
	}

	if err := s.post(ctx, buf.Bytes()); err != nil {
		return failedResult("splunk_hec", len(spans), err), err
	}

	return acceptedSpansResult("splunk_hec", len(spans), exportedKeys), nil
}

// ExportQuery converts a query_log row to a HEC event and POSTs it.
func (s *SplunkHECExporter) ExportQuery(ctx context.Context, log model.QueryLog) (model.ExportResult, error) {
	eventData := map[string]interface{}{
		"click_dog_source":  "query_log",
		"query_id":          log.QueryID,
		"query_kind":        log.QueryKind,
		"query_duration_ms": log.QueryDurationMs,
		"user":              log.User,
		"client_name":       log.ClientName,
		"client_hostname":   log.ClientHostname,
		"client_address":    log.ClientAddress,
		"read_rows":         log.ReadRows,
		"read_bytes":        log.ReadBytes,
		"written_rows":      log.WrittenRows,
		"written_bytes":     log.WrittenBytes,
		"result_rows":       log.ResultRows,
		"result_bytes":      log.ResultBytes,
		"memory_usage":      log.MemoryUsage,
	}
	if query := TruncateQuery(log.Query, s.maxQueryLength); query != "" {
		eventData["query"] = query
	}

	if len(log.DatabasesVisited) > 0 {
		eventData["databases"] = log.DatabasesVisited
	}
	if len(log.TablesVisited) > 0 {
		eventData["tables"] = log.TablesVisited
	}
	if log.NormalizedQueryHash != 0 {
		eventData["normalized_query_hash"] = strconv.FormatUint(log.NormalizedQueryHash, 10)
	}
	if log.NormalizedQuery != "" {
		eventData["normalized_query"] = TruncateQuery(log.NormalizedQuery, s.maxQueryLength)
	}
	if log.ExceptionCode != 0 {
		eventData["exception_code"] = log.ExceptionCode
		eventData["error"] = true
	}

	ev := hecEvent{
		Time:       float64(log.EventTime.UnixNano()) / 1e9,
		Source:     s.source,
		SourceType: s.sourceType,
		Index:      s.index,
		Event:      eventData,
	}

	data, err := json.Marshal(ev)
	if err != nil {
		wrappedErr := fmt.Errorf("failed to marshal query event: %w", err)
		return failedResult("splunk_hec", 1, wrappedErr), wrappedErr
	}

	if err := s.post(ctx, data); err != nil {
		return failedResult("splunk_hec", 1, err), err
	}
	return acceptedCountResult("splunk_hec", 1, 1), nil
}

// CheckConnectivity verifies network connectivity to the Splunk HEC endpoint
// by hitting the health endpoint. A 200 or 400/404 means "reachable".
func (s *SplunkHECExporter) CheckConnectivity(ctx context.Context) error {
	// Derive health URL from the event endpoint
	healthURL := strings.TrimSuffix(s.endpoint, "/services/collector/event") + "/services/collector/health/1.0"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create health request: %w", err)
	}
	req.Header.Set("Authorization", "Splunk "+s.token)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	// 200 = healthy, 400/404 = endpoint reachable but health path unavailable (still pass)
	if resp.StatusCode == 200 || resp.StatusCode == 400 || resp.StatusCode == 404 {
		return nil
	}
	return fmt.Errorf("health check returned unexpected status: %d", resp.StatusCode)
}

// Close shuts down idle HTTP connections.
func (s *SplunkHECExporter) Close(_ context.Context) error {
	s.client.CloseIdleConnections()
	return nil
}

// post sends the payload to the HEC endpoint with auth.
func (s *SplunkHECExporter) post(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("splunk HEC: failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Splunk "+s.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("splunk HEC: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("splunk HEC: unexpected status %d", resp.StatusCode)
	}

	return nil
}
