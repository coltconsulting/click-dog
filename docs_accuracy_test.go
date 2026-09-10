package main

import (
	"bytes"
	"html"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/coltconsulting/click-dog/internal/config"
	"gopkg.in/yaml.v3"
)

func publicDocFiles(t *testing.T) []string {
	t.Helper()
	requireInternalDocs(t)

	files := []string{"README.md", "CONTRIBUTING.md"}
	for _, root := range []string{"docs", "www"} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if path == filepath.Join("docs", "development") {
					return filepath.SkipDir
				}
				return nil
			}
			switch filepath.Ext(path) {
			case ".md", ".html", ".txt":
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	return files
}

func deliveryContractFiles(t *testing.T) []string {
	t.Helper()

	files := append([]string{}, publicDocFiles(t)...)
	files = append(files,
		"CLAUDE.md",
		"config.yaml.example",
		"export_gate.go",
		"internal/leader/election.go",
		"main.go",
	)
	err := filepath.WalkDir("deploy/kubernetes", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		switch filepath.Ext(path) {
		case ".md", ".yaml", ".yml":
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk deploy/kubernetes: %v", err)
	}
	return files
}

func TestPublicDocs_DoNotLinkExportIgnoredDevelopmentPaths(t *testing.T) {
	for _, path := range publicDocFiles(t) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(data)
		for _, forbidden := range []string{
			"docs/development/",
			"../development/",
			"github.com/coltconsulting/click-dog/issues/18",
			"github.com/coltconsulting/click-dog/issues/19",
		} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s contains public link/reference to unavailable internal target %q", path, forbidden)
			}
		}
	}
}

func TestDeliveryContract_DoesNotPromiseDownstreamDeduplication(t *testing.T) {
	for _, path := range deliveryContractFiles(t) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := strings.ToLower(string(data))
		for _, forbidden := range []string{
			"effectively-once",
			"backends dedup it away",
			"collectors and backends handle duplicate",
			"collector dedups by",
			"deduplicated downstream by",
			"downstream collectors dedup",
		} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s contains unsupported delivery guarantee %q", path, forbidden)
			}
		}
	}
}

func TestKeeperStartupDocs_QualifyFailurePaths(t *testing.T) {
	for _, path := range deliveryContractFiles(t) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := strings.ToLower(string(data))
		for _, forbidden := range []string{
			"falls back to standalone mode if keeper is unreachable",
			"if keeper is unreachable at startup, the instance runs in standalone mode",
			"keeper unreachable at startup (standalone fallback)",
			"when keeper was unreachable at startup",
		} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s collapses distinct Keeper startup failures into %q", path, forbidden)
			}
		}
	}
}

func TestFailoverDocs_KeepLookbackRecoveryConditional(t *testing.T) {
	requireInternalDocs(t)

	files := []string{
		"docs/configuration.md",
		"docs/operating.md",
		"docs/development/specs/deployment-topology.md",
	}
	// docs/development is internal-only (export-ignored), so the whole tree is
	// absent from the public archive. When it is absent, skip its files here;
	// the public docs in this list are still checked. When it IS present, a
	// missing file under it is a real regression and still fails below. Decide
	// once from the directory, not per-file, so a deleted file while the tree
	// exists is not mistaken for the public archive. Mirrors
	// TestDomainLanguage_SubcommandsDocumented.
	_, devErr := os.Stat("docs/development")
	developmentPresent := !os.IsNotExist(devErr)
	for _, path := range files {
		if !developmentPresent && strings.HasPrefix(path, "docs/development/") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := strings.ToLower(string(data))
		for _, forbidden := range []string{
			"session ttl (≤10s)",
			"prevents a failover gap",
			"never a failover gap",
			"no failover gap",
		} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s contains unconditional failover claim %q", path, forbidden)
			}
		}
	}
}

func TestInstallDocs_DoNotPinCommandExamplesToOneRelease(t *testing.T) {
	requireInternalDocs(t)

	pinned := regexp.MustCompile(`(?m)(?:VERSION=|-v[ \t]+|click_dog_version=)v?[0-9]{2}\.[0-9]{2}\.[0-9]+`)
	for _, path := range []string{
		"docs/install.md",
		"docs/updating.md",
		"docs/verify-releases.md",
		"docs/ansible.md",
		"docs/kubernetes.md",
		"docs/docker.md",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if match := pinned.Find(data); match != nil {
			t.Fatalf("%s pins a command example to %q; resolve latest or use a VERSION variable", path, match)
		}
	}
}

func TestKubernetesExample_DefinesReadonly2Profile(t *testing.T) {
	data, err := os.ReadFile("deploy/kubernetes/clickhouse-operator-spanlog.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, required := range []string{
		"click_dog_readonly/readonly: 2",
		"click_dog_monitor/profile: click_dog_readonly",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("Kubernetes ClickHouse example is missing %q", required)
		}
	}
	if strings.Contains(text, "click_dog_monitor/profile: readonly") {
		t.Error("Kubernetes ClickHouse example uses the built-in readonly=1 profile")
	}
}

func TestAnalyzeQueriesDocs_ListEveryFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runAnalyzeQueries([]string{"-help"}, &out, &errOut); code != 0 {
		t.Fatalf("analyze queries -help exit code = %d, want 0", code)
	}

	requireInternalDocs(t)

	docs, err := os.ReadFile("docs/query-analysis.md")
	if err != nil {
		t.Fatal(err)
	}
	helpFlag := regexp.MustCompile(`(?m)^  -([a-z][a-z-]*)`)
	for _, match := range helpFlag.FindAllStringSubmatch(errOut.String(), -1) {
		name := match[1]
		if !bytes.Contains(docs, []byte("-"+name+"`")) {
			t.Errorf("docs/query-analysis.md does not list analyze queries flag -%s", name)
		}
	}
}

func TestDatadogDashboardCatalog_DocumentedFromQueryAnalysis(t *testing.T) {
	want := map[string]string{
		"query":    "datadog-query-analysis.json",
		"activity": "datadog-user-activity.json",
		"health":   "datadog-clickdog-health.json",
	}
	if got := len(shippedDashboards); got != len(want) {
		t.Fatalf("shipped dashboard count = %d, want %d", got, len(want))
	}

	requireInternalDocs(t)

	docs, err := os.ReadFile("docs/query-analysis.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(docs)
	for _, dashboard := range shippedDashboards {
		filename, ok := want[dashboard.name]
		if !ok {
			t.Errorf("unexpected shipped dashboard selector %q", dashboard.name)
			continue
		}
		if dashboard.filename != filename {
			t.Errorf("dashboard %q filename = %q, want %q", dashboard.name, dashboard.filename, filename)
		}
		if selector := "--dashboard " + dashboard.name; !strings.Contains(text, selector) {
			t.Errorf("docs/query-analysis.md does not document %q", selector)
		}
		delete(want, dashboard.name)
	}
	if len(want) > 0 {
		t.Errorf("shipped dashboard selectors missing: %v", want)
	}

	for _, required := range []string{
		"Application Query Analysis",
		"Exported User Activity",
		"default `all` selection",
		"provisions all three",
		"not a compliance or audit log",
		"click_dog.query_operation_supported",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("docs/query-analysis.md missing dashboard contract wording %q", required)
		}
	}
}

func TestDatadogDashboardCatalog_InstallSurfacesDocumentAllThree(t *testing.T) {
	surfaces := []struct {
		path  string
		start string
		end   string
	}{
		{path: "docs/install.md", start: "## See value", end: "\n## Use `click-dog` from now on"},
		{path: "deploy/install.sh", start: `echo "  Next steps:"`, end: "    else"},
	}
	for _, surface := range surfaces {
		t.Run(surface.path, func(t *testing.T) {
			if strings.HasPrefix(surface.path, "docs/") {
				requireInternalDocs(t)
			}
			data, err := os.ReadFile(surface.path)
			if err != nil {
				t.Fatal(err)
			}
			wholeFile := string(data)
			start := strings.Index(wholeFile, surface.start)
			if start < 0 {
				t.Fatalf("%s missing dashboard guidance start marker %q", surface.path, surface.start)
			}
			section := wholeFile[start:]
			end := strings.Index(section, surface.end)
			if end < 0 {
				t.Fatalf("%s missing dashboard guidance end marker %q", surface.path, surface.end)
			}
			text := strings.Join(strings.Fields(section[:end]), " ")
			for _, required := range []string{
				"three",
				"Application Query Analysis",
				"Exported User Activity",
				"Health",
				"click-dog create-dashboards",
			} {
				if !strings.Contains(text, required) {
					t.Errorf("%s does not document the full dashboard catalog: missing %q", surface.path, required)
				}
			}
			for _, stale := range []string{"creates two", "query-analysis and health dashboards"} {
				if strings.Contains(text, stale) {
					t.Errorf("%s retains stale dashboard wording %q", surface.path, stale)
				}
			}
		})
	}
}

func TestDatadogActivityCompatibilityDocs_DescribeDependentWidgetDegradation(t *testing.T) {
	requireInternalDocs(t)

	requiredByPath := map[string][]string{
		"docs/query-analysis.md": {
			"Other query-log attributes remain on eligible enriched spans",
			"dashboard widgets that filter or group by operation/access type can be empty",
			"user-to-database and user-to-table relationship tables",
		},
		"openspec/specs/datadog-dashboards/spec.md": {
			"underlying user, database, and table attributes SHALL remain available on eligible enriched spans",
			"dashboard widgets that also filter or group by operation/access type MAY be empty",
			"user-to-database and user-to-table relationship tables",
		},
	}
	for path, required := range requiredByPath {
		t.Run(path, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			text := strings.Join(strings.Fields(string(data)), " ")
			for _, phrase := range required {
				if !strings.Contains(text, phrase) {
					t.Errorf("%s missing compatibility wording %q", path, phrase)
				}
			}
		})
	}

	spanSpec, err := os.ReadFile("openspec/specs/span-attributes/spec.md")
	if err != nil {
		t.Fatal(err)
	}
	spanSpecText := strings.Join(strings.Fields(string(spanSpec)), " ")
	if !strings.Contains(spanSpecText, "A failed probe, zero discovered replicas, or mixed-version cluster") {
		t.Error("span-attributes spec does not exhaustively document operation capability disable conditions")
	}
}

func TestDatadogManualImport_ListsEveryShippedDashboardFile(t *testing.T) {
	for _, path := range []string{"docs/integrations/datadog/dashboards.md", "dashboards/README.md"} {
		t.Run(path, func(t *testing.T) {
			if strings.HasPrefix(path, "docs/") {
				requireInternalDocs(t)
			}
			docs, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			text := string(docs)
			start := strings.Index(text, "Option 2: Datadog API")
			if start < 0 {
				t.Fatalf("%s missing manual API import section", path)
			}
			section := text[start:]
			codeStart := strings.Index(section, "```bash")
			if codeStart < 0 {
				t.Fatalf("%s manual API import section missing bash block", path)
			}
			codeStart += len("```bash")
			codeEnd := strings.Index(section[codeStart:], "```")
			if codeEnd < 0 {
				t.Fatalf("%s manual API import bash block is not closed", path)
			}
			manualImport := section[codeStart : codeStart+codeEnd]
			for _, dashboard := range shippedDashboards {
				if !strings.Contains(manualImport, dashboard.filename) {
					t.Errorf("%s manual import does not list %q", path, dashboard.filename)
				}
			}
		})
	}
}

func TestExportedUserActivity_WebsiteCopyKeepsOperationalBoundary(t *testing.T) {
	t.Run("site", func(t *testing.T) {
		requireInternalSite(t)

		home, err := os.ReadFile("www/home.html")
		if err != nil {
			t.Fatal(err)
		}
		homeText := string(home)
		for _, required := range []string{
			"exported user activity",
			"bounded operational activity",
			"It is not an audit log",
			"users, databases, tables, and operations",
		} {
			if !strings.Contains(homeText, required) {
				t.Errorf("homepage missing exported-user-activity wording %q", required)
			}
		}
	})

	t.Run("documentation", func(t *testing.T) {
		requireInternalDocs(t)

		overview, err := os.ReadFile("docs/overview.md")
		if err != nil {
			t.Fatal(err)
		}
		overviewText := strings.Join(strings.Fields(string(overview)), " ")
		if !strings.Contains(overviewText, "exported user activity") {
			t.Error("documentation overview does not make exported user activity discoverable")
		}
	})
}

func TestResilienceDocs_UseCurrentBackoffLogFormat(t *testing.T) {
	requireInternalDocs(t)

	for _, path := range []string{"docs/resilience.md", "docs/troubleshooting.md"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(string(data), "Backoff increased to") {
			t.Errorf("%s documents the pre-format-change backoff log message", path)
		}
	}
}

func TestAuditedOperatorContracts_AreDocumented(t *testing.T) {
	requireInternalDocs(t)

	required := map[string][]string{
		"docs/configuration.md": {
			"omitting `monitor:` is not a valid scheduled-mode config",
			"Replace the removed top-level `otel:` mapping",
			"Delete the removed `ha.enabled` key",
			"uses the host's system trust roots",
		},
		"docs/filtering.md": {
			"`blacklist_queries` expressions are not compiled by configuration loading",
			"`click-dog validate` and `click-dog check` can succeed",
		},
		"docs/integrations/generic-otlp.md": {
			"3,500,000 bytes (3.5 MB)",
			"`ResourceExhausted`",
			"returns no accepted span keys",
		},
		"docs/integrations/splunk-hec.md": {
			"appends `/services/collector/event`",
			"does not request or poll Splunk indexer acknowledgements",
			"fixed 30-second timeout",
			"does **not** validate the token",
		},
		"docs/observability.md": {
			"at most four sends in flight",
			"There is no waiting queue",
			"uses a synchronous send during graceful signal handling",
			"Last success does not prove the regular span fetch ran",
		},
		"docs/span-attributes.md": {
			"A direct attribute therefore wins",
			"Values larger than 64 KiB are silently skipped",
		},
		"docs/install.md": {
			"does not preflight it",
			"it deletes both destination files",
			"systemd-only",
		},
		"docs/updating.md": {
			"20 most recent **public** GitHub Releases",
		},
	}

	for path, phrases := range required {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := strings.Join(strings.Fields(string(data)), " ")
		for _, phrase := range phrases {
			if !strings.Contains(text, phrase) {
				t.Errorf("%s is missing audited operator contract %q", path, phrase)
			}
		}
	}
}

func TestConfigSourceComments_DoNotRestoreStaleSemantics(t *testing.T) {
	data, err := os.ReadFile("internal/config/config.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, stale := range []string{
		"Log rotation settings (only when log_file is set)",
		"Skip queries with SQL text > this many characters (0 = no limit, default: 100000)",
	} {
		if strings.Contains(text, stale) {
			t.Errorf("internal/config/config.go contains stale comment %q", stale)
		}
	}
}

// TestMonitorMaxQueryLengthDocs_ScopeToQueryLogFetch pins the documented scope
// of monitor.max_query_length to where the predicate actually lives. The
// length(query) filter is built only by queryLogBuilder.addDurationFilters, so
// it reaches the backfill and analyze query_log fetches and never the
// scheduled span-log fetch; on that path the key only bounds the enriched
// query_log.normalized_query attribute. Docs have twice drifted toward
// promising a fetch-time exclusion that scheduled mode does not perform.
func TestMonitorMaxQueryLengthDocs_ScopeToQueryLogFetch(t *testing.T) {
	reader, err := os.ReadFile("internal/clickhouse/reader.go")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(reader), "length(query) <= ?"); got != 1 {
		t.Fatalf("length(query) predicate occurs %d times in internal/clickhouse/reader.go, want 1; "+
			"if a span-log length filter was added, update docs/filtering.md and "+
			"docs/integrations/generic-otlp.md before changing this test", got)
	}

	// The predicate reaches only the builders that call addDurationFilters:
	// backfill's slowQueries* pair and analyze trace's recentQueryCandidates.
	// The analyze queries family rollups take no MaxQueryLength at all, so the
	// docs must not fold them into a blanket "analyze" claim.
	if strings.Contains(string(reader), "MaxQueryLength") {
		opts := string(reader)
		start := strings.Index(opts, "type QueryFamilyRollupOptions struct {")
		if start < 0 {
			t.Fatal("QueryFamilyRollupOptions not found in internal/clickhouse/reader.go")
		}
		end := strings.Index(opts[start:], "\n}")
		if end < 0 {
			t.Fatal("could not delimit QueryFamilyRollupOptions")
		}
		if strings.Contains(opts[start:start+end], "MaxQueryLength") {
			t.Error("QueryFamilyRollupOptions gained a MaxQueryLength field; " +
				"docs/filtering.md and docs/integrations/generic-otlp.md say the " +
				"family rollups do not take this key")
		}
	}

	requireInternalDocs(t)

	for path, phrases := range map[string][]string{
		"docs/filtering.md": {
			"During the query_log fetch (backfill, `analyze trace` candidate search)",
			"`monitor.max_query_length` never drops a live span",
			"The `analyze queries` family rollups do not take this key",
		},
		"docs/integrations/generic-otlp.md": {
			"Scheduled mode does not apply that predicate to the span-log fetch",
			"the `analyze queries` family rollups do not take the key at all",
		},
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := strings.Join(strings.Fields(string(data)), " ")
		for _, phrase := range phrases {
			if !strings.Contains(text, phrase) {
				t.Errorf("%s is missing the monitor.max_query_length scope contract %q", path, phrase)
			}
		}
	}

	for path, stale := range map[string]string{
		"docs/filtering.md":                 "| `monitor.max_query_length` | During ClickHouse query |",
		"docs/integrations/generic-otlp.md": "`monitor.max_query_length` applies to scheduled/live fetch",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := strings.Join(strings.Fields(string(data)), " ")
		if strings.Contains(text, stale) {
			t.Errorf("%s restates the unscoped fetch-time claim %q", path, stale)
		}
	}
}

func TestReadmeQuickStart_UsesMinimalConfig(t *testing.T) {
	data, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	sectionStart := strings.Index(text, "## Quick Start")
	if sectionStart < 0 {
		t.Fatal("README.md is missing the Quick Start section")
	}
	fenceStart := strings.Index(text[sectionStart:], "```yaml\n")
	if fenceStart < 0 {
		t.Fatal("README.md Quick Start is missing its YAML block")
	}
	fenceStart += sectionStart + len("```yaml\n")
	fenceEnd := strings.Index(text[fenceStart:], "\n```")
	if fenceEnd < 0 {
		t.Fatal("README.md Quick Start YAML block is not closed")
	}
	quickStart := text[fenceStart : fenceStart+fenceEnd]

	for _, routineDefault := range []string{"port", "username", "service_name", "enabled", "check_interval_s", "log_level"} {
		pattern := regexp.MustCompile(`(?m)^\s*` + routineDefault + `:`)
		if pattern.MatchString(quickStart) {
			t.Errorf("README.md Quick Start exposes routine default %q", routineDefault)
		}
	}

	configPath := filepath.Join(t.TempDir(), "click-dog.yaml")
	if err := os.WriteFile(configPath, []byte(quickStart+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLICKHOUSE_PASSWORD", "test-secret")
	if _, err := config.LoadConfig(configPath); err != nil {
		t.Fatalf("README.md Quick Start config must load and validate: %v\n%s", err, quickStart)
	}
}

func yamlLeafPaths(t *testing.T, label string, data []byte) map[string]bool {
	t.Helper()

	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("parse %s as YAML: %v", label, err)
	}
	paths := make(map[string]bool)
	var walk func(*yaml.Node, string)
	walk = func(node *yaml.Node, prefix string) {
		switch node.Kind {
		case yaml.DocumentNode:
			for _, child := range node.Content {
				walk(child, prefix)
			}
		case yaml.MappingNode:
			for index := 0; index < len(node.Content); index += 2 {
				key, value := node.Content[index], node.Content[index+1]
				path := key.Value
				if prefix != "" {
					path = prefix + "." + path
				}
				if value.Kind == yaml.ScalarNode {
					paths[path] = true
				} else {
					walk(value, path)
				}
			}
		case yaml.SequenceNode:
			path := prefix + "[]"
			for _, child := range node.Content {
				if child.Kind == yaml.ScalarNode {
					paths[path] = true
				} else {
					walk(child, path)
				}
			}
		}
	}
	walk(&document, "")
	return paths
}

func TestHomeQuickStart_ShowsThreeConfigViews(t *testing.T) {
	requireInternalSite(t)

	data, err := os.ReadFile("www/home.html")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	stripTags := regexp.MustCompile(`<[^>]+>`)
	extractConfig := func(t *testing.T, view string) string {
		t.Helper()
		panel := regexp.MustCompile(`(?s)<div class="config-panel[^"]*" id="config-` + view + `">(.*?)</div>`).FindStringSubmatch(text)
		if panel == nil {
			t.Fatalf("homepage is missing the %s configuration panel", view)
		}
		block := regexp.MustCompile(`(?s)<pre class="code">(.*?)</pre>`).FindStringSubmatch(panel[1])
		if block == nil {
			t.Fatalf("homepage %s panel is missing its config block", view)
		}
		return strings.TrimSpace(html.UnescapeString(stripTags.ReplaceAllString(block[1], ""))) + "\n"
	}
	tempDir := t.TempDir()
	t.Setenv("CLICKHOUSE_PASSWORD", "test-secret")
	for _, view := range []struct {
		name      string
		queryMode config.QueryTextMode
		password  string
	}{
		{"starter", config.QueryTextModeNormalizedOnly, "test-secret"},
		{"minimal", config.QueryTextModeRaw, ""},
	} {
		t.Run(view.name, func(t *testing.T) {
			plainConfig := extractConfig(t, view.name)
			configPath := filepath.Join(tempDir, view.name+".yaml")
			if err := os.WriteFile(configPath, []byte(plainConfig), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadConfig(configPath)
			if err != nil {
				t.Fatalf("homepage config must load and validate: %v\n%s", err, plainConfig)
			}
			if cfg.Filters.EffectiveQueryTextMode() != view.queryMode || cfg.ClickHouse.Password != view.password {
				t.Error("homepage config must match its stated query-text and authentication choices")
			}
			if !cfg.Monitor.Enabled || cfg.Monitor.CheckIntervalS != 30 || cfg.ClickHouse.Port != 9000 {
				t.Error("homepage config must work with the scheduled-mode, polling, and port defaults")
			}
			paths := yamlLeafPaths(t, "homepage "+view.name, []byte(plainConfig))
			wantPaths := map[string]bool{
				"clickhouse.host":                    true,
				"exporters.otel[].collector_address": true,
				"monitor.min_trace_duration_ms":      true,
			}
			if view.name == "starter" {
				wantPaths["clickhouse.password"] = true
				wantPaths["filters.query_text_mode"] = true
			}
			for path := range wantPaths {
				if !paths[path] {
					t.Errorf("homepage config is missing %s", path)
				}
			}
			for path := range paths {
				if !wantPaths[path] {
					t.Errorf("homepage config includes optional or defaulted field %s", path)
				}
			}
		})
	}
	plainAdvanced := extractConfig(t, "advanced")

	clickHouseCA := filepath.Join(tempDir, "clickhouse-ca.pem")
	otelCA := filepath.Join(tempDir, "otel-ca.pem")
	for _, caPath := range []string{clickHouseCA, otelCA} {
		if err := os.WriteFile(caPath, []byte("test TLS material\n"), 0o600); err != nil {
			t.Fatalf("write advanced config TLS fixture: %v", err)
		}
	}
	plainAdvanced = strings.ReplaceAll(plainAdvanced, "/etc/click-dog/clickhouse-ca.pem", clickHouseCA)
	plainAdvanced = strings.ReplaceAll(plainAdvanced, "/etc/click-dog/otel-ca.pem", otelCA)
	advancedConfigPath := filepath.Join(tempDir, "click-dog-advanced.yaml")
	if err := os.WriteFile(advancedConfigPath, []byte(plainAdvanced), 0o600); err != nil {
		t.Fatalf("write extracted advanced homepage config: %v", err)
	}
	if _, err := config.LoadConfig(advancedConfigPath); err != nil {
		t.Fatalf("homepage advanced config must load and validate: %v\n%s", err, plainAdvanced)
	}
	productionProfile, err := os.ReadFile("examples/click-dog-production.yaml")
	if err != nil {
		t.Fatal(err)
	}
	profilePaths := yamlLeafPaths(t, "production profile", productionProfile)
	advancedPaths := yamlLeafPaths(t, "homepage advanced config", []byte(plainAdvanced))
	tlsExtras := map[string]bool{
		"clickhouse.secure":        true,
		"clickhouse.ca_cert":       true,
		"exporters.otel[].secure":  true,
		"exporters.otel[].ca_cert": true,
	}
	var missing, unexpected []string
	// This comparison is intentionally bidirectional: the advanced homepage
	// view is the complete production profile, not an illustrative subset.
	for path := range profilePaths {
		if !advancedPaths[path] {
			missing = append(missing, path)
		}
	}
	for path := range advancedPaths {
		if !profilePaths[path] && !tlsExtras[path] {
			unexpected = append(unexpected, path)
		}
	}
	for path := range tlsExtras {
		if !advancedPaths[path] {
			missing = append(missing, path+" (TLS extension)")
		}
	}
	sort.Strings(missing)
	sort.Strings(unexpected)
	if len(missing) > 0 || len(unexpected) > 0 {
		t.Errorf("homepage advanced config drifted from the production profile: missing=%v unexpected=%v", missing, unexpected)
	}

	for _, configPath := range []string{
		"data-config-switcher",
		`<h3 class="config-panel-title">Starter config</h3>`,
		`<h3 class="config-panel-title">Minimal config</h3>`,
		`<h3 class="config-panel-title">Advanced config</h3>`,
		"/configuration/#validate-and-run",
		"/examples/click-dog-production.yaml",
		"/configuration/",
	} {
		if !strings.Contains(text, configPath) {
			t.Errorf("homepage is missing configuration path %q", configPath)
		}
	}

	openingTag := func(marker string) string {
		t.Helper()
		markerStart := strings.Index(text, marker)
		if markerStart < 0 {
			t.Fatalf("homepage is missing %q", marker)
		}
		start := strings.LastIndex(text[:markerStart], "<")
		if start < 0 {
			t.Fatalf("homepage marker %q is not inside a tag", marker)
		}
		end := strings.Index(text[start:], ">")
		if end < 0 {
			t.Fatalf("homepage tag starting with %q is not closed", marker)
		}
		return text[start : start+end+1]
	}
	staticControls := map[string]string{
		"switcher":       openingTag("data-config-switcher"),
		"starter tab":    openingTag(`id="config-tab-starter"`),
		"minimal tab":    openingTag(`id="config-tab-minimal"`),
		"advanced tab":   openingTag(`id="config-tab-advanced"`),
		"starter panel":  openingTag(`id="config-starter"`),
		"minimal panel":  openingTag(`id="config-minimal"`),
		"advanced panel": openingTag(`id="config-advanced"`),
	}
	for name, tag := range staticControls {
		for _, enhancedOnly := range []string{" role=", " aria-", " tabindex=", " hidden"} {
			if strings.Contains(tag, enhancedOnly) {
				t.Errorf("homepage no-JavaScript %s exposes enhanced-only state %q", name, enhancedOnly)
			}
		}
	}
}

func TestHomeInstallPath_ProvidesInstallerVerification(t *testing.T) {
	requireInternalSite(t)

	home, err := os.ReadFile("www/home.html")
	if err != nil {
		t.Fatal(err)
	}
	hero := regexp.MustCompile(`(?s)<header class="hero">.*?</header>`).Find(home)
	if hero == nil {
		t.Fatal("homepage is missing its hero")
	}
	if !regexp.MustCompile(`<a\b[^>]*href="/install/"[^>]*>Get started</a>`).Match(hero) {
		t.Fatal("homepage Get started action must lead to the install guide")
	}
	guide, err := os.ReadFile("docs/install.md")
	if err != nil {
		t.Fatal(err)
	}
	quickStart := strings.SplitN(string(guide), "## Prerequisites", 2)[0]
	if !strings.Contains(quickStart, "https://github.com/coltconsulting/click-dog/releases/latest/download/install.sh") {
		t.Error("install quick start is missing the release installer download")
	}
	if !regexp.MustCompile(`\[[^\]]+\]\(verify-releases\.md#authenticate-installsh-before-sudo\)`).MatchString(quickStart) {
		t.Error("install quick start must link to bootstrap verification instructions")
	}
}

// obsoleteCommercialActions are retired CTAs (and the retired support-page
// anchor) that must not resurface on any public doc or site surface.
var obsoleteCommercialActions = []string{
	"Book a session",
	"Book%20a%20Click-Dog%20setup%20session",
	"Click-Dog%20commercial%20support%20request",
	"#commercial-support",
}

// assertCommercialSurface checks one file for the production-support content
// it must carry and for retired commercial references, returning the text for
// follow-up assertions.
func assertCommercialSurface(t *testing.T, path string, required []string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := string(data)
	for _, want := range required {
		if !strings.Contains(text, want) {
			t.Errorf("%s is missing production-support content %q", path, want)
		}
	}
	for _, obsolete := range obsoleteCommercialActions {
		if strings.Contains(text, obsolete) {
			t.Errorf("%s still contains retired commercial reference %q", path, obsolete)
		}
	}
	return text
}

func templateBlock(t *testing.T, source, name string) string {
	t.Helper()
	startMarker := `{% block ` + name + ` %}`
	start := strings.Index(source, startMarker)
	if start < 0 {
		t.Fatalf("docs template is missing the %s block", name)
	}
	end := strings.Index(source[start:], `{% endblock %}`)
	if end < 0 {
		t.Fatalf("docs template %s block is not closed", name)
	}
	return source[start : start+end]
}

func TestCommercialSupport_IsDiscoverableAcrossSite(t *testing.T) {
	requireInternalSite(t)

	// href= with the closing quote pins the anchor-free destination; a bare
	// "/support/" substring would still match a stale deep link.
	assertCommercialSurface(t, "www/home.html", []string{
		`href="/support/"`,
		"maintainer",
	})
	main := assertCommercialSurface(t, "www/main.html", []string{
		`href="{{ 'support/' | url }}"`,
		"maintainer",
		`{% block announce %}`,
		"cd-announcement__support",
		`{% block content %}`,
		"cd-docs-cta",
	})
	for _, name := range []string{"announce", "content"} {
		block := templateBlock(t, main, name)
		if !strings.Contains(block, "hide_global_support_cta") {
			t.Errorf("docs template %s block does not honor the page's global-support suppression flag", name)
		}
		if strings.Contains(block, "page.url") {
			t.Errorf("docs template %s block uses a route-specific support guard instead of page metadata", name)
		}
	}

	for _, path := range publicDocFiles(t) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(data)
		for _, obsolete := range obsoleteCommercialActions {
			if strings.Contains(text, obsolete) {
				t.Errorf("%s still contains retired commercial reference %q", path, obsolete)
			}
		}
	}
}

func TestSupportPage_CarriesProductionSupportBrand(t *testing.T) {
	requireInternalDocs(t)

	support := assertCommercialSurface(t, "docs/support.md", []string{
		"Click-Dog Production Support",
		"What production support covers",
		"cd-support-intro__marker",
		"maintainer",
		"Colt Consulting, Click-Dog's creator and maintainer",
		"contracted and provided by",
		"Colt Consulting Ltd",
		"Discuss%20a%20Click-Dog%20deployment",
		"Discuss your deployment",
		"response-time commitment",
	})
	if !strings.Contains(support, "hide_global_support_cta: true") {
		t.Error("support page must suppress the global end-of-page prompt it would otherwise duplicate")
	}
	if got := strings.Count(support, "?subject="); got != 1 {
		t.Errorf("support page has %d pre-addressed mailto actions, want 1", got)
	}

	assertCommercialSurface(t, "docs/overview.md", []string{
		"[Production Support](support.md)",
	})
}

// TestReadme_LinksProductionSupport runs without the site gate: the README
// ships to the public repository, where www/ is export-ignored and the site
// contract tests are skipped.
func TestReadme_LinksProductionSupport(t *testing.T) {
	assertCommercialSurface(t, "README.md", []string{
		"[Production Support](https://click-dog.com/support/)",
		"maintainer",
	})
}

// assertStandardInstallPath enforces the checksum-verified bootstrap contract
// on one install-facing document and returns its text so callers can layer
// document-specific checks without re-reading. A non-empty section scopes the
// chained-command assertions to that heading's own body — cut at its first
// subheading or sibling so another example cannot mask deletion of the
// canonical block. The wording assertions stay document-wide: the flags table
// and prose satisfy them legitimately. forbidDetailedVerify asserts the
// document links to the optional cosign procedure instead of duplicating it.
func assertStandardInstallPath(t *testing.T, path, section string, forbidDetailedVerify bool) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := string(data)

	commandScope := text
	commandLabel := path
	if section != "" {
		start := strings.Index(text, section)
		if start < 0 {
			t.Fatalf("%s is missing the %q section", path, section)
		}
		body := text[start+len(section):]
		if next := regexp.MustCompile(`(?m)^#{2,3} `).FindStringIndex(body); next != nil {
			body = body[:next[0]]
		}
		commandScope = body
		commandLabel = path + " (" + section + ")"
	}
	for _, required := range []string{
		"releases/latest/download/install.sh -o install.sh",
		"sudo bash install.sh",
	} {
		if !strings.Contains(commandScope, required) {
			t.Errorf("%s must show the standard checksum-verified install path: missing %q", commandLabel, required)
		}
	}
	chainedInstall := regexp.MustCompile(`releases/latest/download/install\.sh -o install\.sh\s*&&\s*\n\s*sudo bash install\.sh`)
	if !chainedInstall.MatchString(commandScope) {
		t.Errorf("%s must chain download success to root execution", commandLabel)
	}

	for _, required := range []string{"checksum", "--dangerously-ignore-cosign"} {
		if !strings.Contains(text, required) {
			t.Errorf("%s must document checksum verification and its cosign escape hatch: missing %q", path, required)
		}
	}
	optionalCosign := regexp.MustCompile(`(?is)cosign.{0,160}recommended,\s+not\s+required`)
	if !optionalCosign.MatchString(text) {
		t.Errorf("%s must state that cosign is recommended, not required", path)
	}
	if regexp.MustCompile(`(?m)^\s*less install\.sh\s*$`).MatchString(text) {
		t.Errorf("%s presents visual inspection as an installation step", path)
	}
	if forbidDetailedVerify && strings.Contains(text, "cosign verify-blob") {
		t.Errorf("%s should link to the detailed optional bootstrap verification instead of duplicating it", path)
	}
	return text
}

// Ungated: the README ships in the public export, where it is the primary
// install document, so its bootstrap contract must run on both trees.
func TestReadmeInstallerBootstrap_IsVerified(t *testing.T) {
	assertStandardInstallPath(t, "README.md", "", true)
}

func TestInstallerBootstrap_IsVerifiedAndInstallRolesAreClear(t *testing.T) {
	requireInternalDocs(t)

	installGuideText := assertStandardInstallPath(t, "docs/install.md", "## Quick start", true)
	verifyText, err := os.ReadFile("docs/verify-releases.md")
	if err != nil {
		t.Fatal(err)
	}

	optionalHeading := strings.Index(string(verifyText), "## Authenticate `install.sh` before sudo")
	if optionalHeading < 0 {
		t.Fatal("release verification guide is missing authenticated bootstrap procedure")
	}
	optionalProcedure := string(verifyText)[optionalHeading:]
	for _, required := range []string{
		"(\n  set -euo pipefail",
		"set -euo pipefail",
		"cosign verify-blob",
		`matches == 1`,
		"sha256sum -c install.sh.sha256",
		"shasum -a 256 -c install.sh.sha256",
		"Only after both verification commands exit zero",
	} {
		if !strings.Contains(optionalProcedure, required) {
			t.Errorf("optional bootstrap verification must fail closed and be portable: missing %q", required)
		}
	}
	signatureCheck := strings.Index(optionalProcedure, "cosign verify-blob")
	installerCheck := strings.Index(optionalProcedure, `matches == 1`)
	rootInstruction := strings.Index(optionalProcedure, "Only after both verification commands exit zero")
	if signatureCheck < 0 || installerCheck < 0 || rootInstruction < 0 ||
		signatureCheck >= installerCheck || installerCheck >= rootInstruction {
		t.Error("optional bootstrap procedure must authenticate the manifest and installer before recommending root execution")
	}

	for _, required := range []string{
		"standard guided installation",
		"Unattended install",
		"`--systemd` is present",
		"sudo -u click-dog click-dog check --config /etc/click-dog/click-dog.yaml",
		"sudo -u click-dog click-dog validate --config /etc/click-dog/click-dog.yaml",
		"without connecting",
		"install.sh` is not copied",
		"sudo click-dog deploy uninstall",
		"does **not** modify ClickHouse",
	} {
		if !strings.Contains(installGuideText, required) {
			t.Errorf("install guide must explain installer roles and checks: missing %q", required)
		}
	}
}

func TestOperationModes_DeployIncludesInstalledLifecycle(t *testing.T) {
	requireInternalDocs(t)

	text, err := os.ReadFile("docs/modes.md")
	if err != nil {
		t.Fatal(err)
	}
	content := string(text)
	for _, required := range []string{
		"subcommand family for generating deployment",
		"artifacts and managing the state of a standard systemd installation",
		"sudo click-dog deploy uninstall",
		"remove a standard systemd installation",
	} {
		if !strings.Contains(content, required) {
			t.Errorf("operation-modes deploy coverage missing %q", required)
		}
	}
}

func TestAnsibleInstallRecordsCreatedRuntimeIdentity(t *testing.T) {
	text, err := os.ReadFile("deploy/ansible/playbook.yaml")
	if err != nil {
		t.Fatal(err)
	}
	content := string(text)
	for _, required := range []string{
		"/var/lib/click-dog-installer",
		"installer-created-user",
		"installer-created-group",
		"click_dog_user_before.rc != 0",
		"click_dog_group_before.rc != 0",
		"click_dog_user_before.rc != 0 and click_dog_group_before.rc != 0",
		`mode: "0700"`,
		`mode: "0600"`,
	} {
		if !strings.Contains(content, required) {
			t.Errorf("Ansible playbook must record only identities it creates: missing %q", required)
		}
	}
}
