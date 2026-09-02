package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/analysis"
	"github.com/coltconsulting/click-dog/internal/clickhouse"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/model"
	"github.com/coltconsulting/click-dog/internal/webhook"
)

// fakeAnalysisSource is a canned queryAnalysisSource for the report path. No
// mock framework is needed: each method returns a configured value.
type fakeAnalysisSource struct {
	normalized      bool
	families        []model.QueryFamilyRollup
	familiesErr     error
	spans           []model.OpenTelemetrySpan
	spansErr        error
	queryLog        map[string]model.QueryLog
	queryLogErr     error
	dimensionCounts []clickhouse.QueryFamilyDimensionCount
	dimensionErr    error
}

func (f *fakeAnalysisSource) QueryLogNormalizedSupported() bool { return f.normalized }

func (f *fakeAnalysisSource) FetchQueryFamilyRollups(_ context.Context, _ clickhouse.QueryFamilyRollupOptions) ([]model.QueryFamilyRollup, error) {
	return f.families, f.familiesErr
}

func (f *fakeAnalysisSource) FetchOpenTelemetrySpansWithOpts(_ context.Context, _ int, _ time.Duration, _ int, _ clickhouse.FetchOpts) ([]model.OpenTelemetrySpan, error) {
	return f.spans, f.spansErr
}

func (f *fakeAnalysisSource) FetchQueryLogByQueryIDs(_ context.Context, _ []string, _ int) (map[string]model.QueryLog, error) {
	return f.queryLog, f.queryLogErr
}

func (f *fakeAnalysisSource) FetchQueryFamilyDimensionCounts(_ context.Context, _ clickhouse.QueryFamilyDimensionCountOptions) ([]clickhouse.QueryFamilyDimensionCount, error) {
	return f.dimensionCounts, f.dimensionErr
}

func testAnalyzeConfig() *config.Config {
	cfg := &config.Config{}
	cfg.ClickHouse.Host = "ch.example.internal"
	cfg.Monitor.MinTraceDurationMs = 1000
	cfg.Monitor.MaxSpansPerCycle = 1000
	return cfg
}

func defaultAnalyzeOptions() analyzeQueriesOptions {
	return analyzeQueriesOptions{
		Lookback:           time.Hour,
		Timeout:            time.Minute,
		MinExecutions:      3,
		FamilyLimit:        200,
		QueryPreviewLength: 500,
	}
}

func TestRunAnalyze_NoNestedVerbExits2(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runAnalyze(nil, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "Subcommands:") || !strings.Contains(errOut.String(), "queries") {
		t.Errorf("usage missing from stderr:\n%s", errOut.String())
	}
}

func TestRunAnalyze_UnknownVerbExits2(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runAnalyze([]string{"foo"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), `unknown command "foo"`) {
		t.Errorf("stderr missing unknown-command line:\n%s", errOut.String())
	}
}

func TestRunAnalyze_HelpExits0(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runAnalyze([]string{"--help"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "analyze") {
		t.Errorf("help missing from stdout:\n%s", out.String())
	}
}

func TestAnalyzeQueries_HelpExits0(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runAnalyzeQueries([]string{"--help"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 for --help", code)
	}
	if !strings.Contains(errOut.String(), "analyze queries") {
		t.Errorf("help text missing:\n%s", errOut.String())
	}
}

func TestAnalyzeQueries_BadFormatExits2(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runAnalyzeQueries([]string{"-format", "yaml"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "invalid --format") {
		t.Errorf("stderr missing invalid-format message:\n%s", errOut.String())
	}
}

func TestAnalyzeQueries_BadFlagExits2(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runAnalyzeQueries([]string{"-nonsense"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestAnalyzeQueries_InvalidFailOnExits2(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runAnalyzeQueries([]string{"--fail-on", "fatal"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "invalid --fail-on") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestAnalysisPolicy_CompleteSeverityExitMatrix(t *testing.T) {
	tests := []struct {
		name     string
		failOn   analysisFailOn
		severity analysis.Severity
		wantExit int
	}{
		{name: "none critical", failOn: failOnNone, severity: analysis.SeverityCritical, wantExit: 0},
		{name: "none warning", failOn: failOnNone, severity: analysis.SeverityWarning, wantExit: 0},
		{name: "none info", failOn: failOnNone, severity: analysis.SeverityInfo, wantExit: 0},
		{name: "critical critical", failOn: failOnCritical, severity: analysis.SeverityCritical, wantExit: 3},
		{name: "critical warning", failOn: failOnCritical, severity: analysis.SeverityWarning, wantExit: 0},
		{name: "critical info", failOn: failOnCritical, severity: analysis.SeverityInfo, wantExit: 0},
		{name: "warning critical", failOn: failOnWarning, severity: analysis.SeverityCritical, wantExit: 3},
		{name: "warning warning", failOn: failOnWarning, severity: analysis.SeverityWarning, wantExit: 3},
		{name: "warning info", failOn: failOnWarning, severity: analysis.SeverityInfo, wantExit: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := analysis.AnalysisReport{Findings: []analysis.Finding{{Severity: tt.severity}}}
			got := 0
			if analysisPolicyReached(report, tt.failOn) {
				got = analysisPolicyExitCode
			}
			if got != tt.wantExit {
				t.Fatalf("exit = %d, want %d", got, tt.wantExit)
			}
		})
	}
}

func TestAnalysisPolicy_UsesOnlyIncludedReportFindings(t *testing.T) {
	report := analysis.AnalysisReport{
		Findings: []analysis.Finding{{Severity: analysis.SeverityInfo}},
		AnalyzerRuns: []analysis.AnalyzerRun{{
			Name:  "failed_analyzer",
			Error: "discarded critical output",
		}},
	}
	if analysisPolicyReached(report, failOnCritical) {
		t.Fatal("analyzer run metadata outside report.findings reached the policy gate")
	}
}

func TestAnalyzeQueries_DefaultFlagsDoNotConstructNotificationDestinations(t *testing.T) {
	called := false
	factory := func(*config.Config) ([]analysisNotificationDestination, error) {
		called = true
		return nil, errors.New("must not be called")
	}
	var out, errOut bytes.Buffer
	code := analyzeQueriesWithSourceAndDestinations(criticalAnalysisSource(), testAnalyzeConfig(), "", defaultAnalyzeOptions(), "json", "", &out, &errOut, factory)
	if code != 0 {
		t.Fatalf("default exit = %d, want 0; stderr=%s", code, errOut.String())
	}
	if called {
		t.Fatal("notification destinations were constructed without --notify")
	}
}

func TestAnalyzeQueries_FailOnWarningTableAndJSON(t *testing.T) {
	for _, format := range []string{"table", "json"} {
		t.Run(format, func(t *testing.T) {
			opts := defaultAnalyzeOptions()
			opts.FailOn = failOnWarning
			var out, errOut bytes.Buffer
			code := analyzeQueriesWithSource(&fakeAnalysisSource{}, testAnalyzeConfig(), "", opts, format, "", &out, &errOut)
			if code != analysisPolicyExitCode {
				t.Fatalf("exit code = %d, want %d; stderr=%s", code, analysisPolicyExitCode, errOut.String())
			}
			if !strings.Contains(errOut.String(), "Policy: FAIL") {
				t.Fatalf("stderr missing policy result: %q", errOut.String())
			}
			if format == "json" {
				var report analysis.AnalysisReport
				if err := json.Unmarshal(out.Bytes(), &report); err != nil {
					t.Fatalf("policy-failed stdout is not valid JSON: %v\n%s", err, out.String())
				}
			} else if !strings.Contains(out.String(), "Query analysis") {
				t.Fatalf("table report incomplete: %s", out.String())
			}
		})
	}
}

func TestAnalyzeQueries_NotifyZeroOneAndBothDestinations(t *testing.T) {
	src := criticalAnalysisSource()
	tests := []struct {
		name         string
		destinations int
		wantExit     int
	}{
		{name: "zero", destinations: 0, wantExit: 1},
		{name: "one", destinations: 1, wantExit: 0},
		{name: "both", destinations: 2, wantExit: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := defaultAnalyzeOptions()
			opts.Notify = true
			calls := 0
			factory := func(*config.Config) ([]analysisNotificationDestination, error) {
				var destinations []analysisNotificationDestination
				for i := 0; i < tt.destinations; i++ {
					destinations = append(destinations, analysisNotificationDestination{
						name: fmt.Sprintf("destination_%d", i),
						send: func(context.Context, analysis.NotificationSummary) error {
							calls++
							return nil
						},
					})
				}
				return destinations, nil
			}
			var out, errOut bytes.Buffer
			code := analyzeQueriesWithSourceAndDestinations(src, testAnalyzeConfig(), "", opts, "json", "", &out, &errOut, factory)
			if code != tt.wantExit {
				t.Fatalf("exit = %d, want %d; stderr=%s", code, tt.wantExit, errOut.String())
			}
			if calls != tt.destinations {
				t.Fatalf("delivery calls = %d, want %d", calls, tt.destinations)
			}
			var report analysis.AnalysisReport
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatalf("stdout is not valid JSON: %v", err)
			}
		})
	}
}

func TestAnalysisNotificationDestinations_ConfiguredInventory(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*config.Config)
		wantNames string
	}{
		{name: "zero", configure: func(*config.Config) {}, wantNames: ""},
		{name: "webhook", configure: func(cfg *config.Config) {
			cfg.Webhook = config.WebhookConfig{Enabled: true, URL: "https://hooks.example/path", TimeoutS: 1, Events: []string{webhook.EventAnalysisFindings}}
		}, wantNames: "webhook"},
		{name: "filtered webhook", configure: func(cfg *config.Config) {
			cfg.Webhook = config.WebhookConfig{Enabled: true, URL: "https://hooks.example/path", TimeoutS: 1, Events: []string{webhook.EventStartup}}
		}, wantNames: ""},
		{name: "datadog", configure: func(cfg *config.Config) {
			cfg.DatadogEvents = testAnalyzeDatadogConfig()
		}, wantNames: "datadog_events"},
		{name: "both", configure: func(cfg *config.Config) {
			cfg.Webhook = config.WebhookConfig{Enabled: true, URL: "https://hooks.example/path", TimeoutS: 1}
			cfg.DatadogEvents = testAnalyzeDatadogConfig()
		}, wantNames: "webhook,datadog_events"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testAnalyzeConfig()
			tt.configure(cfg)
			destinations, err := analysisNotificationDestinations(cfg)
			if err != nil {
				t.Fatal(err)
			}
			names := make([]string, len(destinations))
			for i, destination := range destinations {
				names[i] = destination.name
			}
			if got := strings.Join(names, ","); got != tt.wantNames {
				t.Fatalf("destinations = %q, want %q", got, tt.wantNames)
			}
		})
	}
}

func testAnalyzeDatadogConfig() config.DatadogEventsConfig {
	return config.DatadogEventsConfig{
		Enabled: true, Site: "datadoghq.com", APIKey: "api-key", ApplicationKey: "app-key",
		TimeoutS: 1, Environment: "test", Service: "click-dog",
	}
}

func TestAnalyzeQueries_NotifyNoEligibleFindingsSkipsNetwork(t *testing.T) {
	opts := defaultAnalyzeOptions()
	opts.Notify = true
	calls := 0
	factory := func(*config.Config) ([]analysisNotificationDestination, error) {
		return []analysisNotificationDestination{{
			name: "webhook",
			send: func(context.Context, analysis.NotificationSummary) error {
				calls++
				return nil
			},
		}}, nil
	}
	var out, errOut bytes.Buffer
	code := analyzeQueriesWithSourceAndDestinations(&fakeAnalysisSource{normalized: true}, testAnalyzeConfig(), "", opts, "json", "", &out, &errOut, factory)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, errOut.String())
	}
	if calls != 0 || !strings.Contains(errOut.String(), "skipped (no eligible findings)") {
		t.Fatalf("calls=%d stderr=%q", calls, errOut.String())
	}
}

func TestAnalyzeQueries_NotificationFailurePrecedesPolicyAndAttemptsAll(t *testing.T) {
	opts := defaultAnalyzeOptions()
	opts.Notify = true
	opts.FailOn = failOnCritical
	var calls []string
	factory := func(*config.Config) ([]analysisNotificationDestination, error) {
		return []analysisNotificationDestination{
			{name: "webhook", send: func(context.Context, analysis.NotificationSummary) error {
				calls = append(calls, "webhook")
				return errors.New("rejected")
			}},
			{name: "datadog_events", send: func(context.Context, analysis.NotificationSummary) error {
				calls = append(calls, "datadog_events")
				return nil
			}},
		}, nil
	}
	var out, errOut bytes.Buffer
	code := analyzeQueriesWithSourceAndDestinations(criticalAnalysisSource(), testAnalyzeConfig(), "", opts, "json", "", &out, &errOut, factory)
	if code != 1 {
		t.Fatalf("exit = %d, want operational 1; stderr=%s", code, errOut.String())
	}
	if strings.Join(calls, ",") != "webhook,datadog_events" {
		t.Fatalf("delivery calls = %v, want both in order", calls)
	}
	if strings.Contains(errOut.String(), "Policy: FAIL") {
		t.Fatalf("policy result should not replace operational failure: %s", errOut.String())
	}
	var report analysis.AnalysisReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("completed report was destroyed: %v\n%s", err, out.String())
	}
}

func TestDeliverAnalysisNotifications_IsSynchronous(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	returned := make(chan struct{})
	destination := analysisNotificationDestination{
		name: "webhook",
		send: func(context.Context, analysis.NotificationSummary) error {
			close(started)
			<-release
			return nil
		},
	}
	go func() {
		deliverAnalysisNotifications(context.Background(), analysis.NotificationSummary{EligibleFindingCount: 1}, []analysisNotificationDestination{destination}, io.Discard)
		close(returned)
	}()
	<-started
	select {
	case <-returned:
		t.Fatal("delivery returned before destination completed")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("delivery did not return after destination completed")
	}
}

func criticalAnalysisSource() *fakeAnalysisSource {
	families := make([]model.QueryFamilyRollup, 0, 7)
	for i := 0; i < 6; i++ {
		family := testRegressionFamily(uint64(100+i), 100, 100, 0, 100, 120, nil)
		family.FamilyID = fmt.Sprintf("qf_peer_%d", i)
		families = append(families, family)
	}
	outlier := testRegressionFamily(999, 100, 100, 0, 3000, 3500, nil)
	outlier.FamilyID = "qf_critical"
	families = append(families, outlier)
	return &fakeAnalysisSource{normalized: true, families: families}
}

func TestAnalyzeQueries_BaselineFlagsAreMutuallyExclusive(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runAnalyzeQueries([]string{"-baseline", "old.json", "-save-baseline", "new.json"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "mutually exclusive") {
		t.Fatalf("stderr = %q, want mutual-exclusion error", errOut.String())
	}
}

func TestAnalyzeQueries_BaselineArtifactsCannotAliasReportOutput(t *testing.T) {
	for _, artifactFlag := range []string{"-baseline", "-save-baseline"} {
		t.Run(artifactFlag, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "artifact.json")
			var out, errOut bytes.Buffer
			code := runAnalyzeQueries([]string{artifactFlag, path, "-output", path}, &out, &errOut)
			if code != 2 {
				t.Fatalf("exit code = %d, want 2", code)
			}
			if !strings.Contains(errOut.String(), "must refer to different files") {
				t.Fatalf("stderr = %q", errOut.String())
			}
		})
	}
}

func TestAnalyzeQueries_JSONOutputIsValid(t *testing.T) {
	src := &fakeAnalysisSource{normalized: true}
	var out, errOut bytes.Buffer

	code := analyzeQueriesWithSource(src, testAnalyzeConfig(), "/etc/click-dog/click-dog.yaml", defaultAnalyzeOptions(), "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (findings are not failures); stderr:\n%s", code, errOut.String())
	}

	var report analysis.AnalysisReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out.String())
	}
	if report.SchemaVersion != analysis.ReportSchemaVersion {
		t.Errorf("schema_version = %q, want %q", report.SchemaVersion, analysis.ReportSchemaVersion)
	}
	// Normalized supported but zero rollups → info coverage finding.
	if len(report.Findings) == 0 {
		t.Error("expected at least a coverage finding for an empty window")
	}
}

func TestAnalyzeQueries_OutputPathKeepsStdoutQuiet(t *testing.T) {
	src := &fakeAnalysisSource{normalized: true}
	var out, errOut bytes.Buffer
	path := filepath.Join(t.TempDir(), "report.txt")

	code := analyzeQueriesWithSource(src, testAnalyzeConfig(), "", defaultAnalyzeOptions(), "table", path, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout should be quiet when -output is set, got:\n%s", out.String())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("report file not written: %v", err)
	}
	if !strings.Contains(string(data), "Query analysis") {
		t.Errorf("report file missing table header:\n%s", data)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("report file perms = %o, want 600", perm)
	}
}

// TestAnalyzeQueries_OutputReassertsModeOnExistingFile guards the case the
// plain-create test cannot reach: a report written into a path that already
// exists must be brought to 0600, not left on the mode it inherits. Reports
// carry normalized query previews and user/client dimension values, so an
// operator re-running analyze into a fixed path a deploy step created 0644
// would otherwise publish them world-readable with no signal.
func TestAnalyzeQueries_OutputReassertsModeOnExistingFile(t *testing.T) {
	src := &fakeAnalysisSource{normalized: true}
	path := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := analyzeQueriesWithSource(src, testAnalyzeConfig(), "", defaultAnalyzeOptions(), "json", path, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("report perms after overwriting an existing file = %o, want 600", perm)
	}
}

// TestAnalyzeQueries_OutputRefusesDirectory proves the directory guard is
// actually wired into the user-facing path: an --output naming an existing
// directory must fail without destroying it, empty directories included.
func TestAnalyzeQueries_OutputRefusesDirectory(t *testing.T) {
	src := &fakeAnalysisSource{normalized: true}
	path := filepath.Join(t.TempDir(), "reports")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := analyzeQueriesWithSource(src, testAnalyzeConfig(), "", defaultAnalyzeOptions(), "json", path, &out, &errOut)
	if code == 0 {
		t.Fatal("exit code = 0; writing a report over a directory should fail")
	}
	if !strings.Contains(errOut.String(), "is a directory") {
		t.Errorf("stderr should name the directory conflict, got:\n%s", errOut.String())
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("output directory was removed: %v", err)
	}
	if !info.IsDir() {
		t.Error("output directory was replaced by a file")
	}
}

func TestAnalyzeQueries_JSONToFile(t *testing.T) {
	src := &fakeAnalysisSource{normalized: true}
	var out, errOut bytes.Buffer
	path := filepath.Join(t.TempDir(), "report.json")

	code := analyzeQueriesWithSource(src, testAnalyzeConfig(), "", defaultAnalyzeOptions(), "json", path, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout should be quiet when -output is set, got:\n%s", out.String())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("report file not written: %v", err)
	}
	var report analysis.AnalysisReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("file is not valid JSON: %v\n%s", err, data)
	}
}

func TestAnalyzeQueries_SaveBaselineProducesV1ReportAndPrivateArtifact(t *testing.T) {
	family := testRegressionFamily(100, 100, 100, 0, 300, 500, nil)
	src := &fakeAnalysisSource{normalized: true, families: []model.QueryFamilyRollup{family}}
	opts := defaultAnalyzeOptions()
	opts.SaveBaselinePath = filepath.Join(t.TempDir(), "baseline.json")
	var out, errOut bytes.Buffer

	code := analyzeQueriesWithSource(src, testAnalyzeConfig(), "", opts, "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}
	var report analysis.AnalysisReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("invalid report JSON: %v", err)
	}
	if report.SchemaVersion != analysis.ReportSchemaVersion || report.Comparison != nil {
		t.Fatalf("save-only report changed ordinary v1 shape: %+v", report)
	}
	baseline, err := analysis.LoadBaseline(opts.SaveBaselinePath)
	if err != nil {
		t.Fatalf("saved baseline did not load: %v", err)
	}
	if baseline.SchemaVersion != analysis.BaselineSchemaVersion || !strings.Contains(errOut.String(), baseline.BaselineID) {
		t.Fatalf("baseline/status mismatch: %+v, stderr=%q", baseline, errOut.String())
	}
	info, err := os.Stat(opts.SaveBaselinePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("baseline permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestAnalyzeQueries_SaveBaselineRequiresSupportedNonEmptyExactGroups(t *testing.T) {
	tests := []struct {
		name string
		src  *fakeAnalysisSource
		want string
	}{
		{
			name: "unsupported rollups",
			src:  &fakeAnalysisSource{familiesErr: clickhouse.ErrQueryFamilyRollupsUnsupported},
			want: "normalized query-family rollups are required",
		},
		{
			name: "empty window",
			src:  &fakeAnalysisSource{normalized: true},
			want: "no exact query groups",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := defaultAnalyzeOptions()
			opts.SaveBaselinePath = filepath.Join(t.TempDir(), "baseline.json")
			var out, errOut bytes.Buffer
			code := analyzeQueriesWithSource(tt.src, testAnalyzeConfig(), "", opts, "json", "", &out, &errOut)
			if code != 1 {
				t.Fatalf("exit code = %d, want 1", code)
			}
			if !strings.Contains(errOut.String(), tt.want) {
				t.Fatalf("stderr = %q, want %q", errOut.String(), tt.want)
			}
			if _, err := os.Stat(opts.SaveBaselinePath); !os.IsNotExist(err) {
				t.Fatalf("unexpected baseline artifact after failed capture: %v", err)
			}
		})
	}
}

func TestAnalyzeQueries_BaselineComparisonProducesV2JSONAndTable(t *testing.T) {
	cfg := testAnalyzeConfig()
	opts := defaultAnalyzeOptions()
	coverage := analysis.CoverageSummary{NormalizedQuerySupported: true, QueryFamilyRollupsSupported: true}
	compatibility := buildBaselineCompatibility(cfg, opts, coverage)
	baselineWindow := analysis.AnalysisWindow{
		Start: time.Now().UTC().Add(-4 * time.Hour).Truncate(time.Second),
		End:   time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second),
	}
	baselineFamily := testRegressionFamily(100, 100, 100, 0, 300, 500, nil)
	snapshot, err := analysis.BuildBaseline(analysis.AnalysisInput{
		Window:   baselineWindow,
		Coverage: coverage,
		Families: []model.QueryFamilyRollup{baselineFamily},
	}, baselineWindow.End, compatibility)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := analysis.WriteBaselineAtomic(path, snapshot); err != nil {
		t.Fatal(err)
	}
	baselineBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	current := testRegressionFamily(100, 100, 100, 0, 1000, 1500, nil)
	src := &fakeAnalysisSource{normalized: true, families: []model.QueryFamilyRollup{current}}
	opts.BaselinePath = path

	t.Run("json", func(t *testing.T) {
		var out, errOut bytes.Buffer
		code := analyzeQueriesWithSource(src, cfg, "", opts, "json", "", &out, &errOut)
		if code != 0 {
			t.Fatalf("exit code = %d; stderr:\n%s", code, errOut.String())
		}
		var report analysis.AnalysisReport
		if err := json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatalf("invalid JSON: %v\n%s", err, out.String())
		}
		if report.SchemaVersion != analysis.ComparisonReportSchemaVersion || report.Comparison == nil {
			t.Fatalf("report = %+v, want v2 comparison", report)
		}
		if report.Comparison.Status != analysis.ComparisonCompatible {
			t.Fatalf("comparison = %+v", report.Comparison)
		}
		if !containsAnalyzerFinding(report.Findings, "latency_regression") {
			t.Fatalf("report findings missing latency regression: %+v", report.Findings)
		}
	})

	t.Run("table", func(t *testing.T) {
		var out, errOut bytes.Buffer
		code := analyzeQueriesWithSource(src, cfg, "", opts, "table", "", &out, &errOut)
		if code != 0 {
			t.Fatalf("exit code = %d; stderr:\n%s", code, errOut.String())
		}
		for _, want := range []string{"Comparison:", "status: compatible", "Regressions:", "p95 latency"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("table missing %q:\n%s", want, out.String())
			}
		}
	})
	baselineAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(baselineBefore, baselineAfter) {
		t.Fatal("baseline comparison mutated the baseline artifact")
	}
}

func TestAnalyzeQueries_RequestedMalformedBaselineIsOperationalFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":"analysis.baseline.v1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := defaultAnalyzeOptions()
	opts.BaselinePath = path
	var out, errOut bytes.Buffer
	code := analyzeQueriesWithSource(&fakeAnalysisSource{normalized: true}, testAnalyzeConfig(), "", opts, "json", "", &out, &errOut)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if out.Len() != 0 || !strings.Contains(errOut.String(), "load baseline") {
		t.Fatalf("out=%q stderr=%q", out.String(), errOut.String())
	}
}

func testRegressionFamily(hash, executions, successful, failed uint64, p95, p99 float64, exceptions []model.QueryExceptionCount) model.QueryFamilyRollup {
	return model.QueryFamilyRollup{
		FamilyID:            "qf_regression_test",
		RepresentativeQuery: "SELECT id FROM app.events WHERE tenant = ?",
		MemberHashesSorted:  []uint64{hash},
		Members: []model.QueryFamilyMember{{
			NormalizedQueryHash: hash,
			NormalizedQuery:     "SELECT id FROM app.events WHERE tenant = ?",
			ExecutionCount:      executions,
			SuccessfulCount:     successful,
			FailedCount:         failed,
			P95DurationMs:       p95,
			P99DurationMs:       p99,
			TopExceptions:       exceptions,
		}},
		Stats: model.QueryFamilyStats{
			ExecutionCount:  executions,
			SuccessfulCount: successful,
			FailedCount:     failed,
			P95DurationMs:   p95,
			P99DurationMs:   p99,
			TopExceptions:   exceptions,
		},
	}
}

func containsAnalyzerFinding(findings []analysis.Finding, analyzer string) bool {
	for _, finding := range findings {
		if finding.Analyzer == analyzer {
			return true
		}
	}
	return false
}

func TestAnalyzeQueries_RollupsUnsupportedStillSucceeds(t *testing.T) {
	src := &fakeAnalysisSource{
		normalized:  false,
		familiesErr: clickhouse.ErrQueryFamilyRollupsUnsupported,
	}
	var out, errOut bytes.Buffer

	code := analyzeQueriesWithSource(src, testAnalyzeConfig(), "", defaultAnalyzeOptions(), "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("unsupported rollups must not fail the command; got code %d, stderr:\n%s", code, errOut.String())
	}
	var report analysis.AnalysisReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if report.Coverage.QueryFamilyRollupsSupported {
		t.Error("coverage should record rollups as unsupported")
	}
}

func TestAnalyzeQueries_RequiredQueryErrorFailsCommand(t *testing.T) {
	src := &fakeAnalysisSource{
		normalized:  true,
		familiesErr: errors.New("connection reset"),
	}
	var out, errOut bytes.Buffer

	code := analyzeQueriesWithSource(src, testAnalyzeConfig(), "", defaultAnalyzeOptions(), "table", "", &out, &errOut)
	if code != 1 {
		t.Fatalf("a required query failure should exit 1, got %d", code)
	}
	if !strings.Contains(errOut.String(), "connection reset") {
		t.Errorf("stderr should surface the query error:\n%s", errOut.String())
	}
}

func TestAnalyzeQueries_RedactDimensionsOmitsRawValues(t *testing.T) {
	families := []model.QueryFamilyRollup{
		{
			FamilyID:            "qf_0000000000000001",
			RepresentativeQuery: "SELECT 1",
			MemberHashesSorted:  []uint64{100},
			Stats:               model.QueryFamilyStats{ExecutionCount: 100},
		},
	}
	dimCounts := []clickhouse.QueryFamilyDimensionCount{
		{NormalizedQueryHash: 100, Dimension: clickhouse.QueryFamilyDimensionUser, Value: "secret_etl_user", ExecutionCount: 95},
		{NormalizedQueryHash: 100, Dimension: clickhouse.QueryFamilyDimensionUser, Value: "other_user", ExecutionCount: 5},
	}
	src := &fakeAnalysisSource{normalized: true, families: families, dimensionCounts: dimCounts}

	opts := defaultAnalyzeOptions()
	opts.RedactDimensions = true

	var out, errOut bytes.Buffer
	code := analyzeQueriesWithSource(src, testAnalyzeConfig(), "", opts, "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}

	output := out.String()
	if strings.Contains(output, "secret_etl_user") || strings.Contains(output, "other_user") {
		t.Errorf("redacted report leaked raw dimension values:\n%s", output)
	}
	if !strings.Contains(output, "user_1") {
		t.Errorf("redacted report should use stable labels like user_1:\n%s", output)
	}
	// Counts must survive redaction (the skew finding reports the dominant share).
	if !strings.Contains(output, `"value_execution_count": 95`) {
		t.Errorf("redaction should preserve counts:\n%s", output)
	}
}

func TestAnalyzeQueries_NotRedactedKeepsRawValues(t *testing.T) {
	families := []model.QueryFamilyRollup{
		{
			FamilyID:            "qf_0000000000000001",
			RepresentativeQuery: "SELECT 1",
			MemberHashesSorted:  []uint64{100},
			Stats:               model.QueryFamilyStats{ExecutionCount: 100},
		},
	}
	dimCounts := []clickhouse.QueryFamilyDimensionCount{
		{NormalizedQueryHash: 100, Dimension: clickhouse.QueryFamilyDimensionUser, Value: "etl_user", ExecutionCount: 95},
	}
	src := &fakeAnalysisSource{normalized: true, families: families, dimensionCounts: dimCounts}

	var out, errOut bytes.Buffer
	code := analyzeQueriesWithSource(src, testAnalyzeConfig(), "", defaultAnalyzeOptions(), "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "etl_user") {
		t.Errorf("default (non-redacted) report should keep raw dimension values:\n%s", out.String())
	}
}

func TestResolveAnalysisSpanSampleLimit(t *testing.T) {
	tests := []struct {
		name             string
		flagLimit        int
		maxSpansPerCycle int
		want             int
	}{
		{"auto default", 0, 0, 1000},
		{"capped by max spans per cycle", 0, 250, 250},
		{"max above auto keeps auto", 0, 5000, 1000},
		{"explicit flag overrides", 200, 50, 200},
		{"explicit flag above cap", 9000, 250, 9000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveAnalysisSpanSampleLimit(tt.flagLimit, tt.maxSpansPerCycle); got != tt.want {
				t.Errorf("resolveAnalysisSpanSampleLimit(%d, %d) = %d, want %d", tt.flagLimit, tt.maxSpansPerCycle, got, tt.want)
			}
		})
	}
}

func TestAnalysisQueryLogLookbackDays(t *testing.T) {
	tests := []struct {
		lookback time.Duration
		want     int
	}{
		{time.Hour, 1},
		{24 * time.Hour, 1},
		{25 * time.Hour, 2},
		{48 * time.Hour, 2},
		{time.Minute, 1},
	}
	for _, tt := range tests {
		if got := analysisQueryLogLookbackDays(tt.lookback); got != tt.want {
			t.Errorf("analysisQueryLogLookbackDays(%s) = %d, want %d", tt.lookback, got, tt.want)
		}
	}
}

func TestAnalysisUserFilterFingerprintUsesEffectiveSets(t *testing.T) {
	a := analysisUserFilterFingerprint([]string{"worker", "api", "api"}, []string{"system", "backup"})
	b := analysisUserFilterFingerprint([]string{"api", "worker"}, []string{"backup", "system", "system"})
	if a != b {
		t.Fatalf("equivalent filter sets produced different fingerprints: %s != %s", a, b)
	}
	if strings.Contains(a, "api") || strings.Contains(a, "system") {
		t.Fatalf("fingerprint exposed filter values: %s", a)
	}
	c := analysisUserFilterFingerprint([]string{"different"}, []string{"backup", "system"})
	if a == c {
		t.Fatalf("meaningful filter change did not change fingerprint: %s", a)
	}
}

func TestBuildAttributionCoverage_MapsSpansToFamilies(t *testing.T) {
	families := []model.QueryFamilyRollup{
		{FamilyID: "qf_1", MemberHashesSorted: []uint64{100}},
	}
	spans := []model.OpenTelemetrySpan{
		// Two spans map to qf_1 via query_id -> normalized hash 100.
		{Attributes: map[string]string{"clickhouse.query_id": "q1", "log_comment": `{"app":"billing","query_name":"invoice"}`}},
		{Attributes: map[string]string{"clickhouse.query_id": "q2", "log_comment": `{"app":"billing"}`}},
		// No query_id — skipped.
		{Attributes: map[string]string{}},
	}
	src := &fakeAnalysisSource{
		queryLog: map[string]model.QueryLog{
			"q1": {QueryID: "q1", NormalizedQueryHash: 100},
			"q2": {QueryID: "q2", NormalizedQueryHash: 100},
		},
	}

	attr, warning := buildAttributionCoverage(context.Background(), src, spans, families, 1)
	if warning != "" {
		t.Fatalf("unexpected warning: %s", warning)
	}
	cov, ok := attr["qf_1"]
	if !ok {
		t.Fatalf("qf_1 missing from attribution: %+v", attr)
	}
	if cov.SampledSpans != 2 {
		t.Errorf("sampled spans = %d, want 2", cov.SampledSpans)
	}
	if cov.WithLogCommentApp != 2 {
		t.Errorf("with app = %d, want 2", cov.WithLogCommentApp)
	}
	if cov.WithLogCommentQueryName != 1 {
		t.Errorf("with query_name = %d, want 1", cov.WithLogCommentQueryName)
	}
	if cov.AppRatio != 1.0 {
		t.Errorf("app ratio = %v, want 1.0", cov.AppRatio)
	}
	if cov.QueryNameRatio != 0.5 {
		t.Errorf("query_name ratio = %v, want 0.5", cov.QueryNameRatio)
	}
}

func TestBuildAttributionCoverage_EnrichmentErrorWarns(t *testing.T) {
	families := []model.QueryFamilyRollup{{FamilyID: "qf_1", MemberHashesSorted: []uint64{100}}}
	spans := []model.OpenTelemetrySpan{{Attributes: map[string]string{"clickhouse.query_id": "q1"}}}
	src := &fakeAnalysisSource{queryLogErr: errors.New("ACCESS_DENIED")}

	attr, warning := buildAttributionCoverage(context.Background(), src, spans, families, 1)
	if attr != nil {
		t.Errorf("attribution should be nil on enrichment failure, got %+v", attr)
	}
	if !strings.Contains(warning, "ACCESS_DENIED") {
		t.Errorf("warning should surface the enrichment error, got %q", warning)
	}
}

func TestMainUsageListsAnalyze(t *testing.T) {
	var out bytes.Buffer
	printUsage(&out)
	if !strings.Contains(out.String(), "analyze") {
		t.Errorf("main usage should list the analyze command:\n%s", out.String())
	}
}
