package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/coltconsulting/click-dog/internal/metrics"
)

// dashboardByName returns the shipped dashboard definition with the given
// --dashboard name, failing the test if it is absent.
func dashboardByName(t *testing.T, name string) dashboardDef {
	t.Helper()
	for _, d := range shippedDashboards {
		if d.name == name {
			return d
		}
	}
	t.Fatalf("no shipped dashboard named %q", name)
	return dashboardDef{}
}

func TestValidateDatadogSite(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "US1", input: "datadoghq.com", want: "datadoghq.com"},
		{name: "EU", input: "DATADOGHQ.EU", want: "datadoghq.eu"},
		{name: "regional", input: "ap2.datadoghq.com", want: "ap2.datadoghq.com"},
		{name: "custom", input: "demo.datadoghq.com", want: "demo.datadoghq.com"},
		{name: "government", input: "us2.ddog-gov.com", want: "us2.ddog-gov.com"},
		{name: "future root", input: "observability.example", want: "observability.example"},
		{name: "operator selected host", input: "attacker.example", want: "attacker.example"},
		{name: "userinfo injection", input: "datadoghq.com@attacker.example", wantErr: true},
		{name: "port", input: "datadoghq.com:443", wantErr: true},
		{name: "path", input: "datadoghq.com/api", wantErr: true},
		{name: "encoded authority", input: "datadoghq.com%40attacker.example", wantErr: true},
		{name: "newline", input: "datadoghq.com\nattacker.example", wantErr: true},
		{name: "trailing dot", input: "datadoghq.com.", wantErr: true},
		{name: "IP address", input: "127.0.0.1", wantErr: true},
		{name: "single label", input: "localhost", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateDatadogSite(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("validateDatadogSite(%q) = %q, want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateDatadogSite(%q): %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("validateDatadogSite(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestDatadogAPIURLUsesHostField(t *testing.T) {
	got := datadogAPIURL("future-observability.example", "api", "v1", "dashboard", "id with spaces")
	want := "https://api.future-observability.example/api/v1/dashboard/id%20with%20spaces"
	if got != want {
		t.Fatalf("datadogAPIURL() = %q, want %q", got, want)
	}
}

func TestHTTPDDClientRefusesRedirects(t *testing.T) {
	targetHit := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHit = true
		if r.Header.Get("DD-API-KEY") != "" || r.Header.Get("DD-APPLICATION-KEY") != "" {
			t.Error("redirect target received Datadog credentials")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	client := &httpDDClient{apiKey: "api-secret", appKey: "app-secret", http: newDatadogHTTPClient()}
	_, err := client.do(http.MethodGet, source.URL, nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 307") {
		t.Fatalf("redirect response error = %v, want HTTP 307", err)
	}
	if targetHit {
		t.Fatal("Datadog client followed a redirect")
	}
}

// TestCreateDashboards_HealthChecklistPrerequisites is the contract test for
// issue #178: importing the Health dashboard must surface, at import time, the
// OTLP self-metrics switch and collector inheritance that make a fresh import
// show data without a Datadog Agent OpenMetrics scrape.
// Without these the dashboard renders blank and the operator has no breadcrumb
// from the create-dashboards output.
func TestCreateDashboards_HealthChecklistPrerequisites(t *testing.T) {
	var buf bytes.Buffer
	printDashboardChecklist(dashboardByName(t, "health"), &buf)
	out := buf.String()

	for _, want := range []string{
		"metrics.otlp.enabled: true",
		"exporters.otel[0]",
		"OTLP collector",
		"metrics.otlp.host",
		"docs/integrations/datadog/self-monitoring.md",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("health post-import checklist missing %q; got:\n%s", want, out)
		}
	}
}

// TestCreateDashboards_QueryChecklistMentionsTraces verifies the query
// dashboard checklist points operators at trace ingestion and the service
// template variable rather than at the metrics listener (which it does not
// use).
func TestCreateDashboards_QueryChecklistMentionsTraces(t *testing.T) {
	var buf bytes.Buffer
	printDashboardChecklist(dashboardByName(t, "query"), &buf)
	out := buf.String()

	for _, want := range []string{"exporters.otel", "service", "click-dog-monitor"} {
		if !strings.Contains(out, want) {
			t.Errorf("query post-import checklist missing %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "metrics.enabled") || strings.Contains(out, ":9090") {
		t.Errorf("query checklist should not reference the metrics listener; got:\n%s", out)
	}
}

func TestCreateDashboards_ActivityChecklistMentionsEnrichmentAndScope(t *testing.T) {
	var buf bytes.Buffer
	printDashboardChecklist(dashboardByName(t, "activity"), &buf)
	out := buf.String()

	for _, want := range []string{
		"monitor.enrich_from_query_log: true",
		"system.query_log",
		"service",
		"qualified/exported",
		"not total ClickHouse traffic",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("activity post-import checklist missing %q; got:\n%s", want, out)
		}
	}
}

// TestCreateDashboards_EveryDashboardHasChecklist forces a post-import
// checklist on every shipped dashboard, so adding a third dashboard without
// import-time guidance fails loudly rather than shipping silent "No data"
// surprises.
func TestCreateDashboards_EveryDashboardHasChecklist(t *testing.T) {
	for _, d := range shippedDashboards {
		if len(d.prerequisites) == 0 {
			t.Errorf("shipped dashboard %q has no post-import checklist (prerequisites)", d.name)
		}
		var buf bytes.Buffer
		printDashboardChecklist(d, &buf)
		if !strings.Contains(buf.String(), "After import") {
			t.Errorf("dashboard %q checklist missing the \"After import\" header; got:\n%s", d.name, buf.String())
		}
	}
}

func TestEmbeddedDashboards_ValidJSON(t *testing.T) {
	for _, d := range shippedDashboards {
		t.Run(d.name, func(t *testing.T) {
			data, err := embeddedDashboards.ReadFile("dashboards/" + d.filename)
			if err != nil {
				t.Fatalf("reading embedded dashboard %s: %v", d.filename, err)
			}
			if len(data) == 0 {
				t.Fatalf("embedded dashboard %s is empty", d.filename)
			}
			var v map[string]interface{}
			if err := json.Unmarshal(data, &v); err != nil {
				t.Fatalf("%s is not valid JSON: %v", d.filename, err)
			}
			if _, ok := v["title"].(string); !ok {
				t.Errorf("%s: missing or non-string \"title\" field", d.filename)
			}
			if _, ok := v["widgets"].([]interface{}); !ok {
				t.Errorf("%s: missing or non-array \"widgets\" field", d.filename)
			}
		})
	}
}

// TestQueryDashboard_ServiceTemplateVariable is the contract test for
// issue #80. The query dashboard must drive its `service:` filter from a
// template variable so the dashboard works against any operator-chosen
// otel.service_name. It must:
//
//   - declare a `service` template variable with default `click-dog-monitor`
//     (the canonical default also used by `click-dog init` and config
//     defaults), and
//   - never reintroduce a hard-coded `service:<name>` literal inside a widget
//     query — only `$service` is allowed.
//
// The previous failure mode was widget queries containing
// `service:clickhouse-monitor` while `click-dog init` and config defaults
// produced `clickhouse-query-monitor`, so an out-of-the-box dashboard
// import showed no data.
func TestQueryDashboard_ServiceTemplateVariable(t *testing.T) {
	dash := loadQueryDashboard(t)

	tvs, ok := dash["template_variables"].([]interface{})
	if !ok {
		t.Fatal("template_variables missing or wrong type")
	}
	var found map[string]interface{}
	for _, tv := range tvs {
		obj, ok := tv.(map[string]interface{})
		if !ok {
			continue
		}
		if obj["name"] == "service" {
			found = obj
			break
		}
	}
	if found == nil {
		t.Fatal("query dashboard missing a `service` template variable")
	}
	if got := found["prefix"]; got != "service" {
		t.Errorf("service template variable prefix = %v, want %q", got, "service")
	}
	if got := found["default"]; got != "click-dog-monitor" {
		t.Errorf("service template variable default = %v, want %q (canonical default; matches click-dog init and config defaults)", got, "click-dog-monitor")
	}

	// `service:<word>` literal must not appear inside any widget query —
	// only the template-variable form `$service` is allowed. Allow `service`
	// to appear in note widget content (which is documentation), so we only
	// scan span search query strings.
	literalRE := regexp.MustCompile(`\bservice:[A-Za-z0-9._\-*]`)
	for _, q := range queryDashboardSpanSearchQueries(t, dash) {
		if literalRE.MatchString(q) {
			t.Errorf("widget query contains hard-coded service filter %q; use $service template variable instead", q)
		}
		if !strings.Contains(q, "$service") {
			t.Errorf("widget query missing $service template variable: %q", q)
		}
	}
}

func TestQueryDashboard_LiveSpanOnlyContract(t *testing.T) {
	dash := loadQueryDashboard(t)

	description, ok := dash["description"].(string)
	if !ok {
		t.Fatal("query dashboard missing string description")
	}
	for _, want := range []string{
		"Live-span-only",
		"system.opentelemetry_span_log",
		"clickhouse.query/db.*",
	} {
		if !strings.Contains(description, want) {
			t.Errorf("query dashboard description missing %q", want)
		}
	}

	// Locate the contract note by the source contract rather than prose that
	// may be reworded while preserving the same behavior.
	note := queryDashboardNoteContent(t, dash, "click_dog.source=span_log")
	for _, want := range []string{
		"click_dog.source=span_log",
		"system.opentelemetry_span_log",
		"resource_name:query",
		"click_dog.source=query_log",
		"clickhouse.query",
		"db.*",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("query dashboard live contract note missing %q", want)
		}
	}
	if !regexp.MustCompile(`v\d{2}\.\d{2}\.\d+`).MatchString(note) {
		t.Errorf("query dashboard live contract note missing a versioned compatibility requirement: %q", note)
	}

	for _, q := range queryDashboardSpanSearchQueries(t, dash) {
		if !strings.Contains(q, "resource_name:query") {
			t.Errorf("query dashboard widget query missing live resource filter: %q", q)
		}
		if !strings.Contains(q, "@click_dog.source:span_log") {
			t.Errorf("query dashboard widget query missing live source filter: %q", q)
		}
		for _, disallowed := range []string{
			"resource_name:clickhouse.query",
			"@click_dog.source:query_log",
			"@db.",
		} {
			if strings.Contains(q, disallowed) {
				t.Errorf("query dashboard widget query uses backfill-only filter/facet %q: %q", disallowed, q)
			}
		}
	}

	tvs, ok := dash["template_variables"].([]interface{})
	if !ok {
		t.Fatal("template_variables missing or wrong type")
	}
	for _, tv := range tvs {
		obj, ok := tv.(map[string]interface{})
		if !ok {
			continue
		}
		prefix, _ := obj["prefix"].(string)
		if strings.HasPrefix(prefix, "@db.") || prefix == "@click_dog.source" {
			t.Errorf("query dashboard template variable %q uses backfill/source-aware prefix %q; this dashboard is live-span-only", obj["name"], prefix)
		}
	}
}

func TestHealthDashboard_CircuitBreakerSeverityOrder(t *testing.T) {
	data, err := embeddedDashboards.ReadFile("dashboards/datadog-clickdog-health.json")
	if err != nil {
		t.Fatalf("reading health dashboard: %v", err)
	}

	var dash map[string]interface{}
	if err := json.Unmarshal(data, &dash); err != nil {
		t.Fatalf("health dashboard is not valid JSON: %v", err)
	}

	cbValue := findWidgetDefinitionByTitle(t, dash, "Circuit breaker")
	formats := firstRequestConditionalFormats(t, cbValue)
	wantFormats := []map[string]interface{}{
		{"comparator": "=", "value": float64(0), "palette": "white_on_green"},
		{"comparator": "=", "value": float64(1), "palette": "white_on_yellow"},
		{"comparator": "=", "value": float64(2), "palette": "white_on_red"},
	}
	if len(formats) != len(wantFormats) {
		t.Fatalf("conditional formats length = %d, want %d", len(formats), len(wantFormats))
	}
	for i, want := range wantFormats {
		got, ok := formats[i].(map[string]interface{})
		if !ok {
			t.Fatalf("conditional format %d has type %T, want object", i, formats[i])
		}
		for key, wantValue := range want {
			if got[key] != wantValue {
				t.Errorf("conditional format %d %s = %v, want %v", i, key, got[key], wantValue)
			}
		}
	}

	cbTimeline := findWidgetDefinitionByTitle(t, dash, "Circuit breaker state (0=closed, 1=half-open, 2=open)")
	yaxis, ok := cbTimeline["yaxis"].(map[string]interface{})
	if !ok {
		t.Fatal("circuit breaker timeline missing yaxis object")
	}
	if yaxis["min"] != "0" || yaxis["max"] != "2" {
		t.Errorf("circuit breaker timeline yaxis = %+v, want min=0 max=2", yaxis)
	}
}

// TestHealthDashboard_CockpitFleetWorstCaseAggregation is the contract test
// for issue #175. In the default one-sidecar-per-ClickHouse-node deployment the
// top cockpit must answer "is any sidecar unhealthy?" without the operator
// pre-selecting a host. Averaging across hosts lets one healthy peer mask a bad
// sidecar — and produces fractional breaker values that never match the
// 0/1/2 conditional formats — so the breaker and backoff tiles must use the
// worst-case `max` space aggregation and the last-success tile must use the
// stalest `min` timestamp from active exporters only. A per-host view grouped
// `by {host.name}` must surface the same signals so the offending active
// exporter is immediately identifiable without leader-gated standbys poisoning
// stale-export age.
func TestHealthDashboard_CockpitFleetWorstCaseAggregation(t *testing.T) {
	dash := loadHealthDashboard(t)

	cockpit := []struct {
		title      string
		metric     string
		wantPrefix string
	}{
		{"Circuit breaker", "click_dog.circuit_breaker_state", "max:"},
		{"Last success age", "click_dog.last_success_timestamp_seconds", "min:"},
		{"Backoff interval", "click_dog.backoff_interval_seconds", "max:"},
	}
	for _, tc := range cockpit {
		def := findWidgetDefinitionByTitle(t, dash, tc.title)
		var found bool
		for _, q := range widgetQueries(t, def) {
			query, _ := q["query"].(string)
			if !strings.Contains(query, tc.metric) {
				continue
			}
			found = true
			if !strings.HasPrefix(query, tc.wantPrefix) {
				t.Errorf("%q cockpit query %q must use %q space aggregation so one unhealthy sidecar is not hidden by healthy peers", tc.title, query, tc.wantPrefix)
			}
			if strings.HasPrefix(query, "avg:") {
				t.Errorf("%q cockpit query %q averages across the fleet, hiding a single unhealthy sidecar", tc.title, query)
			}
			if tc.metric == "click_dog.last_success_timestamp_seconds" && !strings.Contains(query, "role:active") {
				t.Errorf("%q cockpit query %q must filter to role:active so HA standbys do not make stale-export age red", tc.title, query)
			}
		}
		if !found {
			t.Errorf("%q widget has no query referencing %q", tc.title, tc.metric)
		}
	}

	perHost := findPerHostHealthWidget(t, dash)
	grouped := make(map[string]bool)
	for _, q := range widgetQueries(t, perHost) {
		query, _ := q["query"].(string)
		if !strings.Contains(query, "by {host.name}") {
			continue
		}
		for _, m := range []string{
			"click_dog.circuit_breaker_state",
			"click_dog.leader",
			"click_dog.last_success_timestamp_seconds",
			"click_dog.backoff_interval_seconds",
		} {
			if strings.Contains(query, m) {
				grouped[m] = true
				if m == "click_dog.last_success_timestamp_seconds" && !strings.Contains(query, "role:active") {
					t.Errorf("per-host last-success query %q must filter to role:active so standbys remain visible without ranking as stale exporters", query)
				}
			}
		}
	}
	for _, m := range []string{
		"click_dog.circuit_breaker_state",
		"click_dog.leader",
		"click_dog.last_success_timestamp_seconds",
		"click_dog.backoff_interval_seconds",
	} {
		if !grouped[m] {
			t.Errorf("per-host health widget missing a `by {host.name}` query for %q", m)
		}
	}
}

func findPerHostHealthWidget(t *testing.T, dash map[string]interface{}) map[string]interface{} {
	t.Helper()
	widgets, ok := dash["widgets"].([]interface{})
	if !ok {
		t.Fatal("dashboard widgets missing or wrong type")
	}
	for _, w := range widgets {
		obj, ok := w.(map[string]interface{})
		if !ok {
			continue
		}
		def, ok := obj["definition"].(map[string]interface{})
		if !ok {
			continue
		}
		switch def["type"] {
		case "query_table", "toplist":
		default:
			continue
		}
		for _, q := range widgetQueries(t, def) {
			if s, _ := q["query"].(string); strings.Contains(s, "by {host.name}") {
				return def
			}
		}
	}
	t.Fatal("no per-host health widget (query_table/toplist grouped `by {host.name}`) found")
	return nil
}

func TestHealthDashboard_ActionStateCockpitFirstRow(t *testing.T) {
	dash := loadHealthDashboard(t)

	cockpitTitles := map[string]bool{
		"Last success age":  true,
		"Circuit breaker":   true,
		"Error rate":        true,
		"Export throughput": true,
		"Backoff interval":  true,
	}
	lifetimeTitles := map[string]bool{
		"Uptime":       true,
		"Cycles total": true,
		"Errors total": true,
	}
	cockpitY := make(map[string]float64, len(cockpitTitles))
	lifetimeY := make(map[string]float64, len(lifetimeTitles))
	var minDataY float64
	var haveDataWidget bool

	widgets, ok := dash["widgets"].([]interface{})
	if !ok {
		t.Fatal("dashboard widgets missing or wrong type")
	}
	for i, widget := range widgets {
		widgetObj, ok := widget.(map[string]interface{})
		if !ok {
			t.Errorf("widget %d has type %T, want object", i, widget)
			continue
		}
		def, ok := widgetObj["definition"].(map[string]interface{})
		if !ok {
			t.Errorf("widget %d definition has type %T, want object", i, widgetObj["definition"])
			continue
		}
		title, _ := def["title"].(string)
		widgetType, _ := def["type"].(string)

		layout, ok := widgetObj["layout"].(map[string]interface{})
		if !ok {
			t.Errorf("widget %q missing layout", title)
			continue
		}
		y, ok := layout["y"].(float64)
		if !ok {
			t.Errorf("widget %q layout y has type %T, want number", title, layout["y"])
			continue
		}
		if widgetType != "note" && (!haveDataWidget || y < minDataY) {
			minDataY = y
			haveDataWidget = true
		}

		if cockpitTitles[title] {
			cockpitY[title] = y
		}
		if lifetimeTitles[title] {
			lifetimeY[title] = y
		}
	}

	for title := range cockpitTitles {
		if _, found := cockpitY[title]; !found {
			t.Errorf("top cockpit row missing %q", title)
		}
	}
	for title := range lifetimeTitles {
		if _, found := lifetimeY[title]; !found {
			t.Errorf("lifetime counter section missing %q", title)
		}
	}
	if len(cockpitY) == len(cockpitTitles) && len(lifetimeY) == len(lifetimeTitles) {
		cockpitRows := make(map[float64][]string)
		var minCockpitY, maxCockpitY float64
		var haveCockpitWidget bool
		for title, y := range cockpitY {
			cockpitRows[y] = append(cockpitRows[y], title)
			if !haveCockpitWidget || y < minCockpitY {
				minCockpitY = y
			}
			if !haveCockpitWidget || y > maxCockpitY {
				maxCockpitY = y
			}
			haveCockpitWidget = true
		}
		if len(cockpitRows) != 1 {
			t.Errorf("action-state widgets should share one cockpit row, got rows %v", cockpitRows)
		}
		if haveDataWidget && minCockpitY != minDataY {
			t.Errorf("cockpit row y=%v is not the topmost data row y=%v", minCockpitY, minDataY)
		}

		minLifetimeY := lifetimeY["Uptime"]
		for _, y := range lifetimeY {
			if y < minLifetimeY {
				minLifetimeY = y
			}
		}
		if maxCockpitY >= minLifetimeY {
			t.Errorf("cockpit rows ending at y=%v should appear before lifetime counters starting at y=%v", maxCockpitY, minLifetimeY)
		}
	}

	for _, tc := range []struct {
		title string
		want  string
	}{
		{"Error rate", "sum:click_dog.cycle_results.count{result:error,$host}.as_rate()"},
		{"Export throughput", "sum:click_dog.spans_exported.count{$host}.as_rate()"},
	} {
		def := findWidgetDefinitionByTitle(t, dash, tc.title)
		var found bool
		for _, query := range widgetQueries(t, def) {
			if query["query"] == tc.want {
				found = true
			}
		}
		if !found {
			t.Errorf("%q missing current-rate query %q", tc.title, tc.want)
		}
	}
}

func TestHealthDashboard_ActionStateConditionalFormats(t *testing.T) {
	dash := loadHealthDashboard(t)

	tests := []struct {
		title string
		want  []map[string]interface{}
	}{
		{
			title: "Export throughput",
			want: []map[string]interface{}{
				{"comparator": ">", "value": float64(0), "palette": "white_on_green"},
				{"comparator": "<=", "value": float64(0), "palette": "white_on_gray"},
			},
		},
		{
			title: "Error rate",
			want: []map[string]interface{}{
				{"comparator": "<=", "value": float64(0), "palette": "white_on_green"},
				{"comparator": ">", "value": float64(0), "palette": "white_on_red"},
			},
		},
		{
			title: "Backoff interval",
			want: []map[string]interface{}{
				{"comparator": ">=", "value": float64(300), "palette": "white_on_red"},
				{"comparator": ">", "value": float64(60), "palette": "white_on_yellow"},
				{"comparator": "<=", "value": float64(60), "palette": "white_on_green"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.title, func(t *testing.T) {
			def := findWidgetDefinitionByTitle(t, dash, tc.title)
			formats := firstRequestConditionalFormats(t, def)
			if len(formats) != len(tc.want) {
				t.Fatalf("conditional formats length = %d, want %d", len(formats), len(tc.want))
			}
			for i, want := range tc.want {
				got, ok := formats[i].(map[string]interface{})
				if !ok {
					t.Fatalf("conditional format %d has type %T, want object", i, formats[i])
				}
				for key, wantValue := range want {
					if got[key] != wantValue {
						t.Errorf("conditional format %d %s = %v, want %v", i, key, got[key], wantValue)
					}
				}
			}
		})
	}
}

func TestHealthDashboard_BackoffConditionalFormatsFirstMatch(t *testing.T) {
	dash := loadHealthDashboard(t)
	def := findWidgetDefinitionByTitle(t, dash, "Backoff interval")
	formats := firstRequestConditionalFormats(t, def)

	tests := []struct {
		value float64
		want  string
	}{
		{value: 30, want: "white_on_green"},
		{value: 60, want: "white_on_green"},
		{value: 61, want: "white_on_yellow"},
		{value: 120, want: "white_on_yellow"},
		{value: 300, want: "white_on_red"},
		{value: 600, want: "white_on_red"},
	}
	for _, tc := range tests {
		if got := firstMatchingPalette(t, formats, tc.value); got != tc.want {
			t.Errorf("first matching backoff palette for value %v = %q, want %q", tc.value, got, tc.want)
		}
	}
}

func TestQueryDashboard_ExactFamilyWidgetsUseNormalizedFacets(t *testing.T) {
	dash := loadQueryDashboard(t)

	note := queryDashboardNoteContent(t, dash, "## Exact Query Families")
	for _, want := range []string{"@query_log.normalized_query_hash", "@query_log.normalized_query"} {
		if !strings.Contains(note, want) {
			t.Errorf("exact family note missing %q", want)
		}
	}
	resourceNote := queryDashboardNoteContent(t, dash, "## Resource Usage")
	for _, want := range []string{"@query_log.normalized_query_hash", "older spans"} {
		if !strings.Contains(resourceNote, want) {
			t.Errorf("resource usage note missing %q", want)
		}
	}

	for _, title := range []string{
		"Slowest exact query families",
		"Top query families by rows read (p95)",
		"Top query families by peak memory (p95)",
	} {
		def := findWidgetDefinitionByTitle(t, dash, title)
		facets := widgetGroupByFacets(t, def)
		for _, want := range []string{"@query_log.normalized_query_hash", "@query_log.normalized_query"} {
			if !slices.Contains(facets, want) {
				t.Errorf("%q group_by facets = %v, missing %q", title, facets, want)
			}
		}
		if slices.Contains(facets, "resource_name") {
			t.Errorf("%q should not group exact query families by raw resource_name: %v", title, facets)
		}

		for _, query := range widgetSearchQueries(t, def) {
			if !strings.Contains(query, "@query_log.normalized_query_hash:*") {
				t.Errorf("%q search query should require normalized query hash, got %q", title, query)
			}
		}
	}
}

// TestQueryDashboard_AutomaticSectionsBeforeLogComment is the contract test
// for issue #85: every section that depends on `log_comment` tagging must
// sit below every section that does not, so a fresh-import dashboard shows
// useful automatic-dimension data first.
func TestQueryDashboard_AutomaticSectionsBeforeLogComment(t *testing.T) {
	dash := loadQueryDashboard(t)

	type section struct {
		title       string
		y           float64
		needsLogCmt bool
	}
	var sections []section

	widgets, ok := dash["widgets"].([]interface{})
	if !ok {
		t.Fatal("dashboard widgets missing or wrong type")
	}
	for _, w := range widgets {
		obj, ok := w.(map[string]interface{})
		if !ok {
			continue
		}
		def, ok := obj["definition"].(map[string]interface{})
		if !ok || def["type"] != "note" {
			continue
		}
		content, ok := def["content"].(string)
		if !ok {
			continue
		}
		// Skip the top-level `# Click-Dog ...` title note (no `## `); a
		// future note with `## ` mid-prose would be picked up here too.
		if !strings.Contains(content, "## ") {
			continue
		}
		layout, ok := obj["layout"].(map[string]interface{})
		if !ok {
			t.Fatalf("section note missing layout: %v", obj["id"])
		}
		y, ok := layout["y"].(float64)
		if !ok {
			t.Fatalf("section note layout.y not numeric: %v", layout["y"])
		}
		sections = append(sections, section{
			title:       firstHeaderLine(content),
			y:           y,
			needsLogCmt: noteRequiresLogComment(content),
		})
	}

	if len(sections) == 0 {
		t.Fatal("no section-header notes found in query dashboard")
	}

	var auto, logCmt []section
	for _, s := range sections {
		if s.needsLogCmt {
			logCmt = append(logCmt, s)
		} else {
			auto = append(auto, s)
		}
	}
	if len(auto) == 0 {
		t.Fatal("query dashboard has no automatic-value sections")
	}
	if len(logCmt) == 0 {
		t.Fatal("query dashboard has no log_comment enhancement sections")
	}

	maxAuto := auto[0].y
	maxAutoTitle := auto[0].title
	for _, s := range auto[1:] {
		if s.y > maxAuto {
			maxAuto = s.y
			maxAutoTitle = s.title
		}
	}
	minLog := logCmt[0].y
	minLogTitle := logCmt[0].title
	for _, s := range logCmt[1:] {
		if s.y < minLog {
			minLog = s.y
			minLogTitle = s.title
		}
	}
	if maxAuto >= minLog {
		t.Errorf("automatic section %q at y=%v must come before log_comment section %q at y=%v",
			maxAutoTitle, maxAuto, minLogTitle, minLog)
	}
}

func TestQueryDashboard_CockpitOnFirstScreen(t *testing.T) {
	dash := loadQueryDashboard(t)

	wantTitles := []string{
		"Exported queries (1h)",
		"p95 latency",
		"p99 latency",
		"Slow queries (>1s)",
		"Top user",
		"Top client",
	}
	// Header note + cockpit section header + cockpit tile row land y=0..4;
	// volume/latency timeseries start at y=5; y>=6 is below the fold.
	const firstScreenYLimit = 6.0
	for _, title := range wantTitles {
		widget := findWidgetByTitle(t, dash, title)
		def := widget["definition"].(map[string]interface{})
		layout, ok := widget["layout"].(map[string]interface{})
		if !ok {
			t.Fatalf("%q missing layout", title)
		}
		y, ok := layout["y"].(float64)
		if !ok {
			t.Fatalf("%q layout.y not numeric: %v", title, layout["y"])
		}
		if y >= firstScreenYLimit {
			t.Errorf("cockpit widget %q at y=%v must sit on first screen (y < %v)", title, y, firstScreenYLimit)
		}
		queries := widgetSearchQueries(t, def)
		if len(queries) == 0 {
			t.Errorf("cockpit widget %q has no search queries", title)
		}
		for _, q := range queries {
			if !strings.Contains(q, "$service") {
				t.Errorf("cockpit widget %q query missing $service: %q", title, q)
			}
		}
	}
}

// TestQueryDashboard_VolumeLabelsDisambiguated is the contract test for
// issue #176. In scheduled mode click-dog exports a thresholded stream
// selected by monitor.min_trace_duration_ms (plus query/operation/IP
// filters), not every ClickHouse query. The dashboard's count/volume widgets
// must say so in their labels so operators don't read the exported/qualified
// slow-query stream as total ClickHouse traffic. The previous failure mode
// was bare labels like "Queries (1h)" and "Queries per minute" that implied
// total query volume.
func TestQueryDashboard_VolumeLabelsDisambiguated(t *testing.T) {
	dash := loadQueryDashboard(t)

	// Every count/volume widget must carry disambiguated wording.
	for _, title := range []string{
		"Exported queries (1h)",
		"Exported queries per minute",
		"Exported queries by host",
		"Exported queries by tables accessed",
		"Top ClickHouse users by exported query volume",
		"Top apps by exported query volume",
		"Top named queries by exported volume",
	} {
		findWidgetByTitle(t, dash, title) // fails the test if absent
	}

	// Ambiguous bare-volume titles must not reappear.
	banned := map[string]bool{
		"Queries (1h)":                         true,
		"Queries per minute":                   true,
		"Queries by host":                      true,
		"Queries by tables accessed":           true,
		"Top ClickHouse users by query volume": true,
		"Top apps by query volume":             true,
		"Named query volume":                   true,
	}
	for _, title := range queryDashboardWidgetTitles(t, dash) {
		if banned[title] {
			t.Errorf("widget title %q is ambiguous; scheduled-mode volume is the exported/qualified query stream, "+
				"not total ClickHouse queries — use \"Exported\"/\"Qualified\" wording", title)
		}
	}

	// The top note must explain that scheduled-mode volume reflects the
	// configured duration/filter settings.
	note := queryDashboardNoteContent(t, dash, "# Click-Dog — Application Query Analysis")
	for _, want := range []string{"min_trace_duration_ms", "exported", "not total ClickHouse"} {
		if !strings.Contains(note, want) {
			t.Errorf("top note missing %q; it must explain that volume reflects the configured duration/filter settings", want)
		}
	}
}

func TestActivityDashboard_FilterAndScopeContract(t *testing.T) {
	dash := loadActivityDashboard(t)

	description, ok := dash["description"].(string)
	if !ok {
		t.Fatal("activity dashboard missing string description")
	}
	for _, want := range []string{"Live-span-only", "qualified/exported", "not a complete audit log"} {
		if !strings.Contains(description, want) {
			t.Errorf("activity dashboard description missing %q", want)
		}
	}

	wantVariables := map[string]string{
		"service":     "service",
		"env":         "env",
		"user":        "@query_log.user",
		"database":    "@query_log.databases",
		"table":       "@query_log.tables",
		"operation":   "@query_log.operation",
		"access_type": "@query_log.access_type",
		"host":        "@hostname",
	}
	tvs, ok := dash["template_variables"].([]interface{})
	if !ok {
		t.Fatal("activity dashboard template_variables missing or wrong type")
	}
	for _, raw := range tvs {
		tv, ok := raw.(map[string]interface{})
		if !ok {
			t.Fatalf("activity template variable has type %T, want object", raw)
		}
		name, _ := tv["name"].(string)
		prefix, _ := tv["prefix"].(string)
		want, exists := wantVariables[name]
		if !exists {
			t.Errorf("unexpected activity template variable %q", name)
			continue
		}
		if prefix != want {
			t.Errorf("activity template variable %q prefix = %q, want %q", name, prefix, want)
		}
		delete(wantVariables, name)
	}
	if len(wantVariables) > 0 {
		t.Errorf("activity dashboard missing template variables: %v", wantVariables)
	}

	queries := queryDashboardSpanSearchQueries(t, dash)
	for _, query := range queries {
		for _, want := range []string{
			"$service",
			"resource_name:query",
			"@click_dog.source:span_log",
			"@query_log.user:*",
			"$env",
			"$user",
			"$database",
			"$table",
			"$operation",
			"$access_type",
			"$host",
		} {
			if !strings.Contains(query, want) {
				t.Errorf("activity widget query missing %q: %q", want, query)
			}
		}
		for _, notWant := range []string{"@click_dog.source:query_log", "resource_name:clickhouse.query", "@db."} {
			if strings.Contains(query, notWant) {
				t.Errorf("activity widget query uses backfill-only filter %q: %q", notWant, query)
			}
		}
	}

	note := queryDashboardNoteContent(t, dash, "# Click-Dog — Exported User Activity")
	for _, want := range []string{"min_trace_duration_ms", "not total ClickHouse traffic", "not a complete audit record"} {
		if !strings.Contains(note, want) {
			t.Errorf("activity scope note missing %q", want)
		}
	}
}

func TestActivityDashboard_SearchableRelationshipTables(t *testing.T) {
	dash := loadActivityDashboard(t)
	want := map[string][]string{
		"Users and access types — exported activity": {"@query_log.user", "@query_log.access_type"},
		"User → database — exported activity":        {"@query_log.user", "@query_log.databases", "@query_log.access_type"},
		"User → table — exported activity":           {"@query_log.user", "@query_log.tables", "@query_log.operation"},
		"User → operation — exported activity":       {"@query_log.user", "@query_log.operation", "@query_log.access_type"},
	}

	for title, wantFields := range want {
		def := findWidgetDefinitionByTitle(t, dash, title)
		if got := def["type"]; got != "query_table" {
			t.Errorf("activity relationship widget %q type = %v, want query_table", title, got)
		}
		if got := def["has_search_bar"]; got != "always" {
			t.Errorf("activity relationship widget %q has_search_bar = %v, want always", title, got)
		}
		queries := widgetQueries(t, def)
		if len(queries) != 1 {
			t.Fatalf("activity relationship widget %q has %d queries, want 1", title, len(queries))
		}
		groupBy, ok := queries[0]["group_by"].(map[string]interface{})
		if !ok {
			t.Fatalf("activity relationship widget %q group_by has type %T, want flat object", title, queries[0]["group_by"])
		}
		rawFields, ok := groupBy["fields"].([]interface{})
		if !ok {
			t.Fatalf("activity relationship widget %q group_by.fields has type %T, want array", title, groupBy["fields"])
		}
		gotFields := make([]string, 0, len(rawFields))
		for _, raw := range rawFields {
			field, ok := raw.(string)
			if !ok {
				t.Fatalf("activity relationship widget %q field has type %T, want string", title, raw)
			}
			gotFields = append(gotFields, field)
		}
		if !slices.Equal(gotFields, wantFields) {
			t.Errorf("activity relationship widget %q fields = %v, want %v", title, gotFields, wantFields)
		}
	}
}

// queryDashboardWidgetTitles returns the title of every widget that has one
// (note widgets carry content, not a title, and are skipped).
func queryDashboardWidgetTitles(t *testing.T, dash map[string]interface{}) []string {
	t.Helper()
	widgets, ok := dash["widgets"].([]interface{})
	if !ok {
		t.Fatal("dashboard widgets missing or wrong type")
	}
	var out []string
	for _, widget := range widgets {
		obj, ok := widget.(map[string]interface{})
		if !ok {
			continue
		}
		def, ok := obj["definition"].(map[string]interface{})
		if !ok {
			continue
		}
		if title, ok := def["title"].(string); ok {
			out = append(out, title)
		}
	}
	return out
}

func noteRequiresLogComment(content string) bool {
	return strings.Contains(strings.ToLower(content), "@log_comment.")
}

func firstHeaderLine(content string) string {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			return strings.TrimPrefix(trimmed, "## ")
		}
	}
	return content
}

func findWidgetByTitle(t *testing.T, dash map[string]interface{}, title string) map[string]interface{} {
	t.Helper()
	widgets, ok := dash["widgets"].([]interface{})
	if !ok {
		t.Fatal("dashboard widgets missing or wrong type")
	}
	for _, widget := range widgets {
		widgetObj, ok := widget.(map[string]interface{})
		if !ok {
			t.Fatalf("widget has type %T, want object", widget)
		}
		def, ok := widgetObj["definition"].(map[string]interface{})
		if !ok {
			t.Fatalf("widget definition has type %T, want object", widgetObj["definition"])
		}
		if def["title"] == title {
			return widgetObj
		}
	}
	t.Fatalf("widget with title %q not found", title)
	return nil
}

func findWidgetDefinitionByTitle(t *testing.T, dash map[string]interface{}, title string) map[string]interface{} {
	t.Helper()
	return findWidgetByTitle(t, dash, title)["definition"].(map[string]interface{})
}

func queryDashboardNoteContent(t *testing.T, dash map[string]interface{}, marker string) string {
	t.Helper()
	widgets, ok := dash["widgets"].([]interface{})
	if !ok {
		t.Fatal("dashboard widgets missing or wrong type")
	}
	for _, widget := range widgets {
		widgetObj, ok := widget.(map[string]interface{})
		if !ok {
			continue
		}
		def, ok := widgetObj["definition"].(map[string]interface{})
		if !ok || def["type"] != "note" {
			continue
		}
		content, ok := def["content"].(string)
		if !ok {
			continue
		}
		if strings.Contains(content, marker) {
			return content
		}
	}
	t.Fatalf("note containing %q not found", marker)
	return ""
}

func widgetGroupByFacets(t *testing.T, def map[string]interface{}) []string {
	t.Helper()
	var facets []string
	for _, query := range widgetQueries(t, def) {
		groupBy, _ := query["group_by"].([]interface{})
		for _, item := range groupBy {
			obj, ok := item.(map[string]interface{})
			if !ok {
				t.Fatalf("group_by item has type %T, want object", item)
			}
			if facet, ok := obj["facet"].(string); ok {
				facets = append(facets, facet)
			}
		}
	}
	return facets
}

func widgetSearchQueries(t *testing.T, def map[string]interface{}) []string {
	t.Helper()
	var out []string
	for _, query := range widgetQueries(t, def) {
		search, ok := query["search"].(map[string]interface{})
		if !ok {
			continue
		}
		if s, ok := search["query"].(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func queryDashboardSpanSearchQueries(t *testing.T, dash map[string]interface{}) []string {
	t.Helper()
	widgets, ok := dash["widgets"].([]interface{})
	if !ok {
		t.Fatal("dashboard widgets missing or wrong type")
	}
	var out []string
	for i, widget := range widgets {
		widgetObj, ok := widget.(map[string]interface{})
		if !ok {
			t.Errorf("widget %d has type %T, want object", i, widget)
			continue
		}
		def, ok := widgetObj["definition"].(map[string]interface{})
		if !ok {
			t.Errorf("widget %d definition has type %T, want object", i, widgetObj["definition"])
			continue
		}
		title, _ := def["title"].(string)
		if title == "" {
			title = "<untitled>"
		}
		if def["type"] == "group" {
			t.Errorf("query dashboard does not use group widgets; update queryDashboardSpanSearchQueries before adding grouped widget %q", title)
			continue
		}
		rawRequests, ok := def["requests"]
		if !ok {
			continue
		}
		requests, ok := rawRequests.([]interface{})
		if !ok {
			t.Errorf("widget %q requests has type %T, want array", title, rawRequests)
			continue
		}
		for requestIndex, request := range requests {
			requestObj, ok := request.(map[string]interface{})
			if !ok {
				t.Errorf("widget %q request %d has type %T, want object", title, requestIndex, request)
				continue
			}
			rawQueries, ok := requestObj["queries"]
			if !ok {
				continue
			}
			queries, ok := rawQueries.([]interface{})
			if !ok {
				t.Errorf("widget %q request %d queries has type %T, want array", title, requestIndex, rawQueries)
				continue
			}
			for queryIndex, query := range queries {
				queryObj, ok := query.(map[string]interface{})
				if !ok {
					t.Errorf("widget %q request %d query %d has type %T, want object", title, requestIndex, queryIndex, query)
					continue
				}
				if queryObj["data_source"] != "spans" {
					continue
				}
				search, ok := queryObj["search"].(map[string]interface{})
				if !ok {
					t.Errorf("span query in widget %q missing search object", title)
					continue
				}
				s, ok := search["query"].(string)
				if !ok {
					t.Errorf("span query in widget %q missing string search.query", title)
					continue
				}
				out = append(out, s)
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("no span search queries found in query dashboard")
	}
	return out
}

func widgetQueries(t *testing.T, def map[string]interface{}) []map[string]interface{} {
	t.Helper()
	requests, ok := def["requests"].([]interface{})
	if !ok {
		t.Fatalf("widget %q missing requests", def["title"])
	}
	var out []map[string]interface{}
	for _, request := range requests {
		requestObj, ok := request.(map[string]interface{})
		if !ok {
			t.Fatalf("request has type %T, want object", request)
		}
		queries, ok := requestObj["queries"].([]interface{})
		if !ok {
			continue
		}
		for _, query := range queries {
			queryObj, ok := query.(map[string]interface{})
			if !ok {
				t.Fatalf("query has type %T, want object", query)
			}
			out = append(out, queryObj)
		}
	}
	return out
}

func firstRequestConditionalFormats(t *testing.T, def map[string]interface{}) []interface{} {
	t.Helper()
	requests, ok := def["requests"].([]interface{})
	if !ok || len(requests) == 0 {
		t.Fatalf("widget %q missing requests", def["title"])
	}
	request, ok := requests[0].(map[string]interface{})
	if !ok {
		t.Fatalf("first request has type %T, want object", requests[0])
	}
	formats, ok := request["conditional_formats"].([]interface{})
	if !ok {
		t.Fatalf("widget %q missing conditional_formats", def["title"])
	}
	return formats
}

func firstMatchingPalette(t *testing.T, formats []interface{}, value float64) string {
	t.Helper()
	for i, raw := range formats {
		format, ok := raw.(map[string]interface{})
		if !ok {
			t.Fatalf("conditional format %d has type %T, want object", i, raw)
		}
		comparator, ok := format["comparator"].(string)
		if !ok {
			t.Fatalf("conditional format %d comparator has type %T, want string", i, format["comparator"])
		}
		threshold, ok := format["value"].(float64)
		if !ok {
			t.Fatalf("conditional format %d value has type %T, want number", i, format["value"])
		}
		matches, supported := conditionalFormatMatches(value, comparator, threshold)
		if !supported {
			t.Fatalf("conditional format %d uses unsupported comparator %q; update conditionalFormatMatches", i, comparator)
		}
		if matches {
			palette, ok := format["palette"].(string)
			if !ok {
				t.Fatalf("conditional format %d palette has type %T, want string", i, format["palette"])
			}
			return palette
		}
	}
	t.Fatalf("no conditional format matched value %v", value)
	return ""
}

func conditionalFormatMatches(value float64, comparator string, threshold float64) (bool, bool) {
	switch comparator {
	case "=":
		return value == threshold, true
	case ">":
		return value > threshold, true
	case ">=":
		return value >= threshold, true
	case "<":
		return value < threshold, true
	case "<=":
		return value <= threshold, true
	default:
		return false, false
	}
}

// healthDashboardOpenMetricsNamespace is the namespace the documented Datadog
// Agent OpenMetrics scrape config uses. The dashboard, README, and integration
// docs all reference this namespace; changing it here without updating those
// in lockstep will trip TestHealthDashboard_NoteWidgetMatchesMapping.
const healthDashboardOpenMetricsNamespace = "click_dog"

// healthDashboardMetric describes one click-dog Prometheus metric, the Datadog
// Agent OpenMetrics rename it should be configured with, and the resulting
// Datadog metric name the health dashboard widgets must query.
//
// This list is the contract: it must match (a) the metrics emitted by
// internal/metrics.Metrics.Handler, (b) the YAML rename block in the
// dashboard's note widget, and (c) the metric names in every widget query.
// Each of those three is asserted by a test below.
type healthDashboardMetric struct {
	prom    string // Prometheus metric name as exposed at /metrics
	rename  string // value used after the colon in the OpenMetrics `metrics:` list
	counter bool   // true if Datadog should append `.count` (counters)
}

func healthDashboardOTLPNames() map[string]bool {
	out := make(map[string]bool, len(metrics.CanonicalDescriptors()))
	for _, d := range metrics.CanonicalDescriptors() {
		name := metrics.OTLPName(d, nil)
		if d.Kind == metrics.MetricKindCounter {
			name += ".count"
		}
		out[name] = true
	}
	return out
}

var healthDashboardMapping = []healthDashboardMetric{
	{prom: "click_dog_spans_exported_total", rename: "spans_exported", counter: true},
	{prom: "click_dog_spans_filtered_total", rename: "spans_filtered", counter: true},
	{prom: "click_dog_spans_duplicates_total", rename: "spans_duplicates", counter: true},
	{prom: "click_dog_export_attempts_total", rename: "export.attempts", counter: true},
	{prom: "click_dog_export_accepted_total", rename: "export.accepted", counter: true},
	{prom: "click_dog_export_errors_total", rename: "export.errors", counter: true},
	{prom: "click_dog_cycle_results_total", rename: "cycle_results", counter: true},
	{prom: "click_dog_circuit_breaker_state", rename: "circuit_breaker.state"},
	{prom: "click_dog_leader", rename: "leader"},
	{prom: "click_dog_backoff_interval_seconds", rename: "backoff_interval.seconds"},
	{prom: "click_dog_last_success_timestamp_seconds", rename: "last_success_timestamp.seconds"},
	{prom: "click_dog_uptime_seconds", rename: "uptime.seconds"},
	{prom: "click_dog_last_cycle_duration_seconds", rename: "last_cycle.duration_seconds"},
	{prom: "click_dog_last_cycle_exported_spans", rename: "last_cycle.exported_spans"},
	{prom: "click_dog_last_cycle_filtered_spans", rename: "last_cycle.filtered_spans"},
	{prom: "click_dog_last_cycle_duplicate_spans", rename: "last_cycle.duplicate_spans"},
	// ClickHouse data-plane health (#183). The note widget YAML, the
	// dashboards/README.md mapping table, the Datadog self-monitoring docs
	// table, and this slice must stay in lockstep — TestHealthDashboard_*
	// tests pin all four against each other.
	{prom: "click_dog_span_log_last_poll_timestamp_seconds", rename: "span_log.last_poll_timestamp.seconds"},
	{prom: "click_dog_span_log_newest_row_age_seconds", rename: "span_log.newest_row_age.seconds"},
	{prom: "click_dog_span_log_rows_last_cycle", rename: "span_log.rows_last_cycle"},
	{prom: "click_dog_query_log_enrichment_attempts_total", rename: "query_log.enrichment.attempts", counter: true},
	{prom: "click_dog_query_log_enrichment_successes_total", rename: "query_log.enrichment.successes", counter: true},
	{prom: "click_dog_query_log_enrichment_failures_total", rename: "query_log.enrichment.failures", counter: true},
	{prom: "click_dog_query_log_enrichment_match_ratio", rename: "query_log.enrichment.match_ratio"},
	{prom: "click_dog_spans_with_query_id_ratio", rename: "spans_with_query_id_ratio"},
	{prom: "click_dog_normalized_query_supported", rename: "normalized_query_supported"},
	{prom: "click_dog_query_operation_supported", rename: "query_operation_supported"},
	// Topology self-audit (sidecar + use_cluster_queries anti-pattern). Same
	// four-way lockstep as above — note widget YAML, README table, datadog.md
	// table, and this slice.
	{prom: "click_dog_topology_warning", rename: "topology_warning"},
}

// metricNameRE matches a Datadog metric reference inside a widget query. The
// queries look like `avg:click_dog.cycle_results.count{$host}` or
// `sum:click_dog.spans_exported.count{$host}.as_rate()` — the metric name is
// the dotted identifier between the aggregator colon and the `{` selector.
// TestHealthDashboard_MetricNameRegex pins the regex's behavior against
// representative samples so a silent regression here can't let
// TestHealthDashboard_QueriesMatchOTLPProfile pass with a partial
// extraction.
var metricNameRE = regexp.MustCompile(`(?:^|[^a-zA-Z0-9_.])([a-z][a-z0-9_]*(?:\.[a-z0-9_]+)+)\{`)

// TestHealthDashboard_QueriesMatchOTLPProfile is the contract test for the
// default OTLP self-metrics path. Every metric name a health-dashboard widget
// queries must match the canonical descriptor -> OTLP profile renderer, with
// Datadog's backend `.count` suffix for monotonic sums.
//
// Direction is one-way by design: queries ⊆ mapping. A mapping entry with no
// corresponding widget query is allowed (it represents an emitted metric
// the current dashboard layout doesn't surface yet). The reverse direction
// — every emitted metric must be in the mapping — is enforced by
// TestHealthDashboard_MappingCoversEmittedMetrics, which keeps the legacy
// OpenMetrics mapping from silently lagging behind internal/metrics.
func TestHealthDashboard_QueriesMatchOTLPProfile(t *testing.T) {
	dash := loadHealthDashboard(t)

	expected := healthDashboardOTLPNames()

	queried := healthDashboardQueriedMetrics(t, dash)
	if len(queried) == 0 {
		t.Fatal("no metric names extracted from health dashboard queries")
	}
	for name := range queried {
		if !expected[name] {
			t.Errorf("health dashboard queries metric %q which is not produced by the default OTLP profile; "+
				"update internal/metrics descriptors/profile or the dashboard query", name)
		}
	}
}

// TestHealthDashboard_NoteWidgetMatchesMapping verifies that the YAML rename
// list embedded in the dashboard's note widget exactly matches the
// healthDashboardMapping contract. If they drift, an operator copy-pasting
// the note's YAML into datadog.yaml ends up with a different set of metrics
// than the dashboard widgets expect.
func TestHealthDashboard_NoteWidgetMatchesMapping(t *testing.T) {
	dash := loadHealthDashboard(t)
	noteContent := scrapeConfigNoteContent(t, dash)

	parsed := parseRenamePairs(t, noteContent)
	want := make(map[string]string, len(healthDashboardMapping))
	for _, m := range healthDashboardMapping {
		want[m.prom] = m.rename
	}
	if len(parsed) != len(want) {
		t.Errorf("note widget defines %d rename pairs, mapping has %d: parsed=%v", len(parsed), len(want), parsed)
	}
	for prom, rename := range want {
		got, ok := parsed[prom]
		if !ok {
			t.Errorf("note widget missing rename for %q (expected %q)", prom, rename)
			continue
		}
		if got != rename {
			t.Errorf("note widget renames %q to %q, mapping says %q", prom, got, rename)
		}
	}
}

// TestHealthDashboard_MappingCoversEmittedMetrics verifies every metric
// emitted by internal/metrics.Metrics.Handler appears in the dashboard
// mapping. This catches the case where someone adds a new metric to
// internal/metrics without updating the dashboard, note widget, README,
// and integration docs together.
func TestHealthDashboard_MappingCoversEmittedMetrics(t *testing.T) {
	emitted := emittedPrometheusMetrics(t)

	mapped := make(map[string]bool, len(healthDashboardMapping))
	for _, m := range healthDashboardMapping {
		mapped[m.prom] = true
	}

	var missing []string
	for _, name := range emitted {
		if !mapped[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("metrics emitted at /metrics but not in healthDashboardMapping: %v\n"+
			"add them to dashboards/datadog-clickdog-health.json (note widget), "+
			"dashboards/README.md, docs/integrations/datadog/self-monitoring.md, and healthDashboardMapping", missing)
	}
}

// TestHealthDashboard_MetricNameRegex pins metricNameRE against the four
// query shapes the dashboard uses: bare scalar (`avg:foo.bar{$host}`),
// rate-wrapped (`sum:foo.bar.count{$host}.as_rate()`), formula reference
// (`time() - query1` — no metric expected), and aggregator-prefixed timeseries
// (`max:foo.bar{$host}`). Without this test, a regex change that silently
// stopped matching one of those shapes would let
// TestHealthDashboard_QueriesMatchOTLPProfile pass with a partial
// extraction (queries ⊆ mapping holds vacuously when fewer queries are seen).
func TestHealthDashboard_MetricNameRegex(t *testing.T) {
	cases := []struct {
		query string
		want  []string
	}{
		{"avg:click_dog.uptime_seconds{$host}", []string{"click_dog.uptime_seconds"}},
		{"sum:click_dog.spans_exported.count{$host}.as_rate()", []string{"click_dog.spans_exported.count"}},
		{"max:click_dog.circuit_breaker_state{$host}", []string{"click_dog.circuit_breaker_state"}},
		{"time() - query1", nil},
		// Tag-grouped form used by the cycle_results timeseries widget. The
		// `by {result}` clause has its own `{` but the inner identifier is a
		// single token (no dot), so the regex must NOT capture it as a
		// second metric.
		{"sum:click_dog.cycle_results.count{$host} by {result}.as_rate()", []string{"click_dog.cycle_results.count"}},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			matches := metricNameRE.FindAllStringSubmatch(tc.query, -1)
			var got []string
			for _, m := range matches {
				got = append(got, m[1])
			}
			if len(got) != len(tc.want) {
				t.Fatalf("metricNameRE captures from %q = %v, want %v", tc.query, got, tc.want)
			}
			for i, w := range tc.want {
				if got[i] != w {
					t.Errorf("metricNameRE captures[%d] from %q = %q, want %q", i, tc.query, got[i], w)
				}
			}
		})
	}
}

// TestHealthDashboard_WidgetIDsUnique guards against accidental ID
// collisions on merge. Datadog's import API silently keeps one widget
// and drops the other when two share an `id`, so a duplicate is a
// vanished tile with no error to point at. Cheap insurance for a JSON
// document that gets edited by hand.
func TestHealthDashboard_WidgetIDsUnique(t *testing.T) {
	dash := loadHealthDashboard(t)
	widgets, ok := dash["widgets"].([]interface{})
	if !ok {
		t.Fatal("dashboard widgets missing or wrong type")
	}
	seen := make(map[float64]int, len(widgets))
	for i, w := range widgets {
		obj, ok := w.(map[string]interface{})
		if !ok {
			t.Fatalf("widget %d has type %T, want object", i, w)
		}
		raw, ok := obj["id"]
		if !ok {
			t.Errorf("widget %d missing id field", i)
			continue
		}
		// JSON numbers decode to float64 through encoding/json — the IDs
		// in the dashboard JSON are integers, but the Go-level type is
		// float64. Comparing as float64 is exact for values that fit.
		id, ok := raw.(float64)
		if !ok {
			t.Errorf("widget %d id has type %T, want number", i, raw)
			continue
		}
		if first, dup := seen[id]; dup {
			t.Errorf("widget id %v duplicated at indices %d and %d", id, first, i)
		}
		seen[id] = i
	}
}

func loadHealthDashboard(t *testing.T) map[string]interface{} {
	t.Helper()
	data, err := embeddedDashboards.ReadFile("dashboards/datadog-clickdog-health.json")
	if err != nil {
		t.Fatalf("reading health dashboard: %v", err)
	}
	var dash map[string]interface{}
	if err := json.Unmarshal(data, &dash); err != nil {
		t.Fatalf("health dashboard is not valid JSON: %v", err)
	}
	return dash
}

func loadQueryDashboard(t *testing.T) map[string]interface{} {
	t.Helper()
	data, err := embeddedDashboards.ReadFile("dashboards/datadog-query-analysis.json")
	if err != nil {
		t.Fatalf("reading query dashboard: %v", err)
	}
	var dash map[string]interface{}
	if err := json.Unmarshal(data, &dash); err != nil {
		t.Fatalf("query dashboard is not valid JSON: %v", err)
	}
	return dash
}

func loadActivityDashboard(t *testing.T) map[string]interface{} {
	t.Helper()
	data, err := embeddedDashboards.ReadFile("dashboards/datadog-user-activity.json")
	if err != nil {
		t.Fatalf("reading activity dashboard: %v", err)
	}
	var dash map[string]interface{}
	if err := json.Unmarshal(data, &dash); err != nil {
		t.Fatalf("activity dashboard is not valid JSON: %v", err)
	}
	return dash
}

func healthDashboardQueriedMetrics(t *testing.T, dash map[string]interface{}) map[string]bool {
	t.Helper()
	out := make(map[string]bool)
	widgets, ok := dash["widgets"].([]interface{})
	if !ok {
		t.Fatal("dashboard widgets missing or wrong type")
	}
	for _, w := range widgets {
		walkQueries(w, func(q string) {
			for _, match := range metricNameRE.FindAllStringSubmatch(q, -1) {
				out[match[1]] = true
			}
		})
	}
	return out
}

func walkQueries(node interface{}, visit func(string)) {
	switch v := node.(type) {
	case map[string]interface{}:
		for k, val := range v {
			if k == "query" {
				if s, ok := val.(string); ok {
					visit(s)
				}
			}
			walkQueries(val, visit)
		}
	case []interface{}:
		for _, item := range v {
			walkQueries(item, visit)
		}
	}
}

// scrapeConfigNoteContent returns the content of the note widget that holds
// the OpenMetrics scrape-config block. It selects by content (the
// `namespace: <ns>` line) rather than widget id or position so the contract
// test stays correct if widgets are reordered or a second note widget (e.g.
// a layout separator) is added later.
func scrapeConfigNoteContent(t *testing.T, dash map[string]interface{}) string {
	t.Helper()
	widgets, ok := dash["widgets"].([]interface{})
	if !ok {
		t.Fatal("dashboard widgets missing or wrong type")
	}
	marker := "namespace: " + healthDashboardOpenMetricsNamespace
	for _, w := range widgets {
		obj, ok := w.(map[string]interface{})
		if !ok {
			continue
		}
		def, ok := obj["definition"].(map[string]interface{})
		if !ok {
			continue
		}
		if def["type"] != "note" {
			continue
		}
		content, ok := def["content"].(string)
		if !ok {
			t.Fatalf("note widget content has type %T, want string", def["content"])
		}
		if strings.Contains(content, marker) {
			return content
		}
	}
	t.Fatalf("no note widget containing %q found in health dashboard", marker)
	return ""
}

// renamePairRE captures `      - click_dog_foo_total: foo.bar` lines from the
// note widget's embedded YAML. Anchored to a leading dash so prose mentions
// of metric names elsewhere in the note are not parsed as renames. The
// content of the note widget is implicitly load-bearing for
// TestHealthDashboard_NoteWidgetMatchesMapping: changes to indentation,
// quoting, or comment style in the embedded YAML must keep these lines
// matchable by this regex (any leading whitespace, single `-` prefix,
// snake_case Prometheus name, dotted Datadog name).
var renamePairRE = regexp.MustCompile(`(?m)^\s*-\s+(click_dog_[a-z0-9_]+)\s*:\s*([a-z][a-z0-9_.]*)\s*$`)

// parseRenamePairs extracts Prometheus → Datadog rename pairs from the note
// widget's embedded YAML. Duplicates fail the test: a stray copy of the same
// rename line would silently overwrite the first occurrence and lead to a
// misleading "0 rename pairs found" or wrong-value error from the calling
// test.
func parseRenamePairs(t *testing.T, yaml string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	for _, m := range renamePairRE.FindAllStringSubmatch(yaml, -1) {
		if existing, dup := out[m[1]]; dup {
			t.Errorf("note widget has duplicate rename for %q (first %q, then %q)", m[1], existing, m[2])
		}
		out[m[1]] = m[2]
	}
	return out
}

// emittedPrometheusMetrics returns the click-dog metric names exposed by
// internal/metrics.Metrics.Handler. We exercise the real handler rather than
// duplicating the metric list in this test so adding a new metric to
// internal/metrics is enough to surface it here.
//
// We filter to the `click_dog_` prefix so the test is not sensitive to which
// Prometheus registry the handler uses. The current handler hand-writes the
// click-dog text exposition format and emits nothing else, but if a future
// refactor switched to e.g. promhttp on the default registerer this filter
// keeps the test from drowning in `go_*`, `process_*`, and
// `promhttp_metric_handler_requests_total` noise.
func emittedPrometheusMetrics(t *testing.T) []string {
	t.Helper()
	m := metrics.NewMetrics()
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading metrics body: %v", err)
	}

	seen := make(map[string]bool)
	for _, line := range strings.Split(string(buf), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// `metric_name value` or `metric_name{labels} value`
		end := strings.IndexAny(line, " {")
		if end <= 0 {
			continue
		}
		name := line[:end]
		if !strings.HasPrefix(name, "click_dog_") {
			continue
		}
		seen[name] = true
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// queryDashboardWidgetByTitle returns the definition of the first widget whose
// title matches, so threshold/format assertions don't depend on widget order.
func queryDashboardWidgetByTitle(t *testing.T, dash map[string]interface{}, title string) map[string]interface{} {
	t.Helper()
	widgets, ok := dash["widgets"].([]interface{})
	if !ok {
		t.Fatal("dashboard widgets missing or wrong type")
	}
	for _, w := range widgets {
		obj, ok := w.(map[string]interface{})
		if !ok {
			continue
		}
		def, ok := obj["definition"].(map[string]interface{})
		if !ok {
			continue
		}
		if def["title"] == title {
			return def
		}
	}
	t.Fatalf("query dashboard has no widget titled %q", title)
	return nil
}

// TestQueryDashboard_WidgetIDsUnique guards against duplicate widget IDs as the
// dashboard grows — Datadog tolerates dupes on import but they make later
// programmatic edits (and overlap reasoning) ambiguous.
func TestQueryDashboard_WidgetIDsUnique(t *testing.T) {
	dash := loadQueryDashboard(t)
	widgets, ok := dash["widgets"].([]interface{})
	if !ok {
		t.Fatal("dashboard widgets missing or wrong type")
	}
	seen := make(map[float64]string, len(widgets))
	for i, w := range widgets {
		obj, ok := w.(map[string]interface{})
		if !ok {
			t.Errorf("widget %d has type %T, want object", i, w)
			continue
		}
		id, ok := obj["id"].(float64)
		if !ok {
			t.Errorf("widget %d missing numeric id", i)
			continue
		}
		def, _ := obj["definition"].(map[string]interface{})
		title, _ := def["title"].(string)
		if prev, dup := seen[id]; dup {
			t.Errorf("duplicate widget id %.0f: %q and %q", id, prev, title)
		}
		seen[id] = title
	}
}

// TestQueryDashboard_LatencyTilesAndFailures locks two decisions from the
// dashboard-improvements PR:
//   - the Failures tile keys off the exception-code facet, and
//   - the latency tiles ship a sparkline but NO color thresholds. We have no
//     way to know an operator's target latency, so the tiles assert no
//     good/bad opinion by default (a documented conditional_formats block lives
//     in dashboards/README.md for operators who do have an SLO). This guards
//     against someone re-introducing arbitrary thresholds.
func TestQueryDashboard_LatencyTilesAndFailures(t *testing.T) {
	dash := loadQueryDashboard(t)

	// Failed-queries KPI filters on the exception-code facet.
	failed := queryDashboardWidgetByTitle(t, dash, "Failed queries")
	requests, _ := failed["requests"].([]interface{})
	if len(requests) == 0 {
		t.Fatal(`"Failed queries" widget has no requests`)
	}
	req0, _ := requests[0].(map[string]interface{})
	queries, _ := req0["queries"].([]interface{})
	if len(queries) == 0 {
		t.Fatal(`"Failed queries" widget request has no queries`)
	}
	q0, _ := queries[0].(map[string]interface{})
	search, _ := q0["search"].(map[string]interface{})
	if s, _ := search["query"].(string); !strings.Contains(s, "@query_log.exception_code:>0") {
		t.Errorf(`"Failed queries" must filter on @query_log.exception_code:>0; got %q`, s)
	}

	// Latency tiles: sparkline present, color thresholds absent (no SLO opinion).
	for _, title := range []string{"p95 latency", "p99 latency"} {
		def := queryDashboardWidgetByTitle(t, dash, title)
		if _, ok := def["timeseries_background"].(map[string]interface{}); !ok {
			t.Errorf("%s should keep its timeseries_background sparkline", title)
		}
		reqs, _ := def["requests"].([]interface{})
		for i, raw := range reqs {
			req, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if _, has := req["conditional_formats"]; has {
				t.Errorf("%s request %d carries conditional_formats; latency tiles ship without color thresholds (we don't know the operator's SLO) — add a documented block to README instead", title, i)
			}
		}
	}
}

// --- dashboard update flow ------------------------------------------------

// fakeDDClient records calls so reconcileDashboard can be exercised without
// touching the network.
type fakeDDClient struct {
	dashboards []ddDashboardSummary
	listErr    error
	created    [][]byte
	updated    map[string][]byte
}

func (f *fakeDDClient) list() ([]ddDashboardSummary, error) { return f.dashboards, f.listErr }
func (f *fakeDDClient) create(body []byte) (string, error) {
	f.created = append(f.created, body)
	return "/dashboard/new-id", nil
}
func (f *fakeDDClient) update(id string, body []byte) (string, error) {
	if f.updated == nil {
		f.updated = map[string][]byte{}
	}
	f.updated[id] = body
	return "/dashboard/" + id, nil
}

// queryDashFingerprint returns the embedded query dashboard's content hash and
// title — the two values reconcileDashboard keys on.
func queryDashFingerprint(t *testing.T) (hash, title string) {
	t.Helper()
	raw, err := embeddedDashboards.ReadFile("dashboards/datadog-query-analysis.json")
	if err != nil {
		t.Fatalf("reading embedded query dashboard: %v", err)
	}
	var d map[string]interface{}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("parsing embedded query dashboard: %v", err)
	}
	return sha256Hex(raw), d["title"].(string)
}

// markerString builds a dashboard description carrying a marker, using the real
// stampDashboardMarker so the fixture can't drift from the production format.
func markerString(name, ver, hash string) string {
	d := map[string]interface{}{"description": "Some prose."}
	stampDashboardMarker(d, name, ver, hash)
	return d["description"].(string)
}

func TestDashboardMarker_RoundTrip(t *testing.T) {
	dash := map[string]interface{}{"description": "Original prose."}
	stampDashboardMarker(dash, "query", "v26.06.3-beta", "abc123")
	desc := dash["description"].(string)
	if !strings.Contains(desc, "Original prose.") {
		t.Errorf("stamp dropped existing prose: %q", desc)
	}
	mk, ok := parseDashboardMarker(desc)
	if !ok {
		t.Fatalf("marker not parseable after stamp: %q", desc)
	}
	if mk.name != "query" || mk.version != "v26.06.3-beta" || mk.hash != "abc123" {
		t.Errorf("round-trip mismatch: %+v", mk)
	}

	// Re-stamping replaces, never duplicates.
	stampDashboardMarker(dash, "query", "v99", "def456")
	desc = dash["description"].(string)
	if got := strings.Count(desc, "<!-- clickdog:"); got != 1 {
		t.Errorf("expected exactly one marker after re-stamp, got %d in %q", got, desc)
	}
	mk, _ = parseDashboardMarker(desc)
	if mk.hash != "def456" {
		t.Errorf("re-stamp hash = %q, want def456", mk.hash)
	}

	if _, ok := parseDashboardMarker("no marker here"); ok {
		t.Error("parseDashboardMarker reported a marker where there is none")
	}
}

func TestResolveOnExists_FlagAndHeadless(t *testing.T) {
	var out bytes.Buffer
	// Explicit --on-exists wins, even on a terminal.
	for _, want := range []string{"skip", "overwrite", "new"} {
		got := resolveOnExists(reconcileOpts{onExists: want, interactive: true}, dashboardByName(t, "query"), &out)
		if got != want {
			t.Errorf("onExists=%q resolved to %q", want, got)
		}
	}
	// No flag + not a terminal → skip (never block or surprise-overwrite).
	if got := resolveOnExists(reconcileOpts{onExists: "", interactive: false}, dashboardByName(t, "query"), &out); got != "skip" {
		t.Errorf("headless default = %q, want skip", got)
	}
}

func TestPromptOnExists(t *testing.T) {
	cases := map[string]string{
		"o\n":         "overwrite",
		"overwrite\n": "overwrite",
		"n\n":         "new",
		"s\n":         "skip",
		"\n":          "skip", // bare Enter takes the [s] default
		"":            "skip", // EOF
		"huh?\nn\n":   "new",  // re-prompt past garbage
	}
	for input, want := range cases {
		var out bytes.Buffer
		got := promptOnExists(strings.NewReader(input), &out, dashboardByName(t, "query"))
		if got != want {
			t.Errorf("input %q -> %q, want %q", input, got, want)
		}
	}
}

func TestReconcileDashboard(t *testing.T) {
	hash, title := queryDashFingerprint(t)
	queryDef := dashboardByName(t, "query")

	t.Run("creates when absent", func(t *testing.T) {
		c := &fakeDDClient{dashboards: []ddDashboardSummary{{ID: "x", Title: "Unrelated"}}}
		var out bytes.Buffer
		if err := reconcileDashboard(queryDef, c, reconcileOpts{onExists: "skip"}, &out); err != nil {
			t.Fatal(err)
		}
		if len(c.created) != 1 || len(c.updated) != 0 {
			t.Fatalf("want 1 create/0 update, got %d/%d", len(c.created), len(c.updated))
		}
		// Created body carries a marker with the current content hash.
		var body map[string]interface{}
		_ = json.Unmarshal(c.created[0], &body)
		mk, ok := parseDashboardMarker(body["description"].(string))
		if !ok || mk.hash != hash {
			t.Errorf("created body marker = %+v (ok=%v), want hash %s", mk, ok, hash)
		}
	})

	t.Run("skips when up to date", func(t *testing.T) {
		c := &fakeDDClient{dashboards: []ddDashboardSummary{
			{ID: "abc", Title: title, Description: markerString("query", "v1", hash)},
		}}
		var out bytes.Buffer
		// overwrite is forced, but an up-to-date match short-circuits before it.
		if err := reconcileDashboard(queryDef, c, reconcileOpts{onExists: "overwrite"}, &out); err != nil {
			t.Fatal(err)
		}
		if len(c.created) != 0 || len(c.updated) != 0 {
			t.Errorf("up-to-date should be a no-op, got %d create/%d update", len(c.created), len(c.updated))
		}
		if !strings.Contains(out.String(), "up to date") {
			t.Errorf("missing up-to-date message: %q", out.String())
		}
	})

	t.Run("overwrites when stale", func(t *testing.T) {
		c := &fakeDDClient{dashboards: []ddDashboardSummary{
			{ID: "abc", Title: title, Description: markerString("query", "v0", "deadbeef")},
		}}
		var out bytes.Buffer
		if err := reconcileDashboard(queryDef, c, reconcileOpts{onExists: "overwrite"}, &out); err != nil {
			t.Fatal(err)
		}
		body, ok := c.updated["abc"]
		if !ok {
			t.Fatalf("expected PUT to id abc, updates=%v", c.updated)
		}
		var parsed map[string]interface{}
		_ = json.Unmarshal(body, &parsed)
		mk, _ := parseDashboardMarker(parsed["description"].(string))
		if mk.hash != hash {
			t.Errorf("overwrite body marker hash = %q, want current %q", mk.hash, hash)
		}
	})

	t.Run("creates renamed copy on new", func(t *testing.T) {
		c := &fakeDDClient{dashboards: []ddDashboardSummary{
			{ID: "abc", Title: title, Description: markerString("query", "v0", "deadbeef")},
		}}
		var out bytes.Buffer
		if err := reconcileDashboard(queryDef, c, reconcileOpts{onExists: "new"}, &out); err != nil {
			t.Fatal(err)
		}
		if len(c.created) != 1 || len(c.updated) != 0 {
			t.Fatalf("want 1 create/0 update, got %d/%d", len(c.created), len(c.updated))
		}
		var body map[string]interface{}
		_ = json.Unmarshal(c.created[0], &body)
		if got := body["title"].(string); !strings.HasPrefix(got, title+" (") {
			t.Errorf("new copy title = %q, want disambiguated from %q", got, title)
		}
	})

	t.Run("skips stale when told to skip", func(t *testing.T) {
		c := &fakeDDClient{dashboards: []ddDashboardSummary{
			{ID: "abc", Title: title, Description: markerString("query", "v0", "deadbeef")},
		}}
		var out bytes.Buffer
		if err := reconcileDashboard(queryDef, c, reconcileOpts{onExists: "skip"}, &out); err != nil {
			t.Fatal(err)
		}
		if len(c.created) != 0 || len(c.updated) != 0 {
			t.Errorf("skip should be a no-op, got %d create/%d update", len(c.created), len(c.updated))
		}
	})

	t.Run("unmarked existing defaults to skip when headless", func(t *testing.T) {
		c := &fakeDDClient{dashboards: []ddDashboardSummary{
			{ID: "abc", Title: title, Description: "a hand-made dashboard, no marker"},
		}}
		var out bytes.Buffer
		if err := reconcileDashboard(queryDef, c, reconcileOpts{onExists: "", interactive: false}, &out); err != nil {
			t.Fatal(err)
		}
		if len(c.created) != 0 || len(c.updated) != 0 {
			t.Errorf("unmarked + headless should skip, got %d create/%d update", len(c.created), len(c.updated))
		}
	})

	t.Run("errors on duplicate titles without id", func(t *testing.T) {
		c := &fakeDDClient{dashboards: []ddDashboardSummary{
			{ID: "a", Title: title, Description: markerString("query", "v0", "deadbeef")},
			{ID: "b", Title: title, Description: markerString("query", "v0", "deadbeef")},
		}}
		var out bytes.Buffer
		err := reconcileDashboard(queryDef, c, reconcileOpts{onExists: "overwrite"}, &out)
		if err == nil || !strings.Contains(err.Error(), "--id") {
			t.Fatalf("want --id disambiguation error, got %v", err)
		}
	})

	t.Run("acts on chosen id among duplicates", func(t *testing.T) {
		dups := func() []ddDashboardSummary {
			return []ddDashboardSummary{
				{ID: "a", Title: title, Description: markerString("query", "v0", "deadbeef")},
				{ID: "b", Title: title, Description: markerString("query", "v0", "deadbeef")},
			}
		}
		t.Run("overwrite", func(t *testing.T) {
			c := &fakeDDClient{dashboards: dups()}
			var out bytes.Buffer
			if err := reconcileDashboard(queryDef, c, reconcileOpts{onExists: "overwrite", id: "b"}, &out); err != nil {
				t.Fatal(err)
			}
			if _, ok := c.updated["b"]; !ok || len(c.updated) != 1 || len(c.created) != 0 {
				t.Errorf("want only id b updated, got created=%d updated=%v", len(c.created), c.updated)
			}
		})
		t.Run("new", func(t *testing.T) {
			c := &fakeDDClient{dashboards: dups()}
			var out bytes.Buffer
			if err := reconcileDashboard(queryDef, c, reconcileOpts{onExists: "new", id: "b"}, &out); err != nil {
				t.Fatal(err)
			}
			if len(c.created) != 1 || len(c.updated) != 0 {
				t.Errorf("new should create a copy and not update, got created=%d updated=%v", len(c.created), c.updated)
			}
		})
		t.Run("skip", func(t *testing.T) {
			c := &fakeDDClient{dashboards: dups()}
			var out bytes.Buffer
			if err := reconcileDashboard(queryDef, c, reconcileOpts{onExists: "skip", id: "b"}, &out); err != nil {
				t.Fatal(err)
			}
			if len(c.created) != 0 || len(c.updated) != 0 {
				t.Errorf("skip should be a no-op, got created=%d updated=%v", len(c.created), c.updated)
			}
		})
	})

	t.Run("errors when --id misses the single match", func(t *testing.T) {
		c := &fakeDDClient{dashboards: []ddDashboardSummary{
			{ID: "abc", Title: title, Description: markerString("query", "v0", "deadbeef")},
		}}
		var out bytes.Buffer
		err := reconcileDashboard(queryDef, c, reconcileOpts{onExists: "overwrite", id: "wrong"}, &out)
		if err == nil || !strings.Contains(err.Error(), "--id") {
			t.Fatalf("want stray --id error, got %v", err)
		}
		if len(c.updated) != 0 {
			t.Errorf("must not act on a non-matching --id, updated=%v", c.updated)
		}
	})

	t.Run("propagates list error", func(t *testing.T) {
		c := &fakeDDClient{listErr: errFakeList}
		var out bytes.Buffer
		err := reconcileDashboard(queryDef, c, reconcileOpts{onExists: "skip"}, &out)
		if err == nil || !strings.Contains(err.Error(), "listing dashboards") {
			t.Fatalf("want wrapped list error, got %v", err)
		}
	})
}

var errFakeList = errors.New("boom")

// TestQueryDashboard_WidgetTypesAreImportable pins widget types to the ones
// we've actually imported — an API-invalid distribution widget passed our
// structure tests but broke the whole dashboard POST in v26.06.4.
func TestQueryDashboard_WidgetTypesAreImportable(t *testing.T) {
	importable := map[string]bool{
		"note":        true,
		"query_value": true,
		"toplist":     true,
		"timeseries":  true,
	}
	dash := loadQueryDashboard(t)
	widgets, ok := dash["widgets"].([]interface{})
	if !ok {
		t.Fatal("dashboard widgets missing or wrong type")
	}
	for i, w := range widgets {
		obj, ok := w.(map[string]interface{})
		if !ok {
			t.Fatalf("widget %d has type %T, want object", i, w)
		}
		def, ok := obj["definition"].(map[string]interface{})
		if !ok {
			t.Fatalf("widget %d definition has type %T, want object", i, obj["definition"])
		}
		typ, _ := def["type"].(string)
		if !importable[typ] {
			t.Errorf("widget %d (%v) uses type %q not in the import-verified allowlist; verify it imports against the Datadog API before adding, then widen this test", i, def["title"], typ)
		}
	}
}
