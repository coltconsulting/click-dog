// Package datadog contains the small, purpose-built Datadog Event Management
// client used by explicit query-analysis notification runs.
package datadog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coltconsulting/click-dog/internal/analysis"
	"github.com/coltconsulting/click-dog/internal/config"
)

const maxEventResponseBytes int64 = 64 << 10

// EventClient posts one bounded custom alert event to Datadog Events API v2.
type EventClient struct {
	endpoint       string
	apiKey         string
	applicationKey string
	environment    string
	service        string
	http           *http.Client
}

// NewEventClient constructs a destination from validated click-dog
// configuration. It independently verifies the security-critical site,
// credentials, timeout, and tags so direct callers cannot bypass LoadConfig.
func NewEventClient(cfg config.DatadogEventsConfig) (*EventClient, error) {
	site, err := config.ValidateDatadogSite(cfg.Site)
	if err != nil {
		return nil, fmt.Errorf("invalid Datadog Events site: %v", err)
	}
	if strings.TrimSpace(cfg.APIKey) == "" || strings.TrimSpace(cfg.ApplicationKey) == "" {
		return nil, errors.New("missing Datadog Events API or application credential")
	}
	if cfg.TimeoutS <= 0 || cfg.TimeoutS > config.MaxDatadogEventsTimeoutS {
		return nil, fmt.Errorf("invalid Datadog Events timeout; must be between 1 and %d seconds", config.MaxDatadogEventsTimeoutS)
	}
	if reason := config.ValidateDatadogTagValue("environment tag for Datadog Events", cfg.Environment); reason != "" {
		return nil, errors.New(reason)
	}
	if reason := config.ValidateDatadogTagValue("service tag for Datadog Events", cfg.Service); reason != "" {
		return nil, errors.New(reason)
	}
	return &EventClient{
		endpoint:       eventIntakeURL(site),
		apiKey:         cfg.APIKey,
		applicationKey: cfg.ApplicationKey,
		environment:    cfg.Environment,
		service:        cfg.Service,
		http: &http.Client{
			Timeout: time.Duration(cfg.TimeoutS) * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func eventIntakeURL(site string) string {
	return (&url.URL{
		Scheme: "https",
		Host:   "event-management-intake." + site,
		Path:   "/api/v2/events",
	}).String()
}

// Send posts one structured event rendered only from the shared privacy DTO.
// A nil error means intake accepted a 2xx response; it says nothing about
// monitor evaluation or downstream notification.
func (c *EventClient) Send(ctx context.Context, summary analysis.NotificationSummary) error {
	body, err := json.Marshal(buildEventPayload(summary, c.environment, c.service))
	if err != nil {
		return errors.New("failed to encode Datadog event payload")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("failed to construct Datadog event request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("DD-API-KEY", c.apiKey)
	req.Header.Set("DD-APPLICATION-KEY", c.applicationKey)

	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("delivery of Datadog event timed out: %w", ctx.Err())
		}
		return errors.New("failed to deliver Datadog event")
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxEventResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected HTTP %d from Datadog Events API", resp.StatusCode)
	}
	return nil
}

type eventCreateRequest struct {
	Data eventData `json:"data"`
}

type eventData struct {
	Type       string          `json:"type"`
	Attributes eventAttributes `json:"attributes"`
}

type eventAttributes struct {
	AggregationKey string               `json:"aggregation_key"`
	Attributes     eventAlertAttributes `json:"attributes"`
	Category       string               `json:"category"`
	IntegrationID  string               `json:"integration_id"`
	Message        string               `json:"message"`
	Tags           []string             `json:"tags"`
	Timestamp      string               `json:"timestamp"`
	Title          string               `json:"title"`
}

type eventAlertAttributes struct {
	Custom   eventCustomAttributes `json:"custom"`
	Priority string                `json:"priority"`
	Status   string                `json:"status"`
}

type eventCustomAttributes struct {
	Source                    string                           `json:"source"`
	EventType                 string                           `json:"event_type"`
	ReportSchemaVersion       string                           `json:"report_schema_version"`
	NotificationSchemaVersion string                           `json:"notification_schema_version"`
	WindowStart               string                           `json:"window_start"`
	WindowEnd                 string                           `json:"window_end"`
	HighestEligibleSeverity   analysis.Severity                `json:"highest_eligible_severity"`
	TotalCritical             int                              `json:"total_critical"`
	TotalWarning              int                              `json:"total_warning"`
	TotalInfo                 int                              `json:"total_info"`
	NewCritical               int                              `json:"new_critical"`
	NewWarning                int                              `json:"new_warning"`
	NewInfo                   int                              `json:"new_info"`
	EligibleFindingCount      int                              `json:"eligible_finding_count"`
	IncludedFindingCount      int                              `json:"included_finding_count"`
	TruncatedFindingCount     int                              `json:"truncated_finding_count"`
	Conditions                []analysis.NotificationCondition `json:"conditions"`
	Environment               string                           `json:"environment"`
	Service                   string                           `json:"service"`
}

func buildEventPayload(summary analysis.NotificationSummary, environment, service string) eventCreateRequest {
	status := "warn"
	priority := "3"
	if summary.HighestEligibleSeverity == analysis.SeverityCritical {
		status = "error"
		priority = "1"
	}
	return eventCreateRequest{Data: eventData{
		Type: "event",
		Attributes: eventAttributes{
			AggregationKey: "click-dog-analysis-findings",
			Attributes: eventAlertAttributes{
				Custom: eventCustomAttributes{
					Source:                    "click-dog",
					EventType:                 "analysis_findings",
					ReportSchemaVersion:       summary.ReportSchemaVersion,
					NotificationSchemaVersion: summary.SchemaVersion,
					WindowStart:               summary.Window.Start.UTC().Format(time.RFC3339),
					WindowEnd:                 summary.Window.End.UTC().Format(time.RFC3339),
					HighestEligibleSeverity:   summary.HighestEligibleSeverity,
					TotalCritical:             summary.TotalFindingCounts.Critical,
					TotalWarning:              summary.TotalFindingCounts.Warning,
					TotalInfo:                 summary.TotalFindingCounts.Info,
					NewCritical:               summary.NewFindingCounts.Critical,
					NewWarning:                summary.NewFindingCounts.Warning,
					NewInfo:                   summary.NewFindingCounts.Info,
					EligibleFindingCount:      summary.EligibleFindingCount,
					IncludedFindingCount:      summary.IncludedFindingCount,
					TruncatedFindingCount:     summary.TruncatedFindingCount,
					Conditions:                summary.Conditions,
					Environment:               environment,
					Service:                   service,
				},
				Priority: priority,
				Status:   status,
			},
			Category:      "alert",
			IntegrationID: "custom-events",
			Message: fmt.Sprintf(
				"Findings total critical=%d warning=%d info=%d; new critical=%d warning=%d info=%d. Review the local click-dog JSON report for evidence. Use click-dog analyze trace with a listed normalized query hash for drilldown.",
				summary.TotalFindingCounts.Critical,
				summary.TotalFindingCounts.Warning,
				summary.TotalFindingCounts.Info,
				summary.NewFindingCounts.Critical,
				summary.NewFindingCounts.Warning,
				summary.NewFindingCounts.Info,
			),
			Tags: []string{
				"source:click-dog",
				"event_type:analysis_findings",
				"environment:" + environment,
				"service:" + service,
				"highest_eligible_severity:" + string(summary.HighestEligibleSeverity),
				"report_schema_version:" + summary.ReportSchemaVersion,
				"notification_schema_version:" + summary.SchemaVersion,
			},
			Timestamp: summary.Window.End.UTC().Format(time.RFC3339),
			Title:     "Click-Dog query analysis findings",
		},
	}}
}
