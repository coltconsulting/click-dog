package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/coltconsulting/click-dog/internal/config"
)

// maxDashboardResponseBytes bounds a Datadog dashboard API response. The list
// endpoint returns a summary per dashboard in the org, so the cap is far above
// the event client's 64 KiB, but an unbounded io.ReadAll would still let a
// misbehaving endpoint pin memory — and on a non-2xx the body is interpolated
// into the returned error, holding it a second time. Oversize is an error
// rather than a silent truncation, so a cut-off body never reaches json.Unmarshal
// as a confusing parse failure.
const maxDashboardResponseBytes int64 = 8 << 20

//go:embed dashboards/datadog-query-analysis.json dashboards/datadog-user-activity.json dashboards/datadog-clickdog-health.json
var embeddedDashboards embed.FS

// dashboardDef describes one of the shipped dashboards.
type dashboardDef struct {
	name        string // short name for --dashboard flag
	filename    string // path under dashboards/
	description string
	// prerequisites is the post-import checklist printed after the dashboard
	// is created. Each shipped dashboard reads a different data surface, so a
	// fresh import can render blank ("No data") unless the matching ingestion
	// is wired up. Surfacing the requirement at import time is the whole point
	// of issue #178 — TestCreateDashboards_* pins the Health wording.
	prerequisites []string
}

var shippedDashboards = []dashboardDef{
	{
		name:        "query",
		filename:    "datadog-query-analysis.json",
		description: "Application query analysis (span-based) — traces grouped by app and query name",
		prerequisites: []string{
			"Reads exported spans (traces). Make sure click-dog is exporting to a",
			"collector that forwards to Datadog (exporters.otel in your config).",
			"Widgets filter on the `service` template variable (default",
			"click-dog-monitor) — set it to your exporters.otel service_name.",
		},
	},
	{
		name:        "activity",
		filename:    "datadog-user-activity.json",
		description: "Exported user activity (span-based) — searchable user, database, table, and operation relationships",
		prerequisites: []string{
			"Reads exported root query spans (traces) with query_log enrichment.",
			"Keep `monitor.enrich_from_query_log: true` and make sure click-dog can",
			"read system.query_log; user/database/table/operation fields come from it.",
			"Widgets filter on the `service` template variable (default",
			"click-dog-monitor) — set it to your exporters.otel service_name.",
			"Counts are the qualified/exported stream selected by duration and filters,",
			"not total ClickHouse traffic or a complete audit log.",
		},
	},
	{
		name:        "health",
		filename:    "datadog-clickdog-health.json",
		description: "Click-dog self-monitoring (metrics-based) — cycles, exports, errors, backoff, circuit breaker",
		prerequisites: []string{
			"Reads click-dog's own self-metrics over OTLP — not trace spans.",
			"Enable `metrics.otlp.enabled: true`; by default it reuses",
			"`exporters.otel[0]` so metrics flow to the same OTLP collector as spans.",
			"Set `metrics.otlp.host` in containers if you want a stable host.name",
			"dashboard variable instead of the container/pod hostname.",
			"Prometheus-only users can still scrape :9090/metrics with the legacy",
			"Datadog Agent OpenMetrics rename list documented in docs/integrations/datadog/self-monitoring.md.",
		},
	},
}

// onExistsActions are the valid --on-exists values (merge is a separate,
// not-yet-implemented capability — see docs/development/specs/dashboard-updates.md).
var onExistsActions = map[string]bool{"skip": true, "overwrite": true, "new": true}

func runCreateDashboards(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("create-dashboards", flag.ContinueOnError)
	fs.SetOutput(errOut)
	ddSite := fs.String("site", envOrDefault("DD_SITE", "datadoghq.com"), "trusted Datadog API hostname (e.g. datadoghq.com, datadoghq.eu)")
	which := fs.String("dashboard", "all", "Which dashboard to create: query, activity, health, or all")
	onExists := fs.String("on-exists", "", "Action when a stock dashboard already exists: skip, overwrite, or new. Default: prompt on a terminal, else skip.")
	dashID := fs.String("id", "", "Dashboard ID to act on when several dashboards share the same title")

	fs.Usage = func() {
		_, _ = fmt.Fprintf(errOut, `click-dog create-dashboards — create or update Click-Dog dashboards in Datadog

Creates one or more of the Click-Dog dashboards via the Datadog API:

  query    — Application query analysis (span-based)
             Traces grouped by app and query_name via log_comment tagging
  activity — Exported user activity (span-based)
             Searchable user, database, table, and operation relationships
  health   — Click-Dog self-monitoring (metrics-based)
             Cycles, exports, errors, backoff, circuit breaker state
             Needs metrics.otlp.enabled and an OTLP-capable collector;
             a post-import checklist is printed after creation.

Re-running is safe: each dashboard is stamped with a version marker, so a
later run detects an already-imported dashboard. If it matches this build it
is left untouched; if it differs you are offered overwrite / new / skip
(prompted on a terminal, or set --on-exists for headless use).

Required environment variables:
  DD_API_KEY   Datadog API key
  DD_APP_KEY   Datadog application key
  DD_SITE      Trusted Datadog API hostname (default: datadoghq.com)

Your keys are sent directly to Datadog and are NOT stored by click-dog.

Usage:
  DD_API_KEY=... DD_APP_KEY=... click-dog create-dashboards [flags]

Flags:
`)
		printFlagDefaults(errOut, fs)
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if *onExists != "" && !onExistsActions[*onExists] {
		_, _ = fmt.Fprintf(errOut, "Error: invalid --on-exists %q (expected one of: skip, overwrite, new)\n", *onExists)
		return 1
	}

	apiKey := os.Getenv("DD_API_KEY")
	appKey := os.Getenv("DD_APP_KEY")
	if apiKey == "" || appKey == "" {
		_, _ = fmt.Fprintln(errOut, "Error: DD_API_KEY and DD_APP_KEY environment variables are required.")
		_, _ = fmt.Fprintln(errOut, "Usage: DD_API_KEY=... DD_APP_KEY=... click-dog create-dashboards")
		return 1
	}

	site, err := validateDatadogSite(*ddSite)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "Error: invalid DD_SITE %q: %v\n", *ddSite, err)
		return 1
	}
	*ddSite = site

	// Select dashboards to act on. Valid names come from shippedDashboards
	// directly so adding a new dashboard only requires one change.
	var toReconcile []dashboardDef
	for _, d := range shippedDashboards {
		if *which == "all" || d.name == *which {
			toReconcile = append(toReconcile, d)
		}
	}
	if len(toReconcile) == 0 {
		validNames := make([]string, 0, len(shippedDashboards)+1)
		for _, d := range shippedDashboards {
			validNames = append(validNames, d.name)
		}
		validNames = append(validNames, "all")
		_, _ = fmt.Fprintf(errOut, "Error: unknown dashboard %q (expected one of: %s)\n", *which, strings.Join(validNames, ", "))
		return 1
	}

	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "  Create Datadog Dashboards")
	_, _ = fmt.Fprintln(out, "  ═════════════════════════")
	_, _ = fmt.Fprintf(out, "  Site: %s\n", *ddSite)
	_, _ = fmt.Fprintln(out)

	client := &httpDDClient{site: *ddSite, apiKey: apiKey, appKey: appKey, http: newDatadogHTTPClient()}
	opts := reconcileOpts{
		site:     *ddSite,
		onExists: *onExists,
		id:       *dashID,
		in:       os.Stdin,
		// Prompt only when no action was forced and stdin is a real terminal;
		// otherwise (CI, pipes) default to skip so a re-run never hangs waiting
		// for input or surprises an operator by overwriting.
		interactive: *onExists == "" && term.IsTerminal(int(os.Stdin.Fd())),
	}

	anyFailed := false
	for _, d := range toReconcile {
		if err := reconcileDashboard(d, client, opts, out); err != nil {
			_, _ = fmt.Fprintf(errOut, "  [%s] FAIL: %v\n", d.name, err)
			anyFailed = true
		}
	}

	if anyFailed {
		return 1
	}
	return 0
}

// validateDatadogSite accepts a strict DNS hostname without encoding Datadog's
// evolving site catalogue. DD_SITE is trusted operator configuration; this
// validation prevents URL-authority smuggling through userinfo, ports, paths,
// or encoded delimiters while allowing future and custom Datadog sites.
func validateDatadogSite(raw string) (string, error) {
	return config.ValidateDatadogSite(raw)
}

func newDatadogHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		// Dashboard API calls do not require redirects. Refusing them prevents a
		// future or malicious endpoint from forwarding Datadog credentials.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// reconcileOpts carries the per-run update policy down to reconcileDashboard.
type reconcileOpts struct {
	site        string
	onExists    string    // "", skip, overwrite, new
	id          string    // disambiguates when several dashboards share a title
	in          io.Reader // interactive prompt source
	interactive bool      // prompt when an existing, differing dashboard is found
}

// reconcileDashboard creates the dashboard if absent, skips it if the live copy
// already matches this build, or (when it differs) overwrites / creates-new /
// skips per the resolved action.
func reconcileDashboard(d dashboardDef, client ddClient, opts reconcileOpts, out io.Writer) error {
	_, _ = fmt.Fprintf(out, "  [%s] %s\n", d.name, d.description)

	// Read embedded dashboard JSON — ships with the binary, version-locked.
	raw, err := embeddedDashboards.ReadFile("dashboards/" + d.filename)
	if err != nil {
		return fmt.Errorf("reading embedded dashboard %s: %w", d.filename, err)
	}
	// The content hash is taken over the canonical embedded bytes, before any
	// marker stamping, so it is a stable identity for "what this build ships"
	// regardless of how Datadog rewrites the document on import.
	hash := sha256Hex(raw)

	var dash map[string]interface{}
	if err := json.Unmarshal(raw, &dash); err != nil {
		return fmt.Errorf("parsing dashboard JSON: %w", err)
	}
	if _, ok := dash["layout_type"]; !ok {
		dash["layout_type"] = "ordered"
	}
	title, _ := dash["title"].(string)

	all, err := client.list()
	if err != nil {
		return fmt.Errorf("listing dashboards: %w", err)
	}
	var matches []ddDashboardSummary
	for _, s := range all {
		if s.Title == title {
			matches = append(matches, s)
		}
	}

	// Resolve which live dashboard (if any) we are reconciling against.
	var target *ddDashboardSummary
	switch len(matches) {
	case 0:
		// none — create below
	case 1:
		// A stray --id that names a different dashboard is a mistake, not a
		// no-op — fail rather than silently acting on the single match.
		if opts.id != "" && matches[0].ID != opts.id {
			return fmt.Errorf("--id %q does not match the dashboard titled %q (id %s)", opts.id, title, matches[0].ID)
		}
		target = &matches[0]
	default:
		if opts.id == "" {
			ids := make([]string, len(matches))
			for i, m := range matches {
				ids[i] = m.ID
			}
			return fmt.Errorf("%d dashboards titled %q already exist (%s); pass --id to choose one (and --dashboard %s)",
				len(matches), title, strings.Join(ids, ", "), d.name)
		}
		for i := range matches {
			if matches[i].ID == opts.id {
				target = &matches[i]
				break
			}
		}
		if target == nil {
			return fmt.Errorf("--id %q does not match any of the %d dashboards titled %q", opts.id, len(matches), title)
		}
	}

	if target == nil {
		_, _ = fmt.Fprintf(out, "  [%s] not found in Datadog — creating (this build: %s)\n", d.name, version)
		return createDashboard(d, client, dash, hash, opts.site, false, out)
	}

	// A live dashboard with our title exists — is it already this build?
	mk, found := parseDashboardMarker(target.Description)
	if found && mk.hash == hash {
		_, _ = fmt.Fprintf(out, "  [%s] up to date — the live dashboard already matches this build (%s)\n", d.name, version)
		return nil
	}

	// Differs: show live-vs-build so it's obvious what would change, then act.
	_, _ = fmt.Fprintf(out, "  [%s] live dashboard differs from this build (live: %s → build: %s)\n",
		d.name, liveVersionLabel(mk, found), version)

	switch resolveOnExists(opts, d, out) {
	case "skip":
		_, _ = fmt.Fprintf(out, "  [%s] skipped — existing dashboard left unchanged (re-run with --on-exists overwrite to update, or new for a copy)\n", d.name)
		return nil
	case "overwrite":
		_, _ = fmt.Fprintf(out, "  [%s] overwriting in place...\n", d.name)
		stampDashboardMarker(dash, d.name, version, hash)
		body, err := json.Marshal(dash)
		if err != nil {
			return fmt.Errorf("marshalling dashboard: %w", err)
		}
		url, err := client.update(target.ID, body)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "  [%s] updated in place: %s\n", d.name, dashLink(opts.site, url, target.ID))
		printDashboardChecklist(d, out)
		_, _ = fmt.Fprintln(out)
		return nil
	case "new":
		// Disambiguate the title so the new copy doesn't collide with the
		// existing one on the next run's title match.
		dash["title"] = fmt.Sprintf("%s (%s)", title, version)
		_, _ = fmt.Fprintf(out, "  [%s] creating a new copy titled %q...\n", d.name, dash["title"])
		return createDashboard(d, client, dash, hash, opts.site, true, out)
	}
	return nil
}

// createDashboard stamps the version marker and POSTs a new dashboard. renamed
// is true for the "create new copy" path (title already disambiguated).
func createDashboard(d dashboardDef, client ddClient, dash map[string]interface{}, hash, site string, renamed bool, out io.Writer) error {
	stampDashboardMarker(dash, d.name, version, hash)
	body, err := json.Marshal(dash)
	if err != nil {
		return fmt.Errorf("marshalling dashboard: %w", err)
	}
	url, err := client.create(body)
	if err != nil {
		return err
	}
	label := "created"
	if renamed {
		label = "created new copy"
	}
	if link := dashLink(site, url, ""); link != "" {
		_, _ = fmt.Fprintf(out, "  [%s] %s: %s\n", d.name, label, link)
	} else {
		_, _ = fmt.Fprintf(out, "  [%s] %s (URL not available in response)\n", d.name, label)
	}
	printDashboardChecklist(d, out)
	_, _ = fmt.Fprintln(out)
	return nil
}

// liveVersionLabel describes the live dashboard's version for the difference
// line — the marker's version, or that it carries no click-dog marker.
func liveVersionLabel(mk dashboardMarker, found bool) string {
	if !found {
		return "unmarked (no click-dog marker)"
	}
	return mk.version
}

// resolveOnExists decides what to do about an existing, differing dashboard:
// an explicit --on-exists wins; otherwise prompt on a terminal, else skip.
// out is used only on the interactive path (passed through to promptOnExists).
func resolveOnExists(opts reconcileOpts, d dashboardDef, out io.Writer) string {
	if opts.onExists != "" {
		return opts.onExists
	}
	if !opts.interactive {
		return "skip"
	}
	return promptOnExists(opts.in, out, d)
}

// promptOnExists asks how to handle an existing, differing dashboard,
// re-prompting on unrecognized input and defaulting to skip on EOF. The caller
// has already printed the live-vs-build difference.
func promptOnExists(in io.Reader, out io.Writer, d dashboardDef) string {
	r := bufio.NewReader(in)
	for {
		_, _ = fmt.Fprintf(out, "  [%s] [o]verwrite in place, create [n]ew copy, or [s]kip? [s] ", d.name)
		line, err := r.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "o", "overwrite":
			return "overwrite"
		case "n", "new":
			return "new"
		case "s", "skip", "":
			return "skip"
		}
		if err != nil { // EOF with no decision → safe default
			return "skip"
		}
	}
}

// --- version marker ------------------------------------------------------
//
// Datadog rewrites a dashboard on import (reassigns every widget id, adds
// author/created_at/url, normalizes layout), so hashing the live document can
// never match what we shipped. Instead we stamp a marker into the description
// at create/overwrite time and compare only that marker on later runs. The
// hash is taken over the canonical embedded bytes (see reconcileDashboard).

type dashboardMarker struct {
	name    string
	version string
	hash    string
}

var dashboardMarkerRE = regexp.MustCompile(`<!--\s*clickdog:\s*name=(\S+)\s+version=(\S+)\s+sha256=([0-9a-fA-F]+)\s*-->`)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func parseDashboardMarker(description string) (dashboardMarker, bool) {
	m := dashboardMarkerRE.FindStringSubmatch(description)
	if m == nil {
		return dashboardMarker{}, false
	}
	return dashboardMarker{name: m[1], version: m[2], hash: strings.ToLower(m[3])}, true
}

// stampDashboardMarker writes (or replaces) the marker at the end of the
// dashboard description, preserving the human-readable prose before it.
func stampDashboardMarker(dash map[string]interface{}, name, ver, hash string) {
	desc, _ := dash["description"].(string)
	desc = dashboardMarkerRE.ReplaceAllString(desc, "")
	desc = strings.TrimRight(desc, "\n ")
	marker := fmt.Sprintf("<!-- clickdog: name=%s version=%s sha256=%s -->", name, ver, hash)
	if desc == "" {
		dash["description"] = marker
		return
	}
	dash["description"] = desc + "\n\n" + marker
}

// --- Datadog API client --------------------------------------------------

// ddDashboardSummary is the subset of the list/CRUD payloads we use.
type ddDashboardSummary struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	URL         string `json:"url"`
}

// ddClient is the slice of the Datadog dashboard API this command needs.
// An interface keeps reconcileDashboard testable with a fake.
type ddClient interface {
	list() ([]ddDashboardSummary, error)
	create(body []byte) (url string, err error)
	update(id string, body []byte) (url string, err error)
}

type httpDDClient struct {
	site   string
	apiKey string
	appKey string
	http   *http.Client
}

func (c *httpDDClient) do(method, url string, body []byte) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("DD-API-KEY", c.apiKey)
	req.Header.Set("DD-APPLICATION-KEY", c.appKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Read one byte past the cap so an oversize body is reported rather than
	// silently truncated (same posture as the updater's asset fetches).
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxDashboardResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	if int64(len(respBody)) > maxDashboardResponseBytes {
		return nil, fmt.Errorf("response from Datadog exceeds %d bytes", maxDashboardResponseBytes)
	}
	// Datadog's v1 dashboard API returns 200 for list/create/update today, but
	// accept any 2xx so a 201 from a future API version or region doesn't read
	// as a failure.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

func (c *httpDDClient) list() ([]ddDashboardSummary, error) {
	respBody, err := c.do("GET", datadogAPIURL(c.site, "api", "v1", "dashboard"), nil)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Dashboards []ddDashboardSummary `json:"dashboards"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parsing dashboard list: %w", err)
	}
	return parsed.Dashboards, nil
}

func (c *httpDDClient) create(body []byte) (string, error) {
	respBody, err := c.do("POST", datadogAPIURL(c.site, "api", "v1", "dashboard"), body)
	if err != nil {
		return "", err
	}
	return urlFromResponse(respBody), nil
}

func (c *httpDDClient) update(id string, body []byte) (string, error) {
	respBody, err := c.do("PUT", datadogAPIURL(c.site, "api", "v1", "dashboard", id), body)
	if err != nil {
		return "", err
	}
	return urlFromResponse(respBody), nil
}

func datadogAPIURL(site string, pathSegments ...string) string {
	var path, rawPath strings.Builder
	for _, segment := range pathSegments {
		path.WriteByte('/')
		path.WriteString(segment)
		rawPath.WriteByte('/')
		rawPath.WriteString(url.PathEscape(segment))
	}
	return (&url.URL{
		Scheme:  "https",
		Host:    "api." + site,
		Path:    path.String(),
		RawPath: rawPath.String(),
	}).String()
}

func urlFromResponse(respBody []byte) string {
	var resp struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(respBody, &resp); err == nil {
		return resp.URL
	}
	return ""
}

// dashLink builds a clickable dashboard URL from the API's relative url path,
// falling back to the /dashboard/<id> form when the response omitted it.
func dashLink(site, urlPath, id string) string {
	if urlPath != "" {
		return "https://app." + site + urlPath
	}
	if id != "" {
		return "https://app." + site + "/dashboard/" + id
	}
	return ""
}

// printDashboardChecklist emits the post-import "make sure data shows up"
// checklist for a freshly created dashboard. Both shipped dashboards read a
// different surface (query → exported spans, health → click-dog's own
// /metrics), so a successful API import can still render blank unless the
// matching ingestion is configured. Printing the requirement right after the
// "created" line is the import-time guidance issue #178 asked for.
func printDashboardChecklist(d dashboardDef, out io.Writer) {
	if len(d.prerequisites) == 0 {
		return
	}
	_, _ = fmt.Fprintf(out, "  [%s] After import — confirm data shows up:\n", d.name)
	for _, line := range d.prerequisites {
		_, _ = fmt.Fprintf(out, "  [%s]   %s\n", d.name, line)
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
