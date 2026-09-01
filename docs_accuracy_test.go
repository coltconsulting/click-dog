package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
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

	data, err := os.ReadFile("docs/install.md")
	if err != nil {
		t.Fatal(err)
	}

	pinned := regexp.MustCompile(`(?m)(?:VERSION=|-v[ \t]+|click_dog_version=)v?[0-9]{2}\.[0-9]{2}\.[0-9]+`)
	if match := pinned.Find(data); match != nil {
		t.Fatalf("docs/install.md pins a command example to %q; resolve latest or use a VERSION variable", match)
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
		{path: "docs/getting-started.md", start: "## 3. See Value", end: "\n## Next steps"},
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
			"`click-dog -validate` and `click-dog check` can succeed",
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

func TestHomeTerminalTranscript_UsesCurrentOutput(t *testing.T) {
	requireInternalSite(t)

	data, err := os.ReadFile("www/home.html")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)

	for _, fabricated := range []string{
		"✓ config valid · 1 source · 1 sink",
		"→ 248 spans matched · 0 sent (dry-run)",
		"collector up · leader · exporting",
		"clickhouse → otel://localhost:4317 healthy",
	} {
		if strings.Contains(text, fabricated) {
			t.Errorf("www/home.html contains fabricated terminal output %q", fabricated)
		}
	}

	for _, current := range []string{
		"Config click-dog.yaml is valid.",
		"Exporters:   1 OTEL, 0 Splunk HEC",
		"OTEL[0]:       localhost:4317 (service=click-dog-monitor)",
		"Monitor:     min_trace=1000ms, interval=30s",
		"Self-metrics: OTLP push off",
		"Dry-run mode: exports will be discarded",
		"--- Dry Run Summary ---",
		"Starting scheduled mode: min_trace_duration=1000ms, interval=30s, lookback=40s (interval=30 + buffer=10)",
	} {
		if !strings.Contains(text, current) {
			t.Errorf("www/home.html is missing current terminal output %q", current)
		}
	}
}
